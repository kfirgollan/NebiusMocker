package domain

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/nebius/gosdk/proto/fieldmask/mask"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
)

// S3 bucket management (aws s3 mb / rb, s3api put-bucket-*). These go
// through exactly the BucketService.Create / Update / Delete operations the
// gRPC API uses (same validation, operation records with the equivalent
// request, same soft-delete semantics), and, since S3 calls are synchronous,
// wait for the operation to finish.

// opPollInterval is how often a synchronous S3 call checks its operation.
const opPollInterval = 10 * time.Millisecond

// CreateFromS3 creates a bucket with default settings in projectID (S3
// CreateBucket) and returns it once ACTIVE. A taken name is errs.AlreadyExists.
func (s *BucketService) CreateFromS3(ctx context.Context, projectID, name string) (*storagepb.Bucket, error) {
	req := &storagepb.CreateBucketRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: projectID, Name: name},
		Spec:     &storagepb.BucketSpec{},
	}
	op, err := s.Create(ctx, CreateBucketCmd{Metadata: req.GetMetadata(), Spec: req.GetSpec(), Request: req})
	if err != nil {
		return nil, err
	}
	if err := s.await(ctx, op); err != nil {
		return nil, err
	}
	return s.buckets.Get(ctx, op.ResourceID)
}

// DeleteFromS3 deletes an empty bucket exactly as BucketService.Delete does
// by default: soft delete with the default grace period (S3 DeleteBucket).
func (s *BucketService) DeleteFromS3(ctx context.Context, id string) error {
	req := &storagepb.DeleteBucketRequest{Id: id}
	op, err := s.Delete(ctx, DeleteBucketCmd{ID: id, Request: req})
	if err != nil {
		return err
	}
	return s.await(ctx, op)
}

// UpdateSpecFromS3 changes one part of a bucket's spec (S3
// PutBucketVersioning / PutBucketLifecycleConfiguration / PutBucketCors and
// their deletes) through BucketService.Update with the full new spec (the
// spec is reset and replaced, so a cleared setting is removed).
func (s *BucketService) UpdateSpecFromS3(ctx context.Context, id string, mutate func(*storagepb.BucketSpec)) error {
	stored, err := s.buckets.Get(ctx, id)
	if err != nil {
		return err
	}
	if !usable(stored) {
		return errs.NotFound(id)
	}
	spec := proto.CloneOf(stored.GetSpec())
	if spec == nil {
		spec = &storagepb.BucketSpec{}
	}
	mutate(spec)
	if proto.Equal(spec, stored.GetSpec()) {
		return nil
	}
	md := &commonpb.ResourceMetadata{Id: id, ParentId: stored.GetMetadata().GetParentId()}
	resetSpec, err := specResetMask()
	if err != nil {
		return err
	}
	req := &storagepb.UpdateBucketRequest{Metadata: md, Spec: spec}
	op, err := s.Update(ctx, UpdateBucketCmd{Metadata: md, Spec: spec, ResetMask: resetSpec, Request: req})
	if err != nil {
		return err
	}
	return s.await(ctx, op)
}

// specResetMask resets every replaceable spec field (all but the immutable
// filesystem_bucket), so the update replaces the spec and clears settings
// the new spec leaves unset.
func specResetMask() (*mask.Mask, error) {
	fields := (&storagepb.BucketSpec{}).ProtoReflect().Descriptor().Fields()
	paths := make([]string, 0, fields.Len())
	for i := range fields.Len() {
		if f := fields.Get(i); f.ContainingOneof() == nil {
			paths = append(paths, "spec."+string(f.Name()))
		}
	}
	m, err := mask.Parse(strings.Join(paths, ","))
	if err != nil {
		return nil, fmt.Errorf("spec reset mask: %w", err)
	}
	return m, nil
}

// await waits until op has finished and returns its error.
func (s *BucketService) await(ctx context.Context, op *operations.Operation) error {
	for !op.Done() {
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-s.clk.After(opPollInterval):
		}
		var err error
		if op, err = s.ops.Get(ctx, op.ID); err != nil {
			return err
		}
	}
	return op.Err
}

// CheckCapacity enforces the filesystem size of a filesystem bucket (see
// checkFilesystemCapacity) and spec.max_size_bytes: a write of incoming bytes is
// refused with errs.KindQuotaExceeded when the bucket's stored bytes (current
// and noncurrent versions plus uploaded multipart parts) would exceed it.
// Concurrent writes are not serialized, so — as Nebius documents — the size
// can end up slightly above the limit.
func (s *BucketService) CheckCapacity(ctx context.Context, b *storagepb.Bucket, incoming int64) error {
	if IsFilesystemBucket(b) {
		if err := s.checkFilesystemCapacity(ctx, b, incoming); err != nil {
			return err
		}
	}
	limit := b.GetSpec().GetMaxSizeBytes()
	if limit == 0 {
		return nil
	}
	name, err := s.Backend(ctx, b)
	if err != nil {
		return err
	}
	cs, err := ComputeCounters(ctx, s.content, name)
	if err != nil {
		return fmt.Errorf("bucket %s size: %w", b.GetMetadata().GetName(), err)
	}
	if used := UsedBytes(cs); used+incoming > limit {
		return &errs.Error{Kind: errs.KindQuotaExceeded, ResourceID: b.GetMetadata().GetId(), Message: fmt.Sprintf(
			"the bucket size limit is %d bytes (spec.max_size_bytes); %d bytes are stored and the request adds %d", limit, used, incoming)}
	}
	return nil
}

// ObjectSize returns the size of an object version ("" for the current one),
// e.g. a copy source.
func (s *BucketService) ObjectSize(ctx context.Context, bucket, key, versionID string) (int64, error) {
	for v, err := range s.content.ListVersions(ctx, bucket, key) {
		if err != nil {
			return 0, err
		}
		if v.Key != key || v.DeleteMarker {
			continue
		}
		if (versionID == "" && v.IsLatest) || v.VersionID == versionID {
			return v.Size, nil
		}
	}
	return 0, objstore.ErrNotFound
}

// ApplyLifecycles runs every usable bucket's lifecycle rules against the
// object store at the current (injected) clock time. The module runs it
// periodically.
func (s *BucketService) ApplyLifecycles(ctx context.Context) (LifecycleResult, error) {
	all, err := s.buckets.ListAll(ctx)
	if err != nil {
		return LifecycleResult{}, err
	}
	var total LifecycleResult
	var errList []error
	now := s.clk.Now()
	for _, b := range all {
		cfg := b.GetSpec().GetLifecycleConfiguration()
		if !usable(b) || len(cfg.GetRules()) == 0 {
			continue
		}
		r, err := ApplyLifecycle(ctx, s.content, b.GetMetadata().GetName(), cfg, now)
		total.Expired += r.Expired
		total.NoncurrentExpired += r.NoncurrentExpired
		total.MarkersRemoved += r.MarkersRemoved
		total.UploadsAborted += r.UploadsAborted
		if err != nil {
			errList = append(errList, fmt.Errorf("lifecycle of %s: %w", b.GetMetadata().GetName(), err))
		}
	}
	return total, errors.Join(errList...)
}
