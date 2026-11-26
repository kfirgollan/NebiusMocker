package domain

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/nebius/gosdk/proto/fieldmask/mask"
	fieldmaskpb "github.com/nebius/gosdk/proto/fieldmask/protobuf"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"
	storagev1alpha1pb "github.com/nebius/gosdk/proto/nebius/storage/v1alpha1"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	"github.com/kfirgollan/NebiusMocker/pkg/reqctx"
)

const (
	// defaultTransferPageSize is the List default ("Default: 10").
	defaultTransferPageSize = 10
	// defaultMaxParallel bounds concurrently running iterations.
	defaultMaxParallel = 4
)

// transferEngine runs one iteration of a plan (satisfied by *TransferEngine).
type transferEngine interface {
	Run(ctx context.Context, plan Plan, seq int64, watermark time.Time, progress func(Iteration)) (Iteration, time.Time)
}

// TransferDeps are the dependencies of a TransferService.
type TransferDeps struct {
	Repo      TransferRepository
	Buckets   BucketRepository
	Hierarchy Hierarchy
	Ops       Operations
	IDs       IDGenerator
	Clock     clock.Clock
	// Region is the only Nebius region the mocker serves (e.g. "eu-north1").
	Region string
	// Engine runs iterations (a *TransferEngine).
	Engine transferEngine
	// Log receives runner errors; nil discards them.
	Log *slog.Logger
	// MaxParallel bounds concurrently running iterations (default 4).
	MaxParallel int
}

// TransferService implements nebius.storage.v1.TransferService and the
// deprecated nebius.storage.v1alpha1.TransferService over one store, and runs
// the iterations of ACTIVE transfers (Run).
type TransferService struct {
	repo        TransferRepository
	buckets     BucketRepository
	parents     Hierarchy
	ops         Operations
	ids         IDGenerator
	clk         clock.Clock
	region      string
	engine      transferEngine
	log         *slog.Logger
	maxParallel int

	// mu serializes every read-modify-write of a record (RPC work, the
	// runner and iteration progress), so status writes never lose spec
	// updates and the running set is consistent with the stored state.
	mu sync.Mutex
	// runMu guards running; it is taken inside mu, never the other way.
	runMu   sync.Mutex
	running map[string]*runningIteration
	// wake nudges the runner (buffer 1: one pending signal is enough).
	wake chan struct{}
}

// runningIteration is an iteration goroutine: cancel interrupts it, done is
// closed once its result is stored.
type runningIteration struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// NewTransferService wires a TransferService.
func NewTransferService(d TransferDeps) (*TransferService, error) {
	if d.Repo == nil || d.Buckets == nil || d.Hierarchy == nil || d.Ops == nil || d.IDs == nil || d.Clock == nil || d.Engine == nil {
		return nil, errors.New("storage: transfer service dependencies must not be nil")
	}
	if d.Region == "" {
		return nil, errors.New("storage: transfer service needs the served region")
	}
	if d.MaxParallel < 0 {
		return nil, fmt.Errorf("storage: transfer MaxParallel must not be negative, got %d", d.MaxParallel)
	}
	return &TransferService{
		repo: d.Repo, buckets: d.Buckets, parents: d.Hierarchy, ops: d.Ops, ids: d.IDs, clk: d.Clock,
		region: d.Region, engine: d.Engine, log: cmp.Or(d.Log, slog.New(slog.DiscardHandler)),
		maxParallel: cmp.Or(d.MaxParallel, defaultMaxParallel),
		running:     map[string]*runningIteration{},
		wake:        make(chan struct{}, 1),
	}, nil
}

// CreateTransferV1Cmd is the input of CreateV1.
type CreateTransferV1Cmd struct {
	Metadata *commonpb.ResourceMetadata
	Spec     *storagepb.TransferSpec
	Request  proto.Message
}

// CreateTransferV1Alpha1Cmd is the input of CreateV1Alpha1.
type CreateTransferV1Alpha1Cmd struct {
	Metadata *commonpb.ResourceMetadata
	Spec     *storagev1alpha1pb.TransferSpec
	Request  proto.Message
}

