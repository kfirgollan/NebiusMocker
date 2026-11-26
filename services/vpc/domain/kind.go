package domain

import (
	"context"
	"fmt"
	"maps"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/nebius/gosdk/proto/fieldmask/mask"
	fieldmaskpb "github.com/nebius/gosdk/proto/fieldmask/protobuf"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/pkg/reqctx"
)

const (
	defaultPageSize = 100
	maxPageSize     = 1000
)

// CreateCmd is the input of a Create RPC.
type CreateCmd[S proto.Message] struct {
	Metadata *commonpb.ResourceMetadata
	Spec     S
	// Request is echoed in Operation.request.
	Request proto.Message
}

// UpdateCmd is the input of an Update RPC (full replace guarded by a reset mask).
type UpdateCmd[S proto.Message] struct {
	Metadata *commonpb.ResourceMetadata
	Spec     S
	// ResetMask lists fields to clear when absent from the request (X-ResetMask).
	ResetMask *mask.Mask
	Request   proto.Message
}

// kind is the generic lifecycle of one VPC resource kind: metadata, reset-mask
// updates, operations and the shared IPAM lock. Kind-specific rules plug in
// through the hook functions, which run with core.mu held.
type kind[T Resource, S proto.Message] struct {
	c       *core
	prefix  string // NID type
	what    string // human name, e.g. "subnet"
	repo    Repository[T]
	newMsg  func() T
	newSpec func() S

	// build validates a create request and returns the resource with its status.
	build func(ctx context.Context, md *commonpb.ResourceMetadata, spec S) (T, error)
	// afterCreate, if set, runs after the resource is stored (dependent defaults).
	afterCreate func(ctx context.Context, r T) error
	// apply validates and completes an updated copy (immutables, status).
	apply func(ctx context.Context, stored, updated T) error
	// checkDelete refuses deletion while the resource is referenced.
	checkDelete func(ctx context.Context, r T) error
	// remove, if set, replaces repo.Delete (cascades to children).
	remove func(ctx context.Context, r T) error
	// decorate, if set, fills live status fields on reads.
	decorate func(ctx context.Context, r T) error
	// quota, if set, names the project a resource is charged to and what it
	// consumes. User creates are admitted against it; usage counts it.
	quota func(ctx context.Context, r T) (project string, amounts map[string]int64, err error)
}

// Get returns a resource by ID.
func (k *kind[T, S]) Get(ctx context.Context, id string) (T, error) {
	r, err := k.repo.Get(ctx, id)
	if err != nil {
		var zero T
		return zero, err
	}
	return r, k.decorated(ctx, r)
}

// GetByName returns a resource by parent and name.
func (k *kind[T, S]) GetByName(ctx context.Context, parentID, name string) (T, error) {
	r, err := k.repo.GetByName(ctx, parentID, name)
	if err != nil {
		var zero T
		return zero, err
	}
	return r, k.decorated(ctx, r)
}

// List returns one page of the resources under parentID.
func (k *kind[T, S]) List(ctx context.Context, parentID string, pageSize int64, pageToken string) (Page[T], error) {
	size, err := pageSizeOf(pageSize)
	if err != nil {
		return Page[T]{}, err
	}
	page, err := k.repo.List(ctx, parentID, size, pageToken)
	if err != nil {
		return Page[T]{}, err
	}
	return page, k.decorateAll(ctx, page.Items)
}

// listWhere returns one page of the resources (any parent) matching match.
func (k *kind[T, S]) listWhere(ctx context.Context, match func(T) bool, pageSize int64, pageToken string) (Page[T], error) {
	size, err := pageSizeOf(pageSize)
	if err != nil {
		return Page[T]{}, err
	}
	page, err := k.repo.ListWhere(ctx, match, size, pageToken)
	if err != nil {
		return Page[T]{}, err
	}
	return page, k.decorateAll(ctx, page.Items)
}

func (k *kind[T, S]) decorated(ctx context.Context, r T) error {
	if k.decorate == nil {
		return nil
	}
	return k.decorate(ctx, r)
}

func (k *kind[T, S]) decorateAll(ctx context.Context, rs []T) error {
	for _, r := range rs {
		if err := k.decorated(ctx, r); err != nil {
			return err
		}
	}
	return nil
}

func pageSizeOf(pageSize int64) (int, error) {
	if pageSize < 0 || pageSize > maxPageSize {
		return 0, errs.BadRequest(errs.Violation{Field: "page_size", Message: fmt.Sprintf("must be between 0 and %d", maxPageSize)})
	}
	if pageSize == 0 {
		return defaultPageSize, nil
	}
	return int(pageSize), nil
}

