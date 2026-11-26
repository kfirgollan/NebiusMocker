package msppostgresql

import (
	"context"
	"fmt"

	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/dockerhost"
	"github.com/kfirgollan/NebiusMocker/pkg/localca"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/services/msppostgresql/backend/docker"
	"github.com/kfirgollan/NebiusMocker/services/msppostgresql/backend/mock"
	"github.com/kfirgollan/NebiusMocker/services/msppostgresql/domain"
)

// newBackend builds the PostgreSQL data plane
// NEBIUS_MOCKER_MSP_POSTGRESQL_BACKEND selects.
func newBackend(ctx context.Context, cfg config.Config, deps mocker.Deps) (domain.Backend, error) {
	switch cfg.MSP.PostgreSQLBackend {
	case config.MSPBackendMock:
		return mock.New(), nil
	case config.MSPBackendDocker:
		client, err := dockerhost.New(cfg.MSP.DockerHost)
		if err != nil {
			return nil, fmt.Errorf("msppostgresql: NEBIUS_MOCKER_MSP_DOCKER_HOST: %w", err)
		}
		ca, err := localca.LoadOrCreate(cfg.MSP.TLSDir, deps.Clock)
		if err != nil {
			return nil, fmt.Errorf("msppostgresql: NEBIUS_MOCKER_MSP_TLS_DIR: %w", err)
		}
		return docker.New(ctx, docker.Options{
			Config: cfg.MSP, EdgeListen: cfg.Listen, Docker: client, Clock: deps.Clock, Log: deps.Log, CA: ca,
		})
	default:
		return nil, fmt.Errorf("msppostgresql: backend %q is not available", cfg.MSP.PostgreSQLBackend)
	}
}

// detached reports whether a backend's calls may block for long (every
// backend but the mock starts containers), so that operations run their
// work on their own goroutine.
func detached(b domain.Backend) bool { return b.Name() != mock.Name }
