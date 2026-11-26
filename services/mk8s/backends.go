package mk8s

import (
	"fmt"

	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/services/mk8s/backend/mock"
	"github.com/kfirgollan/NebiusMocker/services/mk8s/domain"
)

// newBackend builds the Kubernetes data plane NEBIUS_MOCKER_MK8S_BACKEND
// selects.
func newBackend(cfg config.Config, deps mocker.Deps) (domain.Backend, error) {
	switch cfg.MK8s.Backend {
	case config.MK8sBackendMock:
		return mock.New(deps.Clock), nil
	default:
		return nil, fmt.Errorf("mk8s: backend %q is not available", cfg.MK8s.Backend)
	}
}

// detached reports whether a backend's calls may block for long (every
// backend but the mock starts containers), so that operations run their
// work on their own goroutine.
func detached(b domain.Backend) bool { return b.Name() != mock.Name }
