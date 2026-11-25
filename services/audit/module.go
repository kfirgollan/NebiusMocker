// Package audit is the NebiusMocker module for Nebius Audit (audit.v2).
//
// Implemented: AuditEventService.List over the shared in-memory audit log
// and AuditEventExportService (Start, Cancel, Get, List), which writes
// matching events to object storage as JSON Lines. See docs/services/audit.md.
package audit

import (
	"context"
	"fmt"

	"google.golang.org/grpc"

	auditv2pb "github.com/nebius/gosdk/proto/nebius/audit/v2"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/pkg/reqctx"
	"github.com/kfirgollan/NebiusMocker/services/audit/api"
	"github.com/kfirgollan/NebiusMocker/services/audit/dal"
	"github.com/kfirgollan/NebiusMocker/services/audit/domain"
)

// Module is the audit service module.
type Module struct {
	exports *domain.ExportService
}

var (
	_ mocker.Module    = (*Module)(nil)
	_ mocker.Lifecycle = (*Module)(nil)
)

// New returns the audit module.
func New() *Module { return &Module{} }

// Name implements mocker.Module.
func (*Module) Name() string { return "audit" }

// Register builds the audit services, installs authorization rules and
// registers the gRPC services.
func (m *Module) Register(_ context.Context, reg grpc.ServiceRegistrar, deps mocker.Deps) error {
	exports := dal.NewExportRepo(deps.Store)
	deps.Authz.RegisterKind(domain.KindExport, func(ctx context.Context, id string) (string, error) {
		e, err := exports.Get(ctx, id)
		return e.Resource.GetMetadata().GetParentId(), err
	})
	api.RegisterAuthzRules(deps.Rules)

	events, err := domain.NewEventService(deps.AuditLog, deps.Authz, regionNames(deps.Config.Regions))
	if err != nil {
		return fmt.Errorf("audit: %w", err)
	}
	m.exports, err = domain.NewExportService(domain.ExportDeps{
		Exports: exports, Events: deps.AuditLog, Hierarchy: deps.Authz,
		Buckets: buckets{authz: deps.Authz, peers: deps.Peers, enforce: deps.Config.Authz == authz.ModeEnforce},
		Writer:  deps.ObjStore, Regions: regionNames(deps.Config.Regions),
		Ops: deps.Ops, IDs: deps.IDs, Clock: deps.Clock,
		Delay: deps.Config.Audit.ExportDuration, Log: deps.Log,
	})
	if err != nil {
		return fmt.Errorf("audit: %w", err)
	}
	auditv2pb.RegisterAuditEventServiceServer(reg, api.NewEventHandler(events))
	auditv2pb.RegisterAuditEventExportServiceServer(reg, api.NewExportHandler(m.exports))
	return nil
}

// Start implements mocker.Lifecycle: it starts the export runner.
func (m *Module) Start(context.Context) error {
	m.exports.StartRunner()
	return nil
}

// Stop implements mocker.Lifecycle: it stops the export runner.
func (m *Module) Stop(context.Context) error {
	m.exports.StopRunner()
	return nil
}

// storageBucketKind is the NID type of Object Storage buckets.
const storageBucketKind = "storagebucket"

// permObjectCreate is the permission to write objects into a bucket; the
// storage.uploader role grants it. It mirrors storage's PermObjectWrite in
// services/storage/domain/s3.go, which an audit package must not import.
const permObjectCreate authz.Permission = "storage.objects.create"

// buckets adapts the authz hierarchy and the storage module's
// peer.BucketResolver to domain.Buckets. Exports write to Object Storage, so
// they need the storage module.
type buckets struct {
	authz   *authz.Engine
	peers   *peer.Directory
	enforce bool
}

var _ domain.Buckets = buckets{}

// resolver returns the storage module's bucket resolver, or errs.BadState
// when the storage module is not enabled.
func (b buckets) resolver(bucketID string) (peer.BucketResolver, error) {
	r, ok := peer.Lookup(b.peers, peer.BucketResolverKey)
	if !ok || !b.authz.HasKind(storageBucketKind) {
		return nil, errs.BadState(bucketID, "audit exports write to Object Storage: enable the storage module (NEBIUS_MOCKER_SERVICES)")
	}
	return r, nil
}

// Validate requires the bucket to exist in the same tenant as parentID and,
// under enforced authorization, the caller to hold storage.objects.create on
// it (the Nebius docs: "You must have storage.uploader role for the
// destination bucket").
func (b buckets) Validate(ctx context.Context, bucketID, parentID string) error {
	if _, err := b.resolver(bucketID); err != nil {
		return err
	}
	notFound := errs.BadRequest(errs.Violation{
		Field:   "spec.nebius_object_storage.bucket_by_id.id",
		Message: fmt.Sprintf("bucket %s not found in the tenant of %s", bucketID, parentID),
	})
	root, err := b.authz.Root(ctx, bucketID)
	if errs.IsKind(err, errs.KindNotFound) {
		return notFound
	}
	if err != nil {
		return err
	}
	want, err := b.authz.Root(ctx, parentID)
	if err != nil {
		return err
	}
	if root != want {
		return notFound
	}
	if !b.enforce {
		return nil
	}
	d, err := b.authz.Check(ctx, reqctx.PrincipalFrom(ctx).ID, permObjectCreate, bucketID)
	if err != nil {
		return err
	}
	if !d.Allowed {
		return errs.PermissionDenied(bucketID)
	}
	return nil
}

// BackendName resolves the backend bucket through the storage module.
func (b buckets) BackendName(ctx context.Context, bucketID string) (string, error) {
	r, err := b.resolver(bucketID)
	if err != nil {
		return "", err
	}
	name, err := r.BackendBucket(ctx, bucketID)
	if err != nil {
		return "", fmt.Errorf("resolve bucket %s: %w", bucketID, err)
	}
	return name, nil
}

// regionNames lists the names of the emulated regions, default first.
func regionNames(regions []config.Region) []string {
	names := make([]string, 0, len(regions))
	for _, r := range regions {
		names = append(names, r.Name)
	}
	return names
}
