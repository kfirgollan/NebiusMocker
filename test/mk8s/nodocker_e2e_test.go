//go:build e2e_mk8s

package mk8s

import (
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/services/compute"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/services/mk8s"
	"github.com/kfirgollan/NebiusMocker/services/vpc"
)

// TestK3sWithoutDocker: with NEBIUS_MOCKER_MK8S_BACKEND=k3s and no Docker
// daemon the mocker refuses to start and names the settings, instead of
// serving clusters that never get a Kubernetes API.
func TestK3sWithoutDocker(t *testing.T) {
	cfg, err := config.FromLookup(func(k string) (string, bool) {
		v, ok := map[string]string{
			"NEBIUS_MOCKER_MK8S_BACKEND":         "k3s",
			"NEBIUS_MOCKER_LISTEN":               "127.0.0.1:4566",
			"NEBIUS_MOCKER_MK8S_K3S_DOCKER_HOST": "unix://" + filepath.Join(t.TempDir(), "no-docker.sock"),
		}[k]
		return v, ok
	})
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	srv, err := mocker.New(cfg, slog.New(slog.DiscardHandler), []mocker.Module{iam.New(), vpc.New(), compute.New(), mk8s.New()})
	if err != nil {
		t.Fatalf("mocker.New: %v", err)
	}
	if _, err = srv.Start(t.Context()); err == nil {
		_ = srv.Stop(t.Context())
		t.Fatal("the mocker started with the k3s backend and no Docker")
	}
	for _, want := range []string{"NEBIUS_MOCKER_MK8S_BACKEND=k3s", "NEBIUS_MOCKER_MK8S_K3S_DOCKER_HOST", "no-docker.sock"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %s", err, want)
		}
	}
}
