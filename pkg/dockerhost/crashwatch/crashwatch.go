// Package crashwatch fails a Docker-backed test when one of its containers
// crashed. The managed servers run with a restart policy, so a crash is
// otherwise invisible: the daemon brings the server back and the test may
// still pass. A Watch records the daemon's events of the test's containers
// (by label) from Start on; Check fails the test for every container that
// died on its own with a nonzero exit code (no kill from a stop or remove
// before it), was OOM-killed, or was restarted by the daemon.
package crashwatch

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kfirgollan/NebiusMocker/pkg/dockerhost"
)

// logTail is how many log lines a crash report carries.
const logTail = 40

// Watch records container events; see the package doc.
type Watch struct {
	docker *dockerhost.Client
	labels map[string]string
	since  time.Time // daemon time at Start

	mu        sync.Mutex
	events    []dockerhost.Event
	seen      map[eventKey]bool
	reported  map[string]bool // crash reports already made, by container ID + time
	streamErr error
}

type eventKey struct {
	id, action string
	at         int64
}

// Start watches the containers carrying every given label until the test
// ends; its cleanup runs a final Check.
func Start(tb testing.TB, docker *dockerhost.Client, labels map[string]string) *Watch {
	tb.Helper()
	info, err := docker.Info(tb.Context())
	if err != nil {
		tb.Fatalf("crashwatch: docker info: %v", err)
	}
	w := &Watch{
		docker: docker, labels: labels, since: info.SystemTime,
		seen: map[eventKey]bool{}, reported: map[string]bool{},
	}
	ctx, cancel := context.WithCancel(context.WithoutCancel(tb.Context()))
	var wg sync.WaitGroup
	wg.Go(func() {
		err := docker.ContainerEvents(ctx, labels, w.since, time.Time{}, w.add)
		w.mu.Lock()
		defer w.mu.Unlock()
		w.streamErr = err
	})
	tb.Cleanup(func() {
		w.Check(context.WithoutCancel(ctx), tb)
		cancel()
		wg.Wait()
	})
	return w
}

func (w *Watch) add(e dockerhost.Event) {
	w.mu.Lock()
	defer w.mu.Unlock()
	k := eventKey{id: e.ID, action: e.Action, at: e.TimeNano}
	if w.seen[k] {
		return
	}
	w.seen[k] = true
	w.events = append(w.events, e)
}

// Check fails the test (without stopping it) for every crash so far that it
// has not reported yet, and for every watched container that the daemon
// restarted or that was OOM-killed.
func (w *Watch) Check(ctx context.Context, tb testing.TB) {
	tb.Helper()
	w.catchUp(ctx, tb)
	w.mu.Lock()
	if w.streamErr != nil {
		tb.Errorf("crashwatch: the event stream ended: %v", w.streamErr)
		w.streamErr = nil
	}
	found := crashes(w.events)
	var fresh []crash
	for _, c := range found {
		if key := fmt.Sprint(c.id, c.at); !w.reported[key] {
			w.reported[key] = true
			fresh = append(fresh, c)
		}
	}
	w.mu.Unlock()
	for _, c := range fresh {
		tb.Errorf("crashwatch: container %s (%.12s) %s at %s%s", c.name, c.id, c.why,
			time.Unix(0, c.at).UTC().Format(time.RFC3339Nano), w.logs(ctx, c.id))
	}
	cs, err := w.docker.ListContainers(ctx, w.labels)
	if err != nil {
		tb.Errorf("crashwatch: list containers: %v", err)
		return
	}
	for _, c := range cs {
		ctr, err := w.docker.InspectContainer(ctx, c.ID)
		if err != nil {
			continue // removed meanwhile
		}
		if ctr.RestartCount != 0 || ctr.OOMKilled {
			tb.Errorf("crashwatch: container %s: RestartCount %d, OOMKilled %t, ExitCode %d; want no restart%s",
				ctr.Name, ctr.RestartCount, ctr.OOMKilled, ctr.ExitCode, w.logs(ctx, ctr.ID))
		}
	}
}

// catchUp fetches the events up to the daemon's now that the stream may not
// have delivered yet, replayed from the last one it did.
func (w *Watch) catchUp(ctx context.Context, tb testing.TB) {
	tb.Helper()
	info, err := w.docker.Info(ctx)
	if err != nil {
		tb.Errorf("crashwatch: docker info: %v", err)
		return
	}
	w.mu.Lock()
	from := w.since
	if n := len(w.events); n > 0 {
		from = time.Unix(0, w.events[n-1].TimeNano)
	}
	w.mu.Unlock()
	if !info.SystemTime.After(from) {
		return
	}
	if err := w.docker.ContainerEvents(ctx, w.labels, from, info.SystemTime, w.add); err != nil {
		tb.Errorf("crashwatch: catch up on events: %v", err)
	}
}

func (w *Watch) logs(ctx context.Context, id string) string {
	logs, err := w.docker.Logs(ctx, id, logTail)
	if err != nil {
		return "" // removed: the events are the evidence
	}
	if logs = strings.TrimSpace(logs); logs == "" {
		return "; no log output"
	}
	return "; last log lines:\n" + logs
}

type crash struct {
	id, name, why string
	at            int64
}

// crashes are the dies that no kill (docker stop, docker rm -f) preceded
// since the container last started, except a clean exit of a one-shot job.
func crashes(events []dockerhost.Event) []crash {
	evs := slices.Clone(events)
	slices.SortStableFunc(evs, func(a, b dockerhost.Event) int { return cmp.Compare(a.TimeNano, b.TimeNano) })
	killed, oom := map[string]bool{}, map[string]bool{}
	var out []crash
	for _, e := range evs {
		switch e.Action {
		case "start":
			killed[e.ID], oom[e.ID] = false, false
		case "kill":
			killed[e.ID] = true
		case "oom":
			oom[e.ID] = true
		case "die":
			code := e.Attributes["exitCode"]
			switch {
			case oom[e.ID]:
				out = append(out, crash{id: e.ID, name: e.Attributes["name"], at: e.TimeNano, why: "was OOM-killed (exit code " + code + ")"})
			case !killed[e.ID] && code != "0":
				out = append(out, crash{id: e.ID, name: e.Attributes["name"], at: e.TimeNano, why: "exited on its own with code " + code})
			}
			killed[e.ID], oom[e.ID] = false, false
		}
	}
	return out
}