// Create validates the request, stores the resource and returns its operation.
func (k *kind[T, S]) Create(ctx context.Context, cmd CreateCmd[S]) (*operations.Operation, error) {
	md, err := k.newMetadata(cmd.Metadata)
	if err != nil {
		return nil, err
	}
	spec := k.specOrEmpty(cmd.Spec)
	return k.c.ops.Submit(ctx, operations.Spec{
		ResourceID:  md.GetId(),
		Description: "Create " + k.what,
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     cmd.Request,
		Prepare: func(ctx context.Context) error {
			k.c.mu.Lock()
			defer k.c.mu.Unlock()
			_, err := k.createCharged(ctx, md, spec)
			return err
		},
	})
}

// createLocked builds and stores a system-created resource (defaults, seeds);
// core.mu must be held. It is not admitted against quotas: such resources
// count in usage but never fail the request that brings them along.
func (k *kind[T, S]) createLocked(ctx context.Context, md *commonpb.ResourceMetadata, spec S) (T, error) {
	return k.create(ctx, md, spec, func(ctx context.Context, _ T, store func(ctx context.Context) error) error {
		return store(ctx)
	})
}

// createCharged is createLocked for a user request: the store write runs
// inside the quota gate, which refuses it when the project's quota is full.
func (k *kind[T, S]) createCharged(ctx context.Context, md *commonpb.ResourceMetadata, spec S) (T, error) {
	return k.create(ctx, md, spec, k.admit)
}

func (k *kind[T, S]) create(ctx context.Context, md *commonpb.ResourceMetadata, spec S,
	admit func(ctx context.Context, r T, store func(ctx context.Context) error) error,
) (T, error) {
	var zero T
	r, err := k.build(ctx, md, spec)
	if err != nil {
		return zero, err
	}
	store := func(ctx context.Context) error { return k.repo.Create(ctx, r) }
	if err := admit(ctx, r, store); err != nil {
		return zero, err
	}
	if k.afterCreate != nil {
		if err := k.afterCreate(ctx, r); err != nil {
			_ = k.repo.Delete(ctx, md.GetId()) // roll back; the hook error is what matters
			return zero, err
		}
	}
	return r, nil
}

// newMetadata validates create metadata and assigns a fresh ID.
func (k *kind[T, S]) newMetadata(in *commonpb.ResourceMetadata) (*commonpb.ResourceMetadata, error) {
	if in.GetId() != "" {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.id", Message: "must not be set on create"})
	}
	if in.GetParentId() == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.parent_id", Message: "required"})
	}
	id, err := k.c.ids.New(k.prefix)
	if err != nil {
		return nil, fmt.Errorf("new %s id: %w", k.what, err)
	}
	return k.c.metadata(id, in.GetParentId(), in.GetName(), in.GetLabels()), nil
}

func (c *core) metadata(id, parentID, name string, labels map[string]string) *commonpb.ResourceMetadata {
	now := timestamppb.New(c.clk.Now())
	return &commonpb.ResourceMetadata{
		Id: id, ParentId: parentID, Name: name, Labels: maps.Clone(labels),
		ResourceVersion: 1, CreatedAt: now, UpdatedAt: now,
	}
}

func (k *kind[T, S]) specOrEmpty(spec S) S {
	if !spec.ProtoReflect().IsValid() {
		return k.newSpec()
	}
	return proto.CloneOf(spec)
}

// Update applies a full-replace update with reset-mask semantics.
func (k *kind[T, S]) Update(ctx context.Context, cmd UpdateCmd[S]) (*operations.Operation, error) {
	id := cmd.Metadata.GetId()
	if id == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.id", Message: "required"})
	}
	return k.c.ops.Submit(ctx, operations.Spec{
		ResourceID:  id,
		Description: "Update " + k.what,
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     cmd.Request,
		Prepare: func(ctx context.Context) error {
			k.c.mu.Lock()
			defer k.c.mu.Unlock()
			return k.applyUpdate(ctx, cmd)
		},
	})
}

func (k *kind[T, S]) applyUpdate(ctx context.Context, cmd UpdateCmd[S]) error {
	stored, err := k.repo.Get(ctx, cmd.Metadata.GetId())
	if err != nil {
		return err
	}
	if err := checkUpdatable(stored.GetMetadata(), cmd.Metadata); err != nil {
		return err
	}
	updated, err := k.patch(stored, cmd)
	if err != nil {
		return err
	}
	if err := k.apply(ctx, stored, updated); err != nil {
		return err
	}
	if proto.Equal(updated, stored) {
		return nil // nothing changed: keep resource_version and updated_at
	}
	updated.GetMetadata().ResourceVersion = stored.GetMetadata().GetResourceVersion() + 1
	updated.GetMetadata().UpdatedAt = timestamppb.New(k.c.clk.Now())
	return k.repo.Update(ctx, updated)
}

