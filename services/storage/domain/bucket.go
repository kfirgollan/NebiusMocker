package domain

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/nebius/gosdk/proto/fieldmask/mask"
	fieldmaskpb "github.com/nebius/gosdk/proto/fieldmask/protobuf"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	"github.com/kfirgollan/NebiusMocker/pkg/reqctx"
)

const (
	defaultPageSize = 100
	maxPageSize     = 1000
)

// Settings are the bucket service's configurable values.
type Settings struct {
	// Region is reported in status.region (S3 clients sign for it).
	Region string
	// Domain is the S3 endpoint host; status.domain_name is "<bucket>.<Domain>".
	Domain string
	// PurgeGrace is how long a deleted bucket stays restorable by default.
	PurgeGrace time.Duration
}

// BucketService implements nebius.storage.v1.BucketService.
type BucketService struct {
	buckets BucketRepository
	backend ObjectBackend
	content ContentBackend
	parents Hierarchy
	ops     Operations
	ids     IDGenerator
	clk     clock.Clock
	set     Settings

	filesystems Filesystems
	access      AccessChecker
	mode        authz.Mode
	log         *slog.Logger
}

// BucketDeps are the dependencies of a BucketService. All are required.
type BucketDeps struct {
	Buckets BucketRepository
	Backend ObjectBackend
	// Content is the same object store, for counters and lifecycle rules.
	Content   ContentBackend
	Hierarchy Hierarchy
	Ops       Operations
	IDs       IDGenerator
	Clock     clock.Clock
	Settings  Settings
	// Filesystems resolves the compute filesystems of filesystem buckets;
	// nil rejects filesystem buckets.
	Filesystems Filesystems
	// Access and Mode authorize exposing a filesystem through a filesystem
	// bucket (PermFilesystemViaObjectStorage); a nil Access skips the check.
	Access AccessChecker
	Mode   authz.Mode
	// Log receives audit-mode authorization warnings; nil discards them.
	Log *slog.Logger
}

// NewBucketService wires a BucketService.
func NewBucketService(d BucketDeps) (*BucketService, error) {
	if d.Buckets == nil || d.Backend == nil || d.Content == nil || d.Hierarchy == nil || d.Ops == nil || d.IDs == nil || d.Clock == nil {
		return nil, errors.New("storage: bucket service dependencies must not be nil")
	}
	if d.Settings.Region == "" || d.Settings.Domain == "" || d.Settings.PurgeGrace <= 0 {
		return nil, errors.New("storage: region, domain and a positive purge grace are required")
	}
	return &BucketService{
		buckets: d.Buckets, backend: d.Backend, content: d.Content, parents: d.Hierarchy, ops: d.Ops, ids: d.IDs, clk: d.Clock, set: d.Settings,
		filesystems: d.Filesystems, access: d.Access, mode: d.Mode, log: cmp.Or(d.Log, slog.New(slog.DiscardHandler)),
	}, nil
}

// CreateBucketCmd is the input of Create.
type CreateBucketCmd struct {
	Metadata *commonpb.ResourceMetadata
	Spec     *storagepb.BucketSpec
	Request  proto.Message
}

// UpdateBucketCmd is the input of Update (full replace guarded by a reset mask).
type UpdateBucketCmd struct {
	Metadata  *commonpb.ResourceMetadata
	Spec      *storagepb.BucketSpec
	ResetMask *mask.Mask
	Request   proto.Message
}

// DeleteBucketCmd is the input of Delete. At most one of PurgeAt and TTL is
// set; neither means the default grace period. A zero TTL purges at once.
type DeleteBucketCmd struct {
	ID      string
	PurgeAt *time.Time
	TTL     *time.Duration
	Request proto.Message
}

// Get returns a bucket by ID (including soft-deleted ones).
func (s *BucketService) Get(ctx context.Context, id string) (*storagepb.Bucket, error) {
	return s.withCounters(ctx)(s.buckets.Get(ctx, id))
}

// BackendBucket implements peer.BucketResolver: the object-store bucket that
// holds a Nebius bucket's objects (names map 1:1, except for filesystem
// buckets, whose objects are the filesystem's files).
func (s *BucketService) BackendBucket(ctx context.Context, id string) (string, error) {
	b, err := s.buckets.Get(ctx, id)
	if err != nil {
		return "", err
	}
	return s.Backend(ctx, b)
}