// UpdateTransferV1Cmd is the input of UpdateV1 (full replace guarded by a reset mask).
type UpdateTransferV1Cmd struct {
	Metadata  *commonpb.ResourceMetadata
	Spec      *storagepb.TransferSpec
	ResetMask *mask.Mask
	Request   proto.Message
}

// UpdateTransferV1Alpha1Cmd is the input of UpdateV1Alpha1.
type UpdateTransferV1Alpha1Cmd struct {
	Metadata  *commonpb.ResourceMetadata
	Spec      *storagev1alpha1pb.TransferSpec
	ResetMask *mask.Mask
	Request   proto.Message
}

// CreateV1 validates a storage.v1 transfer; the operation stores it ACTIVE
// and its first iteration starts at once.
func (s *TransferService) CreateV1(ctx context.Context, cmd CreateTransferV1Cmd) (*operations.Operation, error) {
	return s.create(ctx, cmd.Request, func(ctx context.Context, id string) (*TransferRecord, error) {
		md, err := s.newMetadata(ctx, id, cmd.Metadata)
		if err != nil {
			return nil, err
		}
		spec := proto.CloneOf(cmd.Spec)
		if spec != nil && spec.GetInterIterationInterval() == nil {
			spec.InterIterationInterval = durationpb.New(DefaultTransferInterval) // NON_EMPTY_DEFAULT
		}
		if v := s.validateV1(spec); v.err() != nil {
			return nil, v.err()
		}
		rec := &TransferRecord{API: TransferAPIV1, V1: &storagepb.Transfer{Metadata: md, Spec: spec}}
		return rec, s.checkBuckets(ctx, rec)
	})
}

// CreateV1Alpha1 is CreateV1 for the deprecated storage.v1alpha1 API.
func (s *TransferService) CreateV1Alpha1(ctx context.Context, cmd CreateTransferV1Alpha1Cmd) (*operations.Operation, error) {
	return s.create(ctx, cmd.Request, func(ctx context.Context, id string) (*TransferRecord, error) {
		md, err := s.newMetadata(ctx, id, cmd.Metadata)
		if err != nil {
			return nil, err
		}
		spec := proto.CloneOf(cmd.Spec)
		if spec != nil && spec.GetInterIterationInterval() == nil {
			spec.InterIterationInterval = durationpb.New(DefaultTransferInterval) // NON_EMPTY_DEFAULT
		}
		if v := s.validateAlpha(spec); v.err() != nil {
			return nil, v.err()
		}
		rec := &TransferRecord{API: TransferAPIV1Alpha1, Alpha: &storagev1alpha1pb.Transfer{Metadata: md, Spec: spec}}
		return rec, s.checkBuckets(ctx, rec)
	})
}

func (s *TransferService) create(ctx context.Context, req proto.Message, build func(context.Context, string) (*TransferRecord, error)) (*operations.Operation, error) {
	id, err := s.ids.New(KindTransfer)
	if err != nil {
		return nil, fmt.Errorf("new transfer id: %w", err)
	}
	var rec *TransferRecord
	return s.ops.Submit(ctx, operations.Spec{
		ResourceID:  id,
		Description: "Create transfer",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     req,
		Prepare: func(ctx context.Context) error {
			var err error
			rec, err = build(ctx, id)
			return err
		},
		Work: func(ctx context.Context) error {
			s.mu.Lock()
			defer s.mu.Unlock()
			rec.Status = TransferStatus{State: TransferActive}
			rec.NextRun = s.clk.Now()
			if err := s.repo.Create(ctx, rec); err != nil {
				return err
			}
			s.notify()
			return nil
		},
	})
}

