package mk8s

import (
	"context"
	"fmt"

	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/dockerhost"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/services/mk8s/backend/k3s"
	"github.com/kfirgollan/NebiusMocker/services/mk8s/backend/mock"
	"github.com/kfirgollan/NebiusMocker/services/mk8s/domain"
)

// newBackend builds the Kubernetes data plane NEBIUS_MOCKER_MK8S_BACKEND
// selects.
func newBackend(ctx context.Context, cfg config.Config, deps mocker.Deps) (domain.Backend, error) {
	switch cfg.MK8s.Backend {
	case config.MK8sBackendMock:
		return mock.New(deps.Clock), nil
	case config.MK8sBackendK3s:
		docker, err := dockerhost.New(cfg.MK8sK3s.DockerHost)
		if err != nil {
			return nil, fmt.Errorf("mk8s: NEBIUS_MOCKER_MK8S_K3S_DOCKER_HOST: %w", err)
		}
		return k3s.New(ctx, k3s.Options{
			Config: cfg.MK8sK3s, Versions: cfg.MK8s.Versions, EdgeListen: cfg.Listen, EdgeTLS: cfg.TLS.Enabled(),
			Docker: docker, Clock: deps.Clock, Log: deps.Log,
		})
	default:
		return nil, fmt.Errorf("mk8s: backend %q is not available", cfg.MK8s.Backend)
	}
}

// detached reports whether a backend's calls may block for long (every
// backend but the mock starts containers), so that operations run their
// work on their own goroutine.
func detached(b domain.Backend) bool { return b.Name() != mock.Name }