// GetByName returns a bucket by project and name.
func (s *BucketService) GetByName(ctx context.Context, parentID, name string) (*storagepb.Bucket, error) {
	return s.withCounters(ctx)(s.buckets.GetByName(ctx, parentID, name))
}

// List returns one page of a project's buckets in creation order.
func (s *BucketService) List(ctx context.Context, parentID string, pageSize int64, pageToken string) (BucketPage, error) {
	if pageSize < 0 || pageSize > maxPageSize {
		return BucketPage{}, errs.BadRequest(errs.Violation{
			Field: "page_size", Message: fmt.Sprintf("must be between 0 and %d", maxPageSize),
		})
	}
	size := int(pageSize)
	if size == 0 {
		size = defaultPageSize
	}
	page, err := s.buckets.List(ctx, parentID, size, pageToken)
	if err != nil {
		return BucketPage{}, err
	}
	for i, b := range page.Items {
		if page.Items[i], err = s.withCounters(ctx)(b, nil); err != nil {
			return BucketPage{}, err
		}
	}
	return page, nil
}

// ListWithFilter is List restricted by bucket type and filesystem: it walks
// the project's buckets in creation order and keeps those that match.
func (s *BucketService) ListWithFilter(ctx context.Context, parentID string, pageSize int64, pageToken string, f *storagepb.ListBucketsWithFilterRequest_Filters) (BucketPage, error) {
	if f.GetBucketType() == storagepb.BucketStatus_BUCKET_TYPE_UNSPECIFIED && f.GetFilesystemId() == "" {
		return s.List(ctx, parentID, pageSize, pageToken)
	}
	if pageSize < 0 || pageSize > maxPageSize {
		return BucketPage{}, errs.BadRequest(errs.Violation{Field: "page_size", Message: fmt.Sprintf("must be between 0 and %d", maxPageSize)})
	}
	want := int(pageSize)
	if want == 0 {
		want = defaultPageSize
	}
	var out BucketPage
	for token := pageToken; ; {
		page, err := s.List(ctx, parentID, 1, token)
		if err != nil {
			return BucketPage{}, err
		}
		for _, b := range page.Items {
			if matchesFilter(b, f) {
				out.Items = append(out.Items, b)
			}
		}
		token = page.NextPageToken
		if token == "" {
			return out, nil
		}
		if len(out.Items) == want {
			out.NextPageToken = token
			return out, nil
		}
	}
}

// Create validates the request, reserves the name and creates the backing
// bucket when the operation runs.
func (s *BucketService) Create(ctx context.Context, cmd CreateBucketCmd) (*operations.Operation, error) {
	b, err := s.newBucket(ctx, cmd)
	if err != nil {
		return nil, err
	}
	id := b.GetMetadata().GetId()
	return s.ops.Submit(ctx, operations.Spec{
		ResourceID:  id,
		Description: "Create bucket",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     cmd.Request,
		Prepare:     func(ctx context.Context) error { return s.buckets.Create(ctx, b) },
		Work:        func(ctx context.Context) error { return s.provision(ctx, id) },
	})
}

// provision creates the backing bucket and activates the record; on failure
// the record is removed so the name is free again.
func (s *BucketService) provision(ctx context.Context, id string) error {
	b, err := s.buckets.Get(ctx, id)
	if err != nil {
		return err
	}
	if IsFilesystemBucket(b) { // the files live in the filesystem's own bucket
		b.Status.State = storagepb.BucketStatus_ACTIVE
		return s.buckets.Update(ctx, b)
	}
	name := b.GetMetadata().GetName()
	err = s.backend.EnsureBucket(ctx, name)
	if err == nil {
		err = s.backend.SetVersioning(ctx, name, toBackendVersioning(b.GetSpec().GetVersioningPolicy()))
	}
	if err != nil {
		if derr := s.buckets.Delete(ctx, id); derr != nil {
			err = errors.Join(err, fmt.Errorf("roll back bucket %s: %w", id, derr))
		}
		return fmt.Errorf("create backing bucket %s: %w", name, err)
	}
	b.Status.State = storagepb.BucketStatus_ACTIVE
	return s.buckets.Update(ctx, b)
}

