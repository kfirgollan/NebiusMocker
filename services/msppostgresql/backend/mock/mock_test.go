package mock_test

import (
	"testing"
	"time"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/services/msppostgresql/backend/mock"
	"github.com/kfirgollan/NebiusMocker/services/msppostgresql/domain"
)

func TestEndpoints(t *testing.T) {
	t.Parallel()
	b := mock.New()
	ep, err := b.EnsureCluster(t.Context(), domain.BackendCluster{ID: "postgresql-1"})
	if err != nil {
		t.Fatalf("EnsureCluster: %v", err)
	}
	if ep.PrivateReadWrite != "postgresql-1.rw.postgresql.mocker.localhost:5432" || ep.PrivateReadOnly != "postgresql-1.ro.postgresql.mocker.localhost:5432" {
		t.Errorf("private endpoints = %+v", ep)
	}
	if ep.PublicReadWrite != "" || ep.PublicReadOnly != "" {
		t.Errorf("public endpoints without public access: %+v", ep)
	}
	ep, _ = b.EnsureCluster(t.Context(), domain.BackendCluster{ID: "postgresql-1", PublicAccess: true})
	if ep.PublicReadWrite != "postgresql-1.public-rw.postgresql.mocker.localhost:5432" || ep.PublicReadOnly != "postgresql-1.public-ro.postgresql.mocker.localhost:5432" {
		t.Errorf("public endpoints = %+v", ep)
	}
}

func TestBackupsNeedARunningCluster(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	b := mock.New()
	if b.Name() != mock.Name {
		t.Errorf("Name = %q", b.Name())
	}
	if _, err := b.EnsureCluster(ctx, domain.BackendCluster{ID: "postgresql-1"}); err != nil {
		t.Fatalf("EnsureCluster: %v", err)
	}
	if info, err := b.CreateBackup(ctx, "postgresql-1", "b-1"); err != nil || info.SizeBytes != 0 {
		t.Errorf("CreateBackup = %+v, %v", info, err)
	}
	if err := b.StopCluster(ctx, "postgresql-1"); err != nil {
		t.Fatalf("StopCluster: %v", err)
	}
	if _, err := b.CreateBackup(ctx, "postgresql-1", "b-2"); !errs.IsKind(err, errs.KindBadState) {
		t.Errorf("CreateBackup of a stopped cluster: %v, want BadState", err)
	}
	if _, err := b.EnsureCluster(ctx, domain.BackendCluster{ID: "postgresql-1"}); err != nil {
		t.Fatalf("EnsureCluster (start): %v", err)
	}
	if _, err := b.CreateBackup(ctx, "postgresql-1", "b-3"); err != nil {
		t.Errorf("CreateBackup after start: %v", err)
	}
	// Unknown clusters and backups are not errors (idempotent calls).
	if err := b.StopCluster(ctx, "postgresql-9"); err != nil {
		t.Errorf("StopCluster of an unknown cluster: %v", err)
	}
	if err := b.DeleteBackup(ctx, "postgresql-1", "nope", time.Time{}); err != nil {
		t.Errorf("DeleteBackup of an unknown backup: %v", err)
	}
	if err := b.DeleteCluster(ctx, "postgresql-1"); err != nil {
		t.Errorf("DeleteCluster: %v", err)
	}
	if err := b.DeleteCluster(ctx, "postgresql-1"); err != nil {
		t.Errorf("second DeleteCluster: %v", err)
	}
	if err := b.Close(ctx); err != nil {
		t.Errorf("Close: %v", err)
	}
}