// newMetadata checks the metadata of a new transfer: a project parent and a
// name that is unique in it across both API versions.
func (s *TransferService) newMetadata(ctx context.Context, id string, md *commonpb.ResourceMetadata) (*commonpb.ResourceMetadata, error) {
	if md.GetId() != "" {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.id", Message: "must not be set on create"})
	}
	parentID := md.GetParentId()
	switch {
	case parentID == "":
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.parent_id", Message: "required"})
	case kindOf(parentID) != KindProject:
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.parent_id", Message: "must be a project ID"})
	}
	if _, err := s.parents.Parent(ctx, parentID); err != nil {
		return nil, fmt.Errorf("transfer parent %s: %w", parentID, err)
	}
	if md.GetName() == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.name", Message: "required"})
	}
	if err := s.checkNameFree(ctx, parentID, md.GetName(), ""); err != nil {
		return nil, err
	}
	now := timestamppb.New(s.clk.Now())
	return &commonpb.ResourceMetadata{
		Id: id, ParentId: parentID, Name: md.GetName(), Labels: maps.Clone(md.GetLabels()),
		ResourceVersion: 1, CreatedAt: now, UpdatedAt: now,
	}, nil
}

// checkNameFree fails with errs.AlreadyExists when another transfer than
// selfID has name in the project.
func (s *TransferService) checkNameFree(ctx context.Context, parentID, name, selfID string) error {
	existing, err := s.repo.GetByName(ctx, parentID, name)
	switch {
	case errs.IsKind(err, errs.KindNotFound):
		return nil
	case err != nil:
		return err
	case existing.Metadata().GetId() == selfID:
		return nil
	default:
		return errs.AlreadyExists(existing.Metadata().GetId(), fmt.Sprintf("a transfer named %q already exists in the project", name))
	}
}

// UpdateV1 applies a full-replace update with reset-mask semantics to a
// transfer created through storage.v1.
func (s *TransferService) UpdateV1(ctx context.Context, cmd UpdateTransferV1Cmd) (*operations.Operation, error) {
	return s.update(ctx, cmd.Metadata.GetId(), cmd.Request, func(ctx context.Context) (*TransferRecord, error) {
		return s.prepareUpdateV1(ctx, cmd)
	})
}

// UpdateV1Alpha1 is UpdateV1 for transfers created through storage.v1alpha1.
func (s *TransferService) UpdateV1Alpha1(ctx context.Context, cmd UpdateTransferV1Alpha1Cmd) (*operations.Operation, error) {
	return s.update(ctx, cmd.Metadata.GetId(), cmd.Request, func(ctx context.Context) (*TransferRecord, error) {
		return s.prepareUpdateAlpha(ctx, cmd)
	})
}

func (s *TransferService) update(ctx context.Context, id string, req proto.Message, prepare func(context.Context) (*TransferRecord, error)) (*operations.Operation, error) {
	if id == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.id", Message: "required"})
	}
	var updated *TransferRecord // nil: nothing changed
	return s.ops.Submit(ctx, operations.Spec{
		ResourceID:  id,
		Description: "Update transfer",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     req,
		Prepare: func(ctx context.Context) error {
			var err error
			updated, err = prepare(ctx)
			return err
		},
		Work: func(ctx context.Context) error {
			if updated == nil {
				return nil
			}
			return s.applyUpdate(ctx, id, updated)
		},
	})
}

