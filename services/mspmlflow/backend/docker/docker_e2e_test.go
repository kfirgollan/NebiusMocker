//go:build e2e_msp

package docker_test

import (
	"context"
	"crypto/tls"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/dockerhost"
	"github.com/kfirgollan/NebiusMocker/pkg/localca"
	mlflowdocker "github.com/kfirgollan/NebiusMocker/services/mspmlflow/backend/docker"
	"github.com/kfirgollan/NebiusMocker/services/mspmlflow/domain"
)

// TestE2EMLflowLifecycle runs a real MLflow tracking server. The artifact
// store is bogus: the S3 artifact path is proven with the real S3 front in
// test/msp. Here: TLS with the mocker CA, basic auth, the experiments API,
// and persistence and a stable port across stop/start.
func TestE2EMLflowLifecycle(t *testing.T) {
	ctx := t.Context()
	docker, err := dockerhost.New("")
	if err != nil {
		t.Fatal(err)
	}
	clk := clock.Real{}
	dir := t.TempDir()
	ca, err := localca.LoadOrCreate(dir, clk)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.MSPConfig{
		BindIP: "127.0.0.1", EndpointHost: "127.0.0.1", TLSDir: dir, StartTimeout: 5 * time.Minute,
		MLflowImage: "ghcr.io/mlflow/mlflow:v2.22.0", MLflowVersion: "2.22.0", S3Endpoint: "http://host.docker.internal:9",
	}
	owner := liveOwner(t)
	b, err := mlflowdocker.New(ctx, mlflowdocker.Options{
		Config: cfg, EdgeListen: owner, Docker: docker, Clock: clk, CA: ca, Log: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cctx := context.WithoutCancel(ctx)
		if err := b.Close(cctx); err != nil {
			t.Errorf("Close: %v", err)
		}
		labels := map[string]string{mlflowdocker.LabelBackend: "msp-mlflow", mlflowdocker.LabelOwner: b.Owner()}
		cs, _ := docker.ListContainers(cctx, labels)
		vs, _ := docker.ListVolumes(cctx, labels)
		if len(cs)+len(vs) != 0 {
			t.Errorf("leftovers: containers %v volumes %v", cs, vs)
		}
	})

	c := domain.BackendCluster{
		ID: "mlflow-e2e", ProjectID: "project-x", Name: "ml", AdminUsername: "admin", AdminPassword: "Adm1n-password-123",
		CPUs: 2, MemoryBytes: 2 << 30,
		Artifacts: domain.ArtifactStore{Bucket: "bogus", Region: "eu-north1", AccessKeyID: "AK", Secret: "SK"},
	}
	ep, err := b.EnsureCluster(ctx, c)
	if err != nil {
		t.Fatalf("EnsureCluster: %v", err)
	}
	t.Logf("endpoint %+v", ep)

	client := &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: ca.Pool(), MinVersion: tls.VersionTLS12}, DisableKeepAlives: true,
	}}
	call := func(ep, user, password, method, path, body string) int {
		req, err := http.NewRequestWithContext(ctx, method, "https://"+ep+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.SetBasicAuth(user, password)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	if got := call(ep.Private, "admin", "wrong", http.MethodGet, "/api/2.0/mlflow/experiments/search?max_results=1", ""); got != http.StatusUnauthorized {
		t.Errorf("bad password status = %d, want 401", got)
	}
	if got := call(ep.Private, "admin", "Adm1n-password-123", http.MethodGet, "/api/2.0/mlflow/experiments/search?max_results=1", ""); got != http.StatusOK {
		t.Errorf("admin status = %d, want 200", got)
	}
	n, err := b.ExperimentsCount(ctx, c.ID)
	if err != nil || n != 1 {
		t.Fatalf("ExperimentsCount = %d, %v; want 1 (Default)", n, err)
	}
	if got := call(ep.Private, "admin", "Adm1n-password-123", http.MethodPost, "/api/2.0/mlflow/experiments/create", `{"name":"e2e-exp"}`); got != http.StatusOK {
		t.Fatalf("create experiment status = %d", got)
	}
	if n, err := b.ExperimentsCount(ctx, c.ID); err != nil || n != 2 {
		t.Errorf("ExperimentsCount after create = %d, %v; want 2", n, err)
	}

	if err := b.StopCluster(ctx, c.ID); err != nil {
		t.Fatalf("StopCluster: %v", err)
	}
	c.AdminPassword = ""
	ep2, err := b.EnsureCluster(ctx, c)
	if err != nil {
		t.Fatalf("EnsureCluster after stop: %v", err)
	}
	if ep2 != ep {
		t.Errorf("endpoint changed across stop/start: %+v -> %+v", ep, ep2)
	}
	if n, err := b.ExperimentsCount(ctx, c.ID); err != nil || n != 2 {
		t.Errorf("ExperimentsCount after restart = %d, %v; want 2 (persisted)", n, err)
	}
	if err := b.DeleteCluster(ctx, c.ID); err != nil {
		t.Fatalf("DeleteCluster: %v", err)
	}
}

// liveOwner plays the mocker's edge for the owner label: a listener that
// stays open for the test, so reapers (deploy/docker/reap.sh) see the owner
// alive and leave a concurrently running test's objects alone.
func liveOwner(t *testing.T) string {
	t.Helper()
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l.Addr().String()
}