// Update applies a full-replace update with reset-mask semantics. The bucket
// is UPDATING until the backend has the new versioning state.
func (s *BucketService) Update(ctx context.Context, cmd UpdateBucketCmd) (*operations.Operation, error) {
	id := cmd.Metadata.GetId()
	if id == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.id", Message: "required"})
	}
	var stored *storagepb.Bucket
	return s.ops.Submit(ctx, operations.Spec{
		ResourceID:  id,
		Description: "Update bucket",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     cmd.Request,
		Prepare: func(ctx context.Context) error {
			var err error
			stored, err = s.prepareUpdate(ctx, cmd)
			return err
		},
		Work: func(ctx context.Context) error { return s.finishUpdate(ctx, id, stored) },
	})
}

func (s *BucketService) prepareUpdate(ctx context.Context, cmd UpdateBucketCmd) (*storagepb.Bucket, error) {
	stored, err := s.buckets.Get(ctx, cmd.Metadata.GetId())
	if err != nil {
		return nil, err
	}
	if st := stored.GetStatus().GetState(); st != storagepb.BucketStatus_ACTIVE {
		return nil, errs.BadState(stored.GetMetadata().GetId(), "bucket is "+st.String()+", only ACTIVE buckets can be updated")
	}
	updated, err := s.patch(stored, cmd)
	if err != nil {
		return nil, err
	}
	if proto.Equal(updated, stored) {
		return stored, nil
	}
	updated.Metadata.ResourceVersion++
	updated.Metadata.UpdatedAt = timestamppb.New(s.clk.Now())
	updated.Status.State = storagepb.BucketStatus_UPDATING
	return stored, s.buckets.Update(ctx, updated)
}

// finishUpdate applies the versioning change and reactivates the bucket. If
// the backend fails, the previous record is restored.
func (s *BucketService) finishUpdate(ctx context.Context, id string, before *storagepb.Bucket) error {
	b, err := s.buckets.Get(ctx, id)
	if err != nil {
		return err
	}
	if b.GetStatus().GetState() != storagepb.BucketStatus_UPDATING {
		return nil // nothing changed
	}
	want := b.GetSpec().GetVersioningPolicy()
	if want != before.GetSpec().GetVersioningPolicy() {
		if err := s.backend.SetVersioning(ctx, b.GetMetadata().GetName(), toBackendVersioning(want)); err != nil {
			if rerr := s.buckets.Update(ctx, before); rerr != nil {
				err = errors.Join(err, rerr)
			}
			return fmt.Errorf("apply versioning to %s: %w", b.GetMetadata().GetName(), err)
		}
	}
	b.Status.State = storagepb.BucketStatus_ACTIVE
	return s.buckets.Update(ctx, b)
}

// patch computes the updated bucket and enforces the update rules.
func (s *BucketService) patch(stored *storagepb.Bucket, cmd UpdateBucketCmd) (*storagepb.Bucket, error) {
	md, smd := cmd.Metadata, stored.GetMetadata()
	if p := md.GetParentId(); p != "" && p != smd.GetParentId() {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.parent_id", Message: "field is immutable"})
	}
	if n := md.GetName(); n != "" && n != smd.GetName() {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.name", Message: "bucket names are immutable"})
	}
	if v := md.GetResourceVersion(); v != 0 && v != smd.GetResourceVersion() {
		return nil, errs.Conflict(smd.GetId(), fmt.Sprintf("resource_version %d does not match current version %d", v, smd.GetResourceVersion()))
	}
	// The bucket type is immutable: an update may repeat it (Terraform sends
	// the whole spec) but not change it.
	if t := cmd.Spec.GetBucketType(); t != nil && !proto.Equal(
		&storagepb.BucketSpec{BucketType: t}, &storagepb.BucketSpec{BucketType: stored.GetSpec().GetBucketType()}) {
		return nil, errs.BadRequest(errs.Violation{Field: "spec.filesystem_bucket", Message: "field is immutable"})
	}
	resetMask := cmd.ResetMask
	if resetMask == nil {
		resetMask = mask.New()
	}
	updated := proto.CloneOf(stored)
	patch := &storagepb.Bucket{Metadata: &commonpb.ResourceMetadata{Labels: md.GetLabels()}, Spec: cmd.Spec}
	if err := fieldmaskpb.PatchWithResetMask(updated, patch, resetMask); err != nil {
		return nil, errs.BadRequest(errs.Violation{Field: "X-ResetMask", Message: err.Error()})
	}
	labels := updated.GetMetadata().GetLabels()
	updated.Metadata = proto.CloneOf(smd)
	updated.Metadata.Labels = labels
	if updated.GetSpec() == nil {
		updated.Spec = &storagepb.BucketSpec{}
	}
	// NON_EMPTY_DEFAULT: an unset policy keeps the current one.
	if updated.GetSpec().GetVersioningPolicy() == storagepb.VersioningPolicy_VERSIONING_POLICY_UNSPECIFIED {
		updated.Spec.VersioningPolicy = stored.GetSpec().GetVersioningPolicy()
	}
	if err := checkVersioningTransition(stored.GetSpec().GetVersioningPolicy(), updated.GetSpec().GetVersioningPolicy()); err != nil {
		return nil, err
	}
	if err := validateSpec(updated.GetSpec()); err != nil {
		return nil, err
	}
	updated.Status = proto.CloneOf(stored.GetStatus())
	updated.Status.AnonymousAccessEnabled = AnonymousAccess(updated.GetSpec().GetBucketPolicy())
	return updated, nil
}