// applyUpdate stores the new resource of updated on the current record
// (whose runner state may have moved since Prepare) and reschedules a
// waiting ACTIVE transfer for the new interval.
func (s *TransferService) applyUpdate(ctx context.Context, id string, updated *TransferRecord) error {
	err := s.mutate(ctx, id, func(rec *TransferRecord, ri *runningIteration) error {
		rec.V1, rec.Alpha = updated.V1, updated.Alpha
		if rec.Status.State != TransferActive || ri != nil {
			return nil
		}
		plan, err := s.PlanOf(rec)
		if err != nil {
			return err
		}
		rec.NextRun = s.clk.Now()
		if last := rec.Status.LastIteration; last != nil && !last.End.IsZero() {
			rec.NextRun = last.End.Add(plan.Interval)
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.notify()
	return nil
}

// storedForUpdate loads a transfer for an update through api, which must be
// the API it was created with.
func (s *TransferService) storedForUpdate(ctx context.Context, id string, api TransferAPI) (*TransferRecord, error) {
	rec, err := s.visible(ctx, id, api)
	if err != nil {
		return nil, err
	}
	if rec.API != api {
		return nil, errs.BadState(id, "the transfer was created through the storage.v1alpha1 API; NebiusMocker updates it only through storage.v1alpha1.TransferService")
	}
	return rec, nil
}

// patchMetadata checks the metadata of an update and returns the stored
// metadata with the new name; labels come from the patched resource.
func (s *TransferService) patchMetadata(ctx context.Context, stored, md *commonpb.ResourceMetadata) (*commonpb.ResourceMetadata, error) {
	if p := md.GetParentId(); p != "" && p != stored.GetParentId() {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.parent_id", Message: "immutable"})
	}
	if v := md.GetResourceVersion(); v != 0 && v != stored.GetResourceVersion() {
		return nil, errs.Conflict(stored.GetId(), fmt.Sprintf("resource_version %d does not match current version %d", v, stored.GetResourceVersion()))
	}
	out := proto.CloneOf(stored)
	if n := md.GetName(); n != "" && n != stored.GetName() {
		if err := s.checkNameFree(ctx, stored.GetParentId(), n, stored.GetId()); err != nil {
			return nil, err
		}
		out.Name = n
	}
	return out, nil
}

func (s *TransferService) prepareUpdateV1(ctx context.Context, cmd UpdateTransferV1Cmd) (*TransferRecord, error) {
	stored, err := s.storedForUpdate(ctx, cmd.Metadata.GetId(), TransferAPIV1)
	if err != nil {
		return nil, err
	}
	md, err := s.patchMetadata(ctx, stored.V1.GetMetadata(), cmd.Metadata)
	if err != nil {
		return nil, err
	}
	updated := proto.CloneOf(stored.V1)
	patch := &storagepb.Transfer{Metadata: &commonpb.ResourceMetadata{Labels: cmd.Metadata.GetLabels()}, Spec: cmd.Spec}
	if err := fieldmaskpb.PatchWithResetMask(updated, patch, resetMaskOrEmpty(cmd.ResetMask)); err != nil {
		return nil, errs.BadRequest(errs.Violation{Field: "X-ResetMask", Message: err.Error()})
	}
	md.Labels = updated.GetMetadata().GetLabels()
	updated.Metadata = md
	if updated.GetSpec() == nil {
		updated.Spec = &storagepb.TransferSpec{}
	}
	if updated.GetSpec().GetInterIterationInterval() == nil {
		updated.Spec.InterIterationInterval = durationpb.New(DefaultTransferInterval)
	}
	keepV1Secrets(stored.V1.GetSpec(), updated.GetSpec())
	v := s.validateV1(updated.GetSpec())
	v.immutable(v1Immutables(stored.V1.GetSpec()), v1Immutables(updated.GetSpec()))
	if err := v.err(); err != nil {
		return nil, err
	}
	if proto.Equal(updated, stored.V1) {
		return nil, nil //nolint:nilnil // nil record: nothing to change
	}
	updated.Metadata.ResourceVersion++
	updated.Metadata.UpdatedAt = timestamppb.New(s.clk.Now())
	return &TransferRecord{API: TransferAPIV1, V1: updated}, nil
}

func (s *TransferService) prepareUpdateAlpha(ctx context.Context, cmd UpdateTransferV1Alpha1Cmd) (*TransferRecord, error) {
	stored, err := s.storedForUpdate(ctx, cmd.Metadata.GetId(), TransferAPIV1Alpha1)
	if err != nil {
		return nil, err
	}
	md, err := s.patchMetadata(ctx, stored.Alpha.GetMetadata(), cmd.Metadata)
	if err != nil {
		return nil, err
	}
	updated := proto.CloneOf(stored.Alpha)
	patch := &storagev1alpha1pb.Transfer{Metadata: &commonpb.ResourceMetadata{Labels: cmd.Metadata.GetLabels()}, Spec: cmd.Spec}
	if err := fieldmaskpb.PatchWithResetMask(updated, patch, resetMaskOrEmpty(cmd.ResetMask)); err != nil {
		return nil, errs.BadRequest(errs.Violation{Field: "X-ResetMask", Message: err.Error()})
	}
	md.Labels = updated.GetMetadata().GetLabels()
	updated.Metadata = md
	if updated.GetSpec() == nil {
		updated.Spec = &storagev1alpha1pb.TransferSpec{}
	}
	if updated.GetSpec().GetInterIterationInterval() == nil {
		updated.Spec.InterIterationInterval = durationpb.New(DefaultTransferInterval)
	}
	keepAlphaSecrets(stored.Alpha.GetSpec(), updated.GetSpec())
	v := s.validateAlpha(updated.GetSpec())
	v.immutable(alphaImmutables(stored.Alpha.GetSpec()), alphaImmutables(updated.GetSpec()))
	if err := v.err(); err != nil {
		return nil, err
	}
	if proto.Equal(updated, stored.Alpha) {
		return nil, nil //nolint:nilnil // nil record: nothing to change
	}
	updated.Metadata.ResourceVersion++
	updated.Metadata.UpdatedAt = timestamppb.New(s.clk.Now())
	return &TransferRecord{API: TransferAPIV1Alpha1, Alpha: updated}, nil
}

func resetMaskOrEmpty(m *mask.Mask) *mask.Mask {
	if m == nil {
		return mask.New()
	}
	return m
}

// Get returns a transfer without its secrets. The v1 API sees every
// transfer; the v1alpha1 API only those created through it.
func (s *TransferService) Get(ctx context.Context, id string, api TransferAPI) (*TransferRecord, error) {
	rec, err := s.visible(ctx, id, api)
	if err != nil {
		return nil, err
	}
	return redact(rec), nil
}

// GetByName returns a transfer of a project by name, without its secrets.
func (s *TransferService) GetByName(ctx context.Context, parentID, name string, api TransferAPI) (*TransferRecord, error) {
	rec, err := s.repo.GetByName(ctx, parentID, name)
	if err != nil {
		return nil, err
	}
	if !visibleTo(rec, api) {
		return nil, errs.NotFound(name)
	}
	return redact(rec), nil
}

// List returns one page of a project's transfers visible to api, in creation
// order and without secrets.
func (s *TransferService) List(ctx context.Context, parentID string, api TransferAPI, pageSize int64, pageToken string) (TransferPage, error) {
	size, err := pageSizeOf(pageSize, defaultTransferPageSize)
	if err != nil {
		return TransferPage{}, err
	}
	var only TransferAPI // v1 lists every transfer
	if api == TransferAPIV1Alpha1 {
		only = TransferAPIV1Alpha1
	}
	page, err := s.repo.List(ctx, parentID, only, size, pageToken)
	if err != nil {
		return TransferPage{}, err
	}
	for i, rec := range page.Items {
		page.Items[i] = redact(rec)
	}
	return page, nil
}

// History returns one page of a transfer's iterations, newest first.
func (s *TransferService) History(ctx context.Context, id string, api TransferAPI, pageSize int64, pageToken string) (IterationPage, error) {
	size, err := pageSizeOf(pageSize, defaultPageSize)
	if err != nil {
		return IterationPage{}, err
	}
	if _, err := s.visible(ctx, id, api); err != nil {
		return IterationPage{}, err
	}
	return s.repo.History(ctx, id, size, pageToken)
}

func pageSizeOf(pageSize int64, def int) (int, error) {
	if pageSize < 0 || pageSize > maxPageSize {
		return 0, errs.BadRequest(errs.Violation{Field: "page_size", Message: fmt.Sprintf("must be between 0 and %d", maxPageSize)})
	}
	if pageSize == 0 {
		return def, nil
	}
	return int(pageSize), nil
}

// Stop stops an ACTIVE transfer: at once when it waits between iterations,
// otherwise through STOPPING once the running iteration was interrupted.
func (s *TransferService) Stop(ctx context.Context, id string, api TransferAPI, req proto.Message) (*operations.Operation, error) {
	return s.ops.Submit(ctx, operations.Spec{
		ResourceID:  id,
		Description: "Stop transfer",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     req,
		Prepare: func(ctx context.Context) error {
			rec, err := s.visible(ctx, id, api)
			if err != nil {
				return err
			}
			if rec.Status.State != TransferActive {
				return errs.BadState(id, "only ACTIVE transfers can be stopped")
			}
			return nil
		},
		Work: func(ctx context.Context) error {
			var ri *runningIteration
			err := s.mutate(ctx, id, func(rec *TransferRecord, cur *runningIteration) error {
				if rec.Status.State != TransferActive {
					return errUnchanged // stopped or failed by its iteration meanwhile
				}
				if cur == nil {
					rec.Status.State = TransferStopped
					return nil
				}
				rec.Status.State = TransferStopping
				ri = cur
				return nil
			})
			if err != nil || ri == nil {
				return err
			}
			return interrupt(ctx, ri)
		},
	})
}

// Resume restarts a STOPPED or FAILED transfer with a new iteration.
func (s *TransferService) Resume(ctx context.Context, id string, api TransferAPI, req proto.Message) (*operations.Operation, error) {
	return s.ops.Submit(ctx, operations.Spec{
		ResourceID:  id,
		Description: "Resume transfer",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     req,
		Prepare: func(ctx context.Context) error {
			rec, err := s.visible(ctx, id, api)
			if err != nil {
				return err
			}
			if st := rec.Status.State; st != TransferStopped && st != TransferFailed {
				return errs.BadState(id, "only STOPPED or FAILED transfers can be resumed")
			}
			return nil
		},
		Work: func(ctx context.Context) error {
			err := s.mutate(ctx, id, func(rec *TransferRecord, _ *runningIteration) error {
				rec.Status.State = TransferActive
				rec.Status.Error = nil
				rec.EmptyStreak = 0
				rec.NextRun = s.clk.Now()
				return nil
			})
			if err != nil {
				return err
			}
			s.notify()
			return nil
		},
	})
}

// Delete removes a transfer and its history, interrupting a running
// iteration first. Through v1alpha1 only STOPPED or FAILED transfers can be
// deleted ("Only stopped or failed transfers can be deleted").
func (s *TransferService) Delete(ctx context.Context, id string, resourceVersion int64, api TransferAPI, req proto.Message) (*operations.Operation, error) {
	return s.ops.Submit(ctx, operations.Spec{
		ResourceID:  id,
		Description: "Delete transfer",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     req,
		Prepare: func(ctx context.Context) error {
			rec, err := s.visible(ctx, id, api)
			if err != nil {
				return err
			}
			if v := rec.Metadata().GetResourceVersion(); resourceVersion != 0 && resourceVersion != v {
				return errs.BadState(id, fmt.Sprintf("resource_version %d does not match current version %d", resourceVersion, v))
			}
			if st := rec.Status.State; api == TransferAPIV1Alpha1 && st != TransferStopped && st != TransferFailed {
				return errs.BadState(id, "only STOPPED or FAILED transfers can be deleted through storage.v1alpha1; stop the transfer first")
			}
			return nil
		},
		Work: func(ctx context.Context) error {
			var ri *runningIteration
			err := s.mutate(ctx, id, func(rec *TransferRecord, cur *runningIteration) error {
				rec.Status.State = TransferDeleting
				ri = cur
				return nil
			})
			if err != nil {
				return err
			}
			if ri != nil {
				if err := interrupt(ctx, ri); err != nil {
					return err
				}
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			return s.repo.Delete(ctx, id)
		},
	})
}

// interrupt cancels a running iteration and waits until its result is stored.
func interrupt(ctx context.Context, ri *runningIteration) error {
	ri.cancel()
	select {
	case <-ri.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("wait for the running iteration: %w", context.Cause(ctx))
	}
}

// errUnchanged makes mutate skip the write.
var errUnchanged = errors.New("unchanged")

// mutate applies fn to the current record under mu and stores it. ri is the
// running iteration of the transfer, nil when it waits.
func (s *TransferService) mutate(ctx context.Context, id string, fn func(rec *TransferRecord, ri *runningIteration) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.repo.Get(ctx, id)
	if err != nil {
		return err
	}
	if err := fn(rec, s.runningOf(id)); err != nil {
		if errors.Is(err, errUnchanged) {
			return nil
		}
		return err
	}
	return s.repo.Update(ctx, rec)
}

func (s *TransferService) runningOf(id string) *runningIteration {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	return s.running[id]
}

// notify wakes the runner without blocking.
func (s *TransferService) notify() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// visible loads a transfer that api may see.
func (s *TransferService) visible(ctx context.Context, id string, api TransferAPI) (*TransferRecord, error) {
	rec, err := s.repo.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if !visibleTo(rec, api) {
		return nil, errs.NotFound(id)
	}
	return rec, nil
}

// visibleTo: "All existing transfers remain accessible through the v1 API";
// the deprecated v1alpha1 API sees only the transfers created through it.
func visibleTo(rec *TransferRecord, api TransferAPI) bool {
	return api == TransferAPIV1 || rec.API == api
}

// redact returns a deep copy of rec without its INPUT_ONLY secrets: the v1
// secret keys are cleared, the v1alpha1 credentials removed entirely.
func redact(rec *TransferRecord) *TransferRecord {
	out := cloneTransferRecord(rec)
	if spec := out.V1.GetSpec(); spec != nil {
		for _, ak := range v1AccessKeys(spec) {
			ak.SecretAccessKey = ""
		}
		if az := spec.GetSource().GetAzureBlobStorage().GetAzureStorageAccount(); az != nil {
			az.AccessKey = ""
		}
	}
	if spec := out.Alpha.GetSpec(); spec != nil {
		out.AlphaSourceKind = ProviderS3Compatible
		if spec.GetSource().GetCredentials().GetAzureAccessKey() != nil {
			out.AlphaSourceKind = ProviderAzureBlob
		}
		if spec.GetSource() != nil {
			spec.Source.Credentials = nil
		}
		if spec.GetDestination() != nil {
			spec.Destination.Credentials = nil
		}
	}
	return out
}

// v1AccessKeys returns every access-key credential of a v1 spec.
func v1AccessKeys(spec *storagepb.TransferSpec) []*storagepb.TransferCredentialsAccessKey {
	var out []*storagepb.TransferCredentialsAccessKey
	for _, ak := range []*storagepb.TransferCredentialsAccessKey{
		spec.GetSource().GetNebius().GetAccessKey(),
		spec.GetSource().GetS3Compatible().GetAccessKey(),
		spec.GetDestination().GetNebius().GetAccessKey(),
		spec.GetDestination().GetS3Compatible().GetAccessKey(),
	} {
		if ak != nil {
			out = append(out, ak)
		}
	}
	return out
}

func cloneTransferRecord(rec *TransferRecord) *TransferRecord {
	out := *rec
	if rec.V1 != nil {
		out.V1 = proto.CloneOf(rec.V1)
	}
	if rec.Alpha != nil {
		out.Alpha = proto.CloneOf(rec.Alpha)
	}
	out.Status.Error = cloneTransferError(rec.Status.Error)
	if it := rec.Status.LastIteration; it != nil {
		c := *it
		c.Error = cloneTransferError(it.Error)
		out.Status.LastIteration = &c
	}
	return &out
}

func cloneTransferError(e *TransferError) *TransferError {
	if e == nil {
		return nil
	}
	c := *e
	return &c
}
