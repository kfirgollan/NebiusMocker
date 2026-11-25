package domain

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/nebius/gosdk/proto/fieldmask/mask"
	fieldmaskpb "github.com/nebius/gosdk/proto/fieldmask/protobuf"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	kmspb "github.com/nebius/gosdk/proto/nebius/kms/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	"github.com/kfirgollan/NebiusMocker/pkg/reqctx"
)

const (
	defaultPageSize = 100
	maxPageSize     = 1000
	// DefaultDeletionDelay is how long a deleted key stays restorable before it
	// is purged. Nebius does not document the default; the quotas page says a
	// deleted key counts against quota for 30 days, which is the maximum delay
	// UpdateDeletionDelay accepts.
	DefaultDeletionDelay = 30 * 24 * time.Hour
	// MinDeletionDelay is the shortest delay UpdateDeletionDelay accepts.
	MinDeletionDelay = 24 * time.Hour
	// MaxDeletionDelay is the longest delay UpdateDeletionDelay accepts.
	MaxDeletionDelay = 30 * 24 * time.Hour
)

// Visibility selects whether lookups see keys scheduled for deletion
// (show_scheduled_for_deletion).
type Visibility int

const (
	// ActiveOnly hides keys scheduled for deletion (the API default).
	ActiveOnly Visibility = iota
	// IncludeScheduled also returns keys scheduled for deletion.
	IncludeScheduled
)

// keyMsg is implemented by *kmspb.SymmetricKey and *kmspb.AsymmetricKey.
type keyMsg interface {
	proto.Message
	GetMetadata() *commonpb.ResourceMetadata
}

// keyStatus is the lifecycle part shared by both key status messages.
type keyStatus struct {
	State     kmspb.KeyState
	DeletedAt *timestamppb.Timestamp
	PurgeAt   *timestamppb.Timestamp
}

// lifecycle implements the parts of key management both key types share:
// lookups that hide and purge deleted keys, parent validation, delayed
// deletion, deletion-delay updates and undelete.
type lifecycle[T keyMsg] struct {
	kind      string
	label     string // "symmetric key", for messages
	repo      KeyRepository[T]
	ops       Operations
	ids       IDGenerator
	hierarchy Hierarchy
	clk       clock.Clock
	status    func(T) keyStatus
	setStatus func(T, keyStatus)
	// catchUp applies the time-driven changes due by now to an ACTIVE key
	// (automatic rotation) and returns the current key. Like purging it runs
	// lazily on access, which on the injected clock is indistinguishable from
	// a background job. Nil for kinds without such changes.
	catchUp func(context.Context, T) (T, error)
	// restored runs after Undelete made a key ACTIVE again. Nil if unused.
	restored func(context.Context, T) error
}

// current returns k after applying the changes due by now (see catchUp).
func (l *lifecycle[T]) current(ctx context.Context, k T) (T, error) {
	if l.catchUp == nil || l.status(k).State != kmspb.KeyState_ACTIVE {
		return k, nil
	}
	return l.catchUp(ctx, k)
}

func (l *lifecycle[T]) validate() error {
	if l.repo == nil || l.ops == nil || l.ids == nil || l.hierarchy == nil || l.clk == nil {
		return errors.New("kms: key service dependencies must not be nil")
	}
	return nil
}

// load returns the key in any state, purging it first if its delay is over and
// applying the changes due by now to an ACTIVE key.
func (l *lifecycle[T]) load(ctx context.Context, id string) (T, error) {
	var zero T
	k, err := l.repo.Get(ctx, id)
	if err != nil {
		return zero, err
	}
	k, purged, err := l.settle(ctx, k)
	if err != nil {
		return zero, err
	}
	if purged {
		return zero, errs.NotFound(id)
	}
	return k, nil
}

// settle applies the time-driven changes due by now to k, as read from the
// repository: it purges a scheduled key whose deletion delay is over
// (purged = true), and catches up an ACTIVE key (see catchUp). Both are lazy
// (on access), which on the injected clock is indistinguishable from a
// background sweep. A purge that lost a race to another write re-reads the key.
func (l *lifecycle[T]) settle(ctx context.Context, k T) (_ T, purged bool, _ error) {
	var zero T
	id := k.GetMetadata().GetId()
	for range maxWriteAttempts {
		if !l.purgeDue(k) {
			cur, err := l.current(ctx, k)
			return cur, false, err
		}
		err := l.repo.Delete(ctx, k)
		switch {
		case err == nil || errs.IsKind(err, errs.KindNotFound):
			return zero, true, nil
		case !errs.IsKind(err, errs.KindConflict):
			return zero, false, fmt.Errorf("purge %s: %w", id, err)
		}
		if k, err = l.repo.Get(ctx, id); errs.IsKind(err, errs.KindNotFound) {
			return zero, true, nil
		} else if err != nil {
			return zero, false, err
		}
	}
	return zero, false, errConcurrent(id)
}

