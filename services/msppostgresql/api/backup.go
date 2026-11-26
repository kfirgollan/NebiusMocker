package api

import (
	"context"

	commonv1alpha1pb "github.com/nebius/gosdk/proto/nebius/common/v1alpha1"
	pgpb "github.com/nebius/gosdk/proto/nebius/msp/postgresql/v1alpha1"

	"github.com/kfirgollan/NebiusMocker/pkg/serviceerror"
	"github.com/kfirgollan/NebiusMocker/services/msppostgresql/domain"
)

// BackupHandler implements nebius.msp.postgresql.v1alpha1.BackupService.
type BackupHandler struct {
	pgpb.UnimplementedBackupServiceServer
	svc *domain.Service
}

var _ pgpb.BackupServiceServer = (*BackupHandler)(nil)

// NewBackupHandler returns a handler backed by svc.
func NewBackupHandler(svc *domain.Service) *BackupHandler { return &BackupHandler{svc: svc} }

func backupViews(page domain.Page[domain.BackupRecord]) *pgpb.ListBackupsResponse {
	out := &pgpb.ListBackupsResponse{NextPageToken: page.NextPageToken}
	for _, r := range page.Items {
		out.Backups = append(out.Backups, r.Backup)
	}
	return out
}

// Get returns a backup of a cluster.
func (h *BackupHandler) Get(ctx context.Context, req *pgpb.GetBackupRequest) (*pgpb.Backup, error) {
	r, err := h.svc.GetBackup(ctx, req.GetClusterId(), req.GetBackupId())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return r.Backup, nil
}

// List returns one page of a project's backups.
func (h *BackupHandler) List(ctx context.Context, req *pgpb.ListBackupsRequest) (*pgpb.ListBackupsResponse, error) {
	page, err := h.svc.ListBackups(ctx, req.GetParentId(), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return backupViews(page), nil
}

// ListByCluster returns one page of a cluster's backups.
func (h *BackupHandler) ListByCluster(ctx context.Context, req *pgpb.ListBackupsByClusterRequest) (*pgpb.ListBackupsResponse, error) {
	page, err := h.svc.ListBackupsByCluster(ctx, req.GetClusterId(), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return backupViews(page), nil
}

// Create starts an on-demand backup operation.
func (h *BackupHandler) Create(ctx context.Context, req *pgpb.CreateBackupRequest) (*commonv1alpha1pb.Operation, error) { //nolint:staticcheck // SA1019: see op
	o, err := h.svc.CreateBackup(ctx, req.GetClusterId(), req)
	return op(ctx, o, err)
}

// Delete starts a backup deletion operation.
func (h *BackupHandler) Delete(ctx context.Context, req *pgpb.DeleteBackupRequest) (*commonv1alpha1pb.Operation, error) { //nolint:staticcheck // SA1019: see op
	o, err := h.svc.DeleteBackup(ctx, req.GetClusterId(), req.GetBackupId(), req)
	return op(ctx, o, err)
}