// Delete soft-deletes a bucket (SCHEDULED_FOR_DELETION until purge_at), or
// purges it at once for a zero TTL or a past purge_at. Only empty buckets can
// be deleted.
func (s *BucketService) Delete(ctx context.Context, cmd DeleteBucketCmd) (*operations.Operation, error) {
	var deletedAt, purgeAt time.Time
	return s.ops.Submit(ctx, operations.Spec{
		ResourceID:  cmd.ID,
		Description: "Delete bucket",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     cmd.Request,
		Prepare: func(ctx context.Context) error {
			var err error
			deletedAt = s.clk.Now()
			purgeAt, err = s.prepareDelete(ctx, cmd, deletedAt)
			return err
		},
		Work: func(ctx context.Context) error {
			if !purgeAt.After(s.clk.Now()) {
				return s.purge(ctx, cmd.ID)
			}
			return s.schedule(ctx, cmd.ID, deletedAt, purgeAt)
		},
	})
}

func (s *BucketService) prepareDelete(ctx context.Context, cmd DeleteBucketCmd, now time.Time) (time.Time, error) {
	b, err := s.buckets.Get(ctx, cmd.ID)
	if err != nil {
		return time.Time{}, err
	}
	if st := b.GetStatus().GetState(); st != storagepb.BucketStatus_ACTIVE {
		return time.Time{}, errs.BadState(cmd.ID, "bucket is "+st.String()+", only ACTIVE buckets can be deleted")
	}
	if IsFilesystemBucket(b) {
		return now, checkFilesystemBucketDelete(cmd, now)
	}
	empty, err := s.backend.BucketEmpty(ctx, b.GetMetadata().GetName())
	if err != nil && !errors.Is(err, objstore.ErrBucketNotFound) {
		return time.Time{}, fmt.Errorf("check bucket %s is empty: %w", b.GetMetadata().GetName(), err)
	}
	if err == nil && !empty {
		return time.Time{}, errs.BadState(cmd.ID, "the bucket is not empty; delete its objects first")
	}
	switch {
	case cmd.TTL != nil:
		if *cmd.TTL < 0 {
			return time.Time{}, errs.BadRequest(errs.Violation{Field: "ttl", Message: "must not be negative"})
		}
		return now.Add(*cmd.TTL), nil
	case cmd.PurgeAt != nil:
		return *cmd.PurgeAt, nil
	default:
		return now.Add(s.set.PurgeGrace), nil
	}
}

func (s *BucketService) schedule(ctx context.Context, id string, deletedAt, purgeAt time.Time) error {
	b, err := s.buckets.Get(ctx, id)
	if err != nil {
		return err
	}
	b.Status.State = storagepb.BucketStatus_SCHEDULED_FOR_DELETION
	b.Status.DeletedAt = timestamppb.New(deletedAt)
	b.Status.PurgeAt = timestamppb.New(purgeAt)
	return s.buckets.Update(ctx, b)
}

// Purge deletes a soft-deleted bucket immediately.
func (s *BucketService) Purge(ctx context.Context, id string, request proto.Message) (*operations.Operation, error) {
	return s.ops.Submit(ctx, operations.Spec{
		ResourceID:  id,
		Description: "Purge bucket",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     request,
		Prepare:     func(ctx context.Context) error { return s.requireScheduled(ctx, id, "purged") },
		Work:        func(ctx context.Context) error { return s.purge(ctx, id) },
	})
}

