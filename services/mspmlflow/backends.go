package mspmlflow

import (
	"context"
	"fmt"

	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/dockerhost"
	"github.com/kfirgollan/NebiusMocker/pkg/localca"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/services/mspmlflow/backend/docker"
	"github.com/kfirgollan/NebiusMocker/services/mspmlflow/backend/mock"
	"github.com/kfirgollan/NebiusMocker/services/mspmlflow/domain"
)

// newBackend builds the tracking server data plane
// NEBIUS_MOCKER_MSP_MLFLOW_BACKEND selects.
func newBackend(ctx context.Context, cfg config.Config, deps mocker.Deps) (domain.Backend, error) {
	switch cfg.MSP.MLflowBackend {
	case config.MSPBackendMock:
		return mock.New(), nil
	case config.MSPBackendDocker:
		client, err := dockerhost.New(cfg.MSP.DockerHost)
		if err != nil {
			return nil, fmt.Errorf("msp-mlflow: NEBIUS_MOCKER_MSP_DOCKER_HOST: %w", err)
		}
		ca, err := localca.LoadOrCreate(cfg.MSP.TLSDir, deps.Clock)
		if err != nil {
			return nil, fmt.Errorf("msp-mlflow: NEBIUS_MOCKER_MSP_TLS_DIR: %w", err)
		}
		return docker.New(ctx, docker.Options{
			Config: cfg.MSP, EdgeListen: cfg.Listen, S3Listen: cfg.Storage.S3Listen,
			Docker: client, Clock: deps.Clock, Log: deps.Log, CA: ca,
		})
	default:
		return nil, fmt.Errorf("msp-mlflow: backend %q is not available", cfg.MSP.MLflowBackend)
	}
}