// checkUpdatable enforces immutable parent_id and optimistic concurrency.
func checkUpdatable(stored, md *commonpb.ResourceMetadata) error {
	if p := md.GetParentId(); p != "" && p != stored.GetParentId() {
		return errs.BadRequest(errs.Violation{Field: "metadata.parent_id", Message: "field is immutable"})
	}
	if v := md.GetResourceVersion(); v != 0 && v != stored.GetResourceVersion() {
		return errs.Conflict(stored.GetId(), fmt.Sprintf(
			"resource_version %d does not match current version %d", v, stored.GetResourceVersion(),
		))
	}
	return nil
}

// patch applies the request to a copy of stored using the reset mask, then
// restores every server-owned field (identity, versions, timestamps, status).
func (k *kind[T, S]) patch(stored T, cmd UpdateCmd[S]) (T, error) {
	var zero T
	p := k.newMsg()
	pr := p.ProtoReflect()
	fields := pr.Descriptor().Fields()
	md := &commonpb.ResourceMetadata{Name: cmd.Metadata.GetName(), Labels: cmd.Metadata.GetLabels()}
	pr.Set(fields.ByName("metadata"), protoreflect.ValueOfMessage(md.ProtoReflect()))
	if cmd.Spec.ProtoReflect().IsValid() {
		pr.Set(fields.ByName("spec"), protoreflect.ValueOfMessage(proto.CloneOf(cmd.Spec).ProtoReflect()))
	}
	resetMask := cmd.ResetMask
	if resetMask == nil {
		resetMask = mask.New()
	}
	updated := proto.CloneOf(stored)
	if err := fieldmaskpb.PatchWithResetMask(updated, p, resetMask); err != nil {
		return zero, errs.BadRequest(errs.Violation{Field: "X-ResetMask", Message: err.Error()})
	}
	ur := updated.ProtoReflect()
	src := proto.CloneOf(stored).ProtoReflect()
	old := stored.GetMetadata()
	um := &commonpb.ResourceMetadata{
		Id: old.GetId(), ParentId: old.GetParentId(), Name: updated.GetMetadata().GetName(),
		Labels: updated.GetMetadata().GetLabels(), ResourceVersion: old.GetResourceVersion(),
		CreatedAt: old.GetCreatedAt(), UpdatedAt: old.GetUpdatedAt(),
	}
	ur.Set(fields.ByName("metadata"), protoreflect.ValueOfMessage(um.ProtoReflect()))
	if !ur.Has(fields.ByName("spec")) {
		ur.Set(fields.ByName("spec"), protoreflect.ValueOfMessage(k.newSpec().ProtoReflect()))
	}
	if status := fields.ByName("status"); src.Has(status) {
		ur.Set(status, src.Get(status))
	} else {
		ur.Clear(status)
	}
	return updated, nil
}

// Delete removes a resource when its operation completes. References are
// checked up front (so the RPC fails) and again when the work runs.
func (k *kind[T, S]) Delete(ctx context.Context, id string, request proto.Message) (*operations.Operation, error) {
	check := func(ctx context.Context) (T, error) {
		r, err := k.repo.Get(ctx, id)
		if err != nil {
			return r, err
		}
		if k.checkDelete != nil {
			if err := k.checkDelete(ctx, r); err != nil {
				return r, err
			}
		}
		return r, nil
	}
	return k.c.ops.Submit(ctx, operations.Spec{
		ResourceID:  id,
		Description: "Delete " + k.what,
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     request,
		Prepare: func(ctx context.Context) error {
			k.c.mu.Lock()
			defer k.c.mu.Unlock()
			_, err := check(ctx)
			return err
		},
		Work: func(ctx context.Context) error {
			k.c.mu.Lock()
			defer k.c.mu.Unlock()
			r, err := check(ctx)
			if err != nil {
				return err
			}
			return k.removeLocked(ctx, r)
		},
	})
}

func (k *kind[T, S]) removeLocked(ctx context.Context, r T) error {
	if k.remove != nil {
		return k.remove(ctx, r)
	}
	return k.repo.Delete(ctx, r.GetMetadata().GetId())
}

// inUse is the error for a delete refused because of references.
func inUse(id, what string, users []string) error {
	return errs.BadState(id, fmt.Sprintf("%s is in use by %s", what, strings.Join(users, ", ")))
}

func idsOf[T Resource](rs []T) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.GetMetadata().GetId())
	}
	return out
}

func immutable(field string) error {
	return errs.BadRequest(errs.Violation{Field: field, Message: "field is immutable"})
}

// admit runs store through the quota gate, charging what r consumes.
func (k *kind[T, S]) admit(ctx context.Context, r T, store func(ctx context.Context) error) error {
	if k.quota == nil {
		return store(ctx)
	}
	project, amounts, err := k.quota(ctx, r)
	if err != nil {
		return err
	}
	return k.c.gate.Admit(ctx, peer.QuotaClaim{ContainerID: project, Amounts: amounts}, store)
}