// Undelete restores a soft-deleted bucket to ACTIVE.
func (s *BucketService) Undelete(ctx context.Context, id string, request proto.Message) (*operations.Operation, error) {
	return s.ops.Submit(ctx, operations.Spec{
		ResourceID:  id,
		Description: "Undelete bucket",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     request,
		Prepare:     func(ctx context.Context) error { return s.requireScheduled(ctx, id, "undeleted") },
		Work: func(ctx context.Context) error {
			b, err := s.buckets.Get(ctx, id)
			if err != nil {
				return err
			}
			b.Status.State = storagepb.BucketStatus_ACTIVE
			b.Status.DeletedAt = nil
			b.Status.PurgeAt = nil
			return s.buckets.Update(ctx, b)
		},
	})
}

func (s *BucketService) requireScheduled(ctx context.Context, id, verb string) error {
	b, err := s.buckets.Get(ctx, id)
	if err != nil {
		return err
	}
	if st := b.GetStatus().GetState(); st != storagepb.BucketStatus_SCHEDULED_FOR_DELETION {
		return errs.BadState(id, "bucket is "+st.String()+", only SCHEDULED_FOR_DELETION buckets can be "+verb)
	}
	return nil
}

// purge removes the backing bucket (with any remaining object versions) and
// the record, which releases the name.
func (s *BucketService) purge(ctx context.Context, id string) error {
	b, err := s.buckets.Get(ctx, id)
	if err != nil {
		return err
	}
	if IsFilesystemBucket(b) { // the files belong to the filesystem and stay
		return s.buckets.Delete(ctx, id)
	}
	if err := s.backend.DeleteBucket(ctx, b.GetMetadata().GetName()); err != nil {
		return fmt.Errorf("delete backing bucket %s: %w", b.GetMetadata().GetName(), err)
	}
	return s.buckets.Delete(ctx, id)
}

// SweepExpired purges every soft-deleted bucket whose purge_at has passed and
// returns how many it purged. The module runs it periodically.
func (s *BucketService) SweepExpired(ctx context.Context) (int, error) {
	all, err := s.buckets.ListAll(ctx)
	if err != nil {
		return 0, err
	}
	now := s.clk.Now()
	var purged int
	var errList []error
	for _, b := range all {
		st := b.GetStatus()
		if st.GetState() != storagepb.BucketStatus_SCHEDULED_FOR_DELETION || st.GetPurgeAt().AsTime().After(now) {
			continue
		}
		if err := s.purge(ctx, b.GetMetadata().GetId()); err != nil {
			errList = append(errList, err)
			continue
		}
		purged++
	}
	return purged, errors.Join(errList...)
}

func (s *BucketService) newBucket(ctx context.Context, cmd CreateBucketCmd) (*storagepb.Bucket, error) {
	md := cmd.Metadata
	if md.GetId() != "" {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.id", Message: "must not be set on create"})
	}
	if err := s.checkParent(ctx, md.GetParentId()); err != nil {
		return nil, err
	}
	if err := ValidateBucketName(md.GetName()); err != nil {
		return nil, err
	}
	spec := proto.CloneOf(cmd.Spec)
	if spec == nil {
		spec = &storagepb.BucketSpec{}
	}
	if spec.GetVersioningPolicy() == storagepb.VersioningPolicy_VERSIONING_POLICY_UNSPECIFIED {
		spec.VersioningPolicy = storagepb.VersioningPolicy_DISABLED // NON_EMPTY_DEFAULT
	}
	if err := validateSpec(spec); err != nil {
		return nil, err
	}
	bucketType := storagepb.BucketStatus_REGULAR
	if spec.GetFilesystemBucket() != nil {
		if err := s.checkNewFilesystemBucket(ctx, md.GetParentId(), spec); err != nil {
			return nil, err
		}
		bucketType = storagepb.BucketStatus_FILESYSTEM
	}
	id, err := s.ids.New(KindBucket)
	if err != nil {
		return nil, fmt.Errorf("new bucket id: %w", err)
	}
	now := timestamppb.New(s.clk.Now())
	return &storagepb.Bucket{
		Metadata: &commonpb.ResourceMetadata{
			Id: id, ParentId: md.GetParentId(), Name: md.GetName(), Labels: maps.Clone(md.GetLabels()),
			ResourceVersion: 1, CreatedAt: now, UpdatedAt: now,
		},
		Spec: spec,
		Status: &storagepb.BucketStatus{
			State:                  storagepb.BucketStatus_CREATING,
			SuspensionState:        storagepb.BucketStatus_NOT_SUSPENDED,
			DomainName:             md.GetName() + "." + s.set.Domain,
			Region:                 s.set.Region,
			BucketType:             bucketType,
			AnonymousAccessEnabled: AnonymousAccess(spec.GetBucketPolicy()),
		},
	}, nil
}

