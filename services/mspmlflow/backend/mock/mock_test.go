package mock_test

import (
	"testing"

	"github.com/kfirgollan/NebiusMocker/services/mspmlflow/backend/mock"
	"github.com/kfirgollan/NebiusMocker/services/mspmlflow/domain"
)

func TestMockBackend(t *testing.T) {
	t.Parallel()
	b := mock.New()
	ctx := t.Context()
	if b.Name() != mock.Name {
		t.Errorf("Name = %q", b.Name())
	}

	ep, err := b.EnsureCluster(ctx, domain.BackendCluster{ID: "mlflow-1"})
	if err != nil || ep.Private != "mlflow-1.private.mlflow.mocker.localhost" || ep.Public != "" {
		t.Fatalf("EnsureCluster = %+v, %v", ep, err)
	}
	ep, err = b.EnsureCluster(ctx, domain.BackendCluster{ID: "mlflow-2", PublicAccess: true})
	if err != nil || ep.Public != "mlflow-2.mlflow.mocker.localhost" || ep.Private == "" {
		t.Fatalf("EnsureCluster(public) = %+v, %v", ep, err)
	}
	if n, err := b.ExperimentsCount(ctx, "mlflow-1"); err != nil || n != 0 {
		t.Errorf("ExperimentsCount = %d, %v", n, err)
	}

	if err := b.StopCluster(ctx, "mlflow-1"); err != nil || !b.Stopped("mlflow-1") {
		t.Errorf("StopCluster: %v, stopped = %v", err, b.Stopped("mlflow-1"))
	}
	if _, err := b.EnsureCluster(ctx, domain.BackendCluster{ID: "mlflow-1"}); err != nil || b.Stopped("mlflow-1") {
		t.Errorf("restart: %v, stopped = %v", err, b.Stopped("mlflow-1"))
	}
	// Unknown clusters are not an error.
	if err := b.StopCluster(ctx, "mlflow-nope"); err != nil {
		t.Errorf("StopCluster(unknown): %v", err)
	}
	if err := b.DeleteCluster(ctx, "mlflow-1"); err != nil {
		t.Errorf("DeleteCluster: %v", err)
	}
	if err := b.DeleteCluster(ctx, "mlflow-1"); err != nil {
		t.Errorf("DeleteCluster(twice): %v", err)
	}
	if err := b.Close(ctx); err != nil {
		t.Errorf("Close: %v", err)
	}
}