// purgeDue reports whether k is scheduled for deletion and its delay is over.
func (l *lifecycle[T]) purgeDue(k T) bool {
	st := l.status(k)
	return st.State == kmspb.KeyState_SCHEDULED_FOR_DELETION && st.PurgeAt != nil &&
		!l.clk.Now().Before(st.PurgeAt.AsTime())
}

// maxWriteAttempts bounds the compare-and-swap retries of a write that races
// with other writes to the same key.
const maxWriteAttempts = 8

// errConcurrent is returned when a write lost every compare-and-swap attempt.
func errConcurrent(id string) error {
	return errs.Conflict(id, "the key is being changed concurrently; retry")
}

// write stores a change to an existing key with compare-and-swap. read loads
// the key (applying due lazy changes such as rotation) and change derives its
// new state; change returns write = false when there is nothing to store.
// When another request changed the key in between, the key is read again and
// change re-applied. Errors from read and change (e.g. a client's stale
// resource_version) are returned as they are, without retrying.
func (l *lifecycle[T]) write(ctx context.Context, id string, read func(context.Context) (T, error), change func(cur T) (upd T, write bool, err error)) (T, error) {
	var zero T
	for range maxWriteAttempts {
		cur, err := read(ctx)
		if err != nil {
			return zero, err
		}
		upd, ok, err := change(cur)
		if err != nil {
			return zero, err
		}
		if !ok {
			return cur, nil
		}
		err = l.repo.Update(ctx, upd, cur)
		if err == nil {
			return upd, nil
		}
		if !errs.IsKind(err, errs.KindConflict) {
			return zero, err
		}
	}
	return zero, errConcurrent(id)
}

// Get returns a key; a key scheduled for deletion is NotFound unless vis is
// IncludeScheduled.
func (l *lifecycle[T]) Get(ctx context.Context, id string, vis Visibility) (T, error) {
	var zero T
	k, err := l.load(ctx, id)
	if err != nil {
		return zero, err
	}
	if vis == ActiveOnly && l.status(k).State != kmspb.KeyState_ACTIVE {
		return zero, errs.NotFound(id)
	}
	return k, nil
}

// GetByName returns the ACTIVE key with the name in parentID.
func (l *lifecycle[T]) GetByName(ctx context.Context, parentID, name string) (T, error) {
	k, err := l.repo.GetByName(ctx, parentID, name)
	if err != nil {
		var zero T
		return zero, err
	}
	return l.current(ctx, k)
}

// List returns one page of the parent's keys. Keys scheduled for deletion are
// left out unless vis is IncludeScheduled, so a page may hold fewer items than
// pageSize while more remain.
func (l *lifecycle[T]) List(ctx context.Context, parentID string, pageSize int64, pageToken string, vis Visibility) (Page[T], error) {
	if pageSize < 0 || pageSize > maxPageSize {
		return Page[T]{}, errs.BadRequest(errs.Violation{
			Field: "page_size", Message: fmt.Sprintf("must be between 0 and %d", maxPageSize),
		})
	}
	size := int(pageSize)
	if size == 0 {
		size = defaultPageSize
	}
	page, err := l.repo.List(ctx, parentID, size, pageToken)
	if err != nil {
		return Page[T]{}, err
	}
	items := make([]T, 0, len(page.Items))
	for _, k := range page.Items {
		k, purged, err := l.settle(ctx, k)
		if err != nil {
			return Page[T]{}, err
		}
		if purged || (vis == ActiveOnly && l.status(k).State != kmspb.KeyState_ACTIVE) {
			continue
		}
		items = append(items, k)
	}
	return Page[T]{Items: items, NextPageToken: page.NextPageToken}, nil
}