// checkParent requires an existing project.
func (s *BucketService) checkParent(ctx context.Context, parentID string) error {
	if parentID == "" {
		return errs.BadRequest(errs.Violation{Field: "metadata.parent_id", Message: "required"})
	}
	if kindOf(parentID) != KindProject {
		return errs.BadRequest(errs.Violation{Field: "metadata.parent_id", Message: "must be a project ID"})
	}
	if _, err := s.parents.Parent(ctx, parentID); err != nil {
		return fmt.Errorf("bucket parent %s: %w", parentID, err)
	}
	return nil
}

// kindOf returns the NID type prefix of id ("" when it has none).
func kindOf(id string) string {
	kind, _, ok := strings.Cut(id, "-")
	if !ok {
		return ""
	}
	return kind
}

// checkVersioningTransition allows disabled → enabled/suspended and
// enabled ↔ suspended, as the BucketSpec documents. Versioning can never be
// disabled again.
func checkVersioningTransition(from, to storagepb.VersioningPolicy) error {
	if from == to || from == storagepb.VersioningPolicy_DISABLED {
		return nil
	}
	if to == storagepb.VersioningPolicy_DISABLED {
		return errs.BadRequest(errs.Violation{
			Field: "spec.versioning_policy", Message: "versioning cannot be disabled once enabled; suspend it instead",
		})
	}
	return nil
}

// validateSpec checks every spec setting. Settings the mocker cannot emulate
// are rejected by name rather than stored without effect.
func validateSpec(spec *storagepb.BucketSpec) error {
	if spec.GetMaxSizeBytes() < 0 {
		return errs.BadRequest(errs.Violation{Field: "spec.max_size_bytes", Message: "must not be negative"})
	}
	if spec.GetFilesystemBucket() != nil {
		if err := checkFilesystemSpec(spec); err != nil {
			return err
		}
	} else if err := checkStorageClass("spec.default_storage_class", spec.GetDefaultStorageClass()); err != nil {
		return err
	}
	if len(spec.GetLastAccess().GetConditions()) > 0 {
		return errs.BadRequest(errs.Violation{Field: "spec.last_access", Message: "not supported by NebiusMocker: last-access tracking is not emulated"})
	}
	for _, check := range []func() error{
		func() error { return ValidateLifecycle(spec.GetLifecycleConfiguration()) },
		func() error { return ValidateCORS(spec.GetCors()) },
		func() error { return ValidatePolicy(spec.GetBucketPolicy()) },
	} {
		if err := check(); err != nil {
			return err
		}
	}
	return nil
}

// withCounters fills status.counters of a usable bucket from the object
// store's actual content.
func (s *BucketService) withCounters(ctx context.Context) func(*storagepb.Bucket, error) (*storagepb.Bucket, error) {
	return func(b *storagepb.Bucket, err error) (*storagepb.Bucket, error) {
		if err != nil || !usable(b) {
			return b, err
		}
		name, err := s.Backend(ctx, b)
		if err != nil {
			return b, nil //nolint:nilerr // a filesystem bucket whose filesystem is unusable reports no counters; reading the bucket still works
		}
		cs, err := ComputeCounters(ctx, s.content, name)
		if errors.Is(err, objstore.ErrBucketNotFound) {
			return b, nil
		}
		if err != nil {
			return nil, fmt.Errorf("bucket %s counters: %w", b.GetMetadata().GetName(), err)
		}
		b = proto.CloneOf(b)
		b.Status.Counters = cs
		return b, nil
	}
}

func toBackendVersioning(p storagepb.VersioningPolicy) objstore.Versioning {
	switch p {
	case storagepb.VersioningPolicy_ENABLED:
		return objstore.VersioningEnabled
	case storagepb.VersioningPolicy_SUSPENDED:
		return objstore.VersioningSuspended
	case storagepb.VersioningPolicy_VERSIONING_POLICY_UNSPECIFIED, storagepb.VersioningPolicy_DISABLED:
		return objstore.VersioningUnset
	}
	return objstore.VersioningUnset
}
