//go:build e2e_msp

package crashwatch_test

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/dockerhost"
	"github.com/kfirgollan/NebiusMocker/pkg/dockerhost/crashwatch"
)

const image = "postgres:16.11-alpine" // small, and the msp suites pull it anyway

// recorder keeps what the watch reports instead of failing the test.
type recorder struct {
	testing.TB
	mu   sync.Mutex
	errs []string
}

func (r *recorder) Helper() {}

func (r *recorder) Errorf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errs = append(r.errs, fmt.Sprintf(format, args...))
}

func (r *recorder) reports() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.errs, "\n")
}

// TestE2EWatchReportsOnlyCrashes runs, on the real daemon, a container that
// is stopped and removed, a one-shot job that exits 0, and one that crashes
// under a restart policy: only the last one is reported, both by its die
// event and by its RestartCount.
func TestE2EWatchReportsOnlyCrashes(t *testing.T) {
	ctx := t.Context()
	docker, err := dockerhost.New("")
	if err != nil {
		t.Fatal(err)
	}
	if err := docker.EnsureImage(ctx, image); err != nil {
		t.Fatal(err)
	}
	clk := clock.Real{}
	tag := strconv.FormatInt(clk.Now().UnixNano(), 36)
	labels := map[string]string{"nebius-mocker.crashwatch-e2e": tag}
	rec := &recorder{TB: t}
	w := crashwatch.Start(rec, docker, labels)

	run := func(name, restart string, cmd ...string) string {
		id, err := docker.CreateContainer(ctx, dockerhost.ContainerSpec{
			Name: "nmock-crashwatch-" + tag + "-" + name, Image: image, Entrypoint: cmd, Labels: labels, RestartPolicy: restart,
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = docker.RemoveContainer(context.WithoutCancel(ctx), id) })
		if err := docker.StartContainer(ctx, id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	stopped := run("stopped", "on-failure", "sleep", "3600")
	run("job", "", "true")
	crashing := run("crashing", "on-failure", "sh", "-c", "sleep 1; exit 3")

	deadline := clk.Now().Add(time.Minute)
	for {
		c, err := docker.InspectContainer(ctx, crashing)
		if err != nil {
			t.Fatal(err)
		}
		if c.RestartCount > 0 {
			break
		}
		if clk.Now().After(deadline) {
			t.Fatalf("the crashing container was not restarted: %+v", c)
		}
		<-clk.After(200 * time.Millisecond)
	}
	if err := docker.StopContainer(ctx, stopped, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := docker.RemoveContainer(ctx, stopped); err != nil {
		t.Fatal(err)
	}

	w.Check(ctx, rec)
	got := rec.reports()
	t.Logf("reports (expected: the crashing container only):\n%s", got)
	if !strings.Contains(got, "-crashing (") || !strings.Contains(got, "exited on its own with code 3") {
		t.Errorf("the crash's die event was not reported:\n%s", got)
	}
	if !strings.Contains(got, "-crashing: RestartCount ") {
		t.Errorf("the restart was not reported:\n%s", got)
	}
	if strings.Contains(got, "-stopped") || strings.Contains(got, "-job") {
		t.Errorf("an expected exit was reported:\n%s", got)
	}
}
