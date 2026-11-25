// Package api is the Object Storage gRPC view layer: proto ⇄ domain
// conversion and error mapping only.
package api

import (
	"context"
	"time"

	"github.com/nebius/gosdk/proto/fieldmask/grpcheader"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	opsapi "github.com/kfirgollan/NebiusMocker/pkg/operations/api"
	"github.com/kfirgollan/NebiusMocker/pkg/serviceerror"
	"github.com/kfirgollan/NebiusMocker/services/storage/domain"
)

const bucketService = "nebius.storage.v1.BucketService"

// RegisterAuthzRules declares the permission of every BucketService RPC.
// Purge counts as delete and Undelete as update, so the seeded editor role
// can manage the whole bucket lifecycle.
func RegisterAuthzRules(r *authz.Rules) {
	r.CRUD(bucketService, "storage", "buckets")
	m := func(method string) string { return "/" + bucketService + "/" + method }
	r.Add(m("ListWithFilter"), authz.OnParent(authz.Perm("storage", "buckets", authz.VerbList)))
	r.Add(m("Purge"), authz.OnID(authz.Perm("storage", "buckets", authz.VerbDelete)))
	r.Add(m("Undelete"), authz.OnID(authz.Perm("storage", "buckets", authz.VerbUpdate)))
}

// BucketHandler implements nebius.storage.v1.BucketService.
type BucketHandler struct {
	storagepb.UnimplementedBucketServiceServer
	svc *domain.BucketService
}

var _ storagepb.BucketServiceServer = (*BucketHandler)(nil)

// NewBucketHandler returns a handler backed by svc.
func NewBucketHandler(svc *domain.BucketService) *BucketHandler { return &BucketHandler{svc: svc} }

// Get returns a bucket by ID.
func (h *BucketHandler) Get(ctx context.Context, req *storagepb.GetBucketRequest) (*storagepb.Bucket, error) {
	b, err := h.svc.Get(ctx, req.GetId())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return b, nil
}

// GetByName returns a bucket by project and name.
func (h *BucketHandler) GetByName(ctx context.Context, req *storagepb.GetBucketByNameRequest) (*storagepb.Bucket, error) {
	b, err := h.svc.GetByName(ctx, req.GetParentId(), req.GetName())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return b, nil
}

// List returns one page of a project's buckets.
func (h *BucketHandler) List(ctx context.Context, req *storagepb.ListBucketsRequest) (*storagepb.ListBucketsResponse, error) {
	page, err := h.svc.List(ctx, req.GetParentId(), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return &storagepb.ListBucketsResponse{Items: page.Items, NextPageToken: page.NextPageToken}, nil
}

// ListWithFilter returns one page of a project's buckets matching the filters.
func (h *BucketHandler) ListWithFilter(ctx context.Context, req *storagepb.ListBucketsWithFilterRequest) (*storagepb.ListBucketsWithFilterResponse, error) {
	page, err := h.svc.ListWithFilter(ctx, req.GetParentId(), req.GetPageSize(), req.GetPageToken(), req.GetFilters())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return &storagepb.ListBucketsWithFilterResponse{Items: page.Items, NextPageToken: page.NextPageToken}, nil
}

// Create starts a bucket creation operation.
func (h *BucketHandler) Create(ctx context.Context, req *storagepb.CreateBucketRequest) (*commonpb.Operation, error) {
	return opResult(ctx)(h.svc.Create(ctx, domain.CreateBucketCmd{Metadata: req.GetMetadata(), Spec: req.GetSpec(), Request: req}))
}

// Update starts a bucket update operation; X-ResetMask selects fields to clear.
func (h *BucketHandler) Update(ctx context.Context, req *storagepb.UpdateBucketRequest) (*commonpb.Operation, error) {
	resetMask, err := grpcheader.GetResetMaskFromIncomingContext(ctx)
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, errs.BadRequest(errs.Violation{Field: grpcheader.ResetMask, Message: err.Error()}))
	}
	return opResult(ctx)(h.svc.Update(ctx, domain.UpdateBucketCmd{
		Metadata: req.GetMetadata(), Spec: req.GetSpec(), ResetMask: resetMask, Request: req,
	}))
}

// Delete starts a (soft) bucket deletion operation.
func (h *BucketHandler) Delete(ctx context.Context, req *storagepb.DeleteBucketRequest) (*commonpb.Operation, error) {
	cmd := domain.DeleteBucketCmd{ID: req.GetId(), Request: req}
	switch p := req.GetPurge().(type) {
	case *storagepb.DeleteBucketRequest_PurgeAt:
		at := p.PurgeAt.AsTime()
		cmd.PurgeAt = &at
	case *storagepb.DeleteBucketRequest_Ttl:
		ttl := time.Duration(p.Ttl.GetSeconds())*time.Second + time.Duration(p.Ttl.GetNanos())
		cmd.TTL = &ttl
	}
	return opResult(ctx)(h.svc.Delete(ctx, cmd))
}

// Purge starts the immediate deletion of a soft-deleted bucket.
func (h *BucketHandler) Purge(ctx context.Context, req *storagepb.PurgeBucketRequest) (*commonpb.Operation, error) {
	return opResult(ctx)(h.svc.Purge(ctx, req.GetId(), req))
}

// Undelete starts restoring a soft-deleted bucket.
func (h *BucketHandler) Undelete(ctx context.Context, req *storagepb.UndeleteBucketRequest) (*commonpb.Operation, error) {
	return opResult(ctx)(h.svc.Undelete(ctx, req.GetId(), req))
}

// opResult converts a domain (operation, error) pair to the RPC result.
func opResult(ctx context.Context) func(*operations.Operation, error) (*commonpb.Operation, error) {
	return func(op *operations.Operation, err error) (*commonpb.Operation, error) {
		if err != nil {
			return nil, serviceerror.FromDomain(ctx, err)
		}
		return opsapi.ToProto(ctx, op), nil
	}
}