// usable returns an ACTIVE key for a cryptographic operation. A key scheduled
// for deletion fails with errs.BadState (FAILED_PRECONDITION).
func (l *lifecycle[T]) usable(ctx context.Context, id string) (T, error) {
	var zero T
	k, err := l.load(ctx, id)
	if err != nil {
		return zero, err
	}
	if st := l.status(k).State; st != kmspb.KeyState_ACTIVE {
		return zero, errs.BadState(id, fmt.Sprintf("%s is %s; restore it with Undelete to use it", l.label, st))
	}
	return k, nil
}

// newMetadata validates create metadata and returns the stored metadata of a
// new key.
func (l *lifecycle[T]) newMetadata(ctx context.Context, md *commonpb.ResourceMetadata) (*commonpb.ResourceMetadata, error) {
	if md.GetId() != "" {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.id", Message: "must not be set on create"})
	}
	parentID := md.GetParentId()
	if err := l.checkParent(ctx, parentID); err != nil {
		return nil, err
	}
	id, err := l.ids.New(l.kind)
	if err != nil {
		return nil, fmt.Errorf("new %s id: %w", l.label, err)
	}
	now := timestamppb.New(l.clk.Now())
	return &commonpb.ResourceMetadata{
		Id:              id,
		ParentId:        parentID,
		Name:            md.GetName(),
		Labels:          maps.Clone(md.GetLabels()),
		ResourceVersion: 1,
		CreatedAt:       now,
		UpdatedAt:       now,
	}, nil
}

// checkParent requires an existing project.
func (l *lifecycle[T]) checkParent(ctx context.Context, parentID string) error {
	if parentID == "" {
		return errs.BadRequest(errs.Violation{Field: "metadata.parent_id", Message: "required"})
	}
	if kind, _, _ := strings.Cut(parentID, "-"); kind != KindProject {
		return errs.BadRequest(errs.Violation{Field: "metadata.parent_id", Message: "must be a project"})
	}
	if _, err := l.hierarchy.Parent(ctx, parentID); err != nil {
		return fmt.Errorf("%s parent %s: %w", l.label, parentID, err)
	}
	return nil
}

// submit runs an operation on the key whose Prepare does the state change.
func (l *lifecycle[T]) submit(ctx context.Context, id, description string, request proto.Message, prepare func(context.Context) error) (*operations.Operation, error) {
	if id == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "id", Message: "required"})
	}
	return l.ops.Submit(ctx, operations.Spec{
		ResourceID:  id,
		Description: description,
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     request,
		Prepare:     prepare,
	})
}

// Delete schedules an ACTIVE key for deletion after DefaultDeletionDelay. The
// key stops being usable at once, and its name is freed.
func (l *lifecycle[T]) Delete(ctx context.Context, id string, request proto.Message) (*operations.Operation, error) {
	return l.submit(ctx, id, "Schedule "+l.label+" for deletion", request, func(ctx context.Context) error {
		_, err := l.write(ctx, id, l.activeReader(id), func(k T) (T, bool, error) {
			now := l.clk.Now()
			upd := proto.CloneOf(k)
			l.setStatus(upd, keyStatus{
				State:     kmspb.KeyState_SCHEDULED_FOR_DELETION,
				DeletedAt: timestamppb.New(now),
				PurgeAt:   timestamppb.New(now.Add(DefaultDeletionDelay)),
			})
			return upd, true, nil
		})
		return err
	})
}

// UpdateDeletionDelay moves the purge time of a scheduled key to now + delay.
func (l *lifecycle[T]) UpdateDeletionDelay(ctx context.Context, id string, delay time.Duration, request proto.Message) (*operations.Operation, error) {
	if delay < MinDeletionDelay || delay > MaxDeletionDelay {
		return nil, errs.BadRequest(errs.Violation{
			Field: "deletion_delay", Message: fmt.Sprintf("must be between %s and %s", MinDeletionDelay, MaxDeletionDelay),
		})
	}
	return l.submit(ctx, id, "Update "+l.label+" deletion delay", request, func(ctx context.Context) error {
		_, err := l.write(ctx, id, l.scheduledReader(id), func(k T) (T, bool, error) {
			upd := proto.CloneOf(k)
			st := l.status(k)
			st.PurgeAt = timestamppb.New(l.clk.Now().Add(delay))
			l.setStatus(upd, st)
			return upd, true, nil
		})
		return err
	})
}

// Undelete restores a scheduled key to ACTIVE. newName, when set, renames it
// (its old name may have been reused meanwhile).
func (l *lifecycle[T]) Undelete(ctx context.Context, id, newName string, request proto.Message) (*operations.Operation, error) {
	return l.submit(ctx, id, "Undelete "+l.label, request, func(ctx context.Context) error {
		upd, err := l.write(ctx, id, l.scheduledReader(id), func(k T) (T, bool, error) {
			upd := proto.CloneOf(k)
			if newName != "" && newName != upd.GetMetadata().GetName() {
				upd.GetMetadata().Name = newName
				upd.GetMetadata().ResourceVersion++
				upd.GetMetadata().UpdatedAt = timestamppb.New(l.clk.Now())
			}
			l.setStatus(upd, keyStatus{State: kmspb.KeyState_ACTIVE})
			return upd, true, nil
		})
		if err != nil {
			return err
		}
		if l.restored == nil {
			return nil
		}
		return l.restored(ctx, upd)
	})
}

// activeReader reads the ACTIVE key id for write.
func (l *lifecycle[T]) activeReader(id string) func(context.Context) (T, error) {
	return func(ctx context.Context) (T, error) { return l.Get(ctx, id, ActiveOnly) }
}

// scheduledReader reads the SCHEDULED_FOR_DELETION key id for write.
func (l *lifecycle[T]) scheduledReader(id string) func(context.Context) (T, error) {
	return func(ctx context.Context) (T, error) { return l.scheduled(ctx, id) }
}

// scheduled returns a key that must be SCHEDULED_FOR_DELETION.
func (l *lifecycle[T]) scheduled(ctx context.Context, id string) (T, error) {
	var zero T
	k, err := l.load(ctx, id)
	if err != nil {
		return zero, err
	}
	if st := l.status(k).State; st != kmspb.KeyState_SCHEDULED_FOR_DELETION {
		return zero, errs.BadState(id, fmt.Sprintf("%s is %s, not scheduled for deletion", l.label, st))
	}
	return k, nil
}

// update applies a full-replace update with reset-mask semantics to an ACTIVE
// key. patch carries the request's name, labels and spec. fixSpec validates the
// patched spec against the stored one (immutable fields, defaults).
func (l *lifecycle[T]) update(ctx context.Context, md *commonpb.ResourceMetadata, patch T, resetMask *mask.Mask, request proto.Message, fixSpec func(stored, updated T) error) (*operations.Operation, error) {
	id := md.GetId()
	if id == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.id", Message: "required"})
	}
	if resetMask == nil {
		resetMask = mask.New()
	}
	change := func(stored T) (T, bool, error) {
		var zero T
		smd := stored.GetMetadata()
		if p := md.GetParentId(); p != "" && p != smd.GetParentId() {
			return zero, false, errs.BadRequest(errs.Violation{Field: "metadata.parent_id", Message: "field is immutable"})
		}
		if v := md.GetResourceVersion(); v != 0 && v != smd.GetResourceVersion() {
			return zero, false, errs.Conflict(id, fmt.Sprintf("resource_version %d does not match current version %d", v, smd.GetResourceVersion()))
		}
		updated := proto.CloneOf(stored)
		if err := fieldmaskpb.PatchWithResetMask(updated, patch, resetMask); err != nil {
			return zero, false, errs.BadRequest(errs.Violation{Field: "X-ResetMask", Message: err.Error()})
		}
		umd := updated.GetMetadata()
		if umd == nil {
			return zero, false, errs.BadRequest(errs.Violation{Field: "metadata", Message: "must not be reset"})
		}
		umd.Id, umd.ParentId = smd.GetId(), smd.GetParentId()
		umd.ResourceVersion, umd.CreatedAt, umd.UpdatedAt = smd.GetResourceVersion(), smd.GetCreatedAt(), smd.GetUpdatedAt()
		l.setStatus(updated, l.status(stored))
		if err := fixSpec(stored, updated); err != nil {
			return zero, false, err
		}
		if proto.Equal(updated, stored) {
			return stored, false, nil // nothing changed: keep resource_version and updated_at
		}
		umd.ResourceVersion++
		umd.UpdatedAt = timestamppb.New(l.clk.Now())
		return updated, true, nil
	}
	return l.ops.Submit(ctx, operations.Spec{
		ResourceID:  id,
		Description: "Update " + l.label,
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     request,
		Prepare: func(ctx context.Context) error {
			_, err := l.write(ctx, id, l.activeReader(id), change)
			return err
		},
	})
}
