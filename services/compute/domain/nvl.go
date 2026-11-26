package domain

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/nebius/gosdk/proto/fieldmask/mask"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	"github.com/kfirgollan/NebiusMocker/pkg/reqctx"
)

// NVLInstanceGroupDeps are the dependencies of NVLInstanceGroupService. All
// are required.
type NVLInstanceGroupDeps struct {
	Groups NVLInstanceGroupRepository
	// Instances fills in status.instances[*].instance_state on reads.
	Instances InstanceReader
	Projects  Projects
	Ops       Operations
	IDs       IDGenerator
	Clock     clock.Clock
}

func (d NVLInstanceGroupDeps) validate() error {
	if d.Groups == nil || d.Instances == nil || d.Projects == nil || d.Ops == nil || d.IDs == nil || d.Clock == nil {
		return errors.New("compute: nvl instance group service dependencies must not be nil")
	}
	return nil
}

// NVLInstanceGroupService implements nebius.compute.v1.NVLInstanceGroupService.
// It keeps the inventory only: no NVLink domain is simulated. Members are
// added and removed by InstanceService through NVLMembership.
type NVLInstanceGroupService struct{ d NVLInstanceGroupDeps }

// NewNVLInstanceGroupService wires an NVLInstanceGroupService.
func NewNVLInstanceGroupService(d NVLInstanceGroupDeps) (*NVLInstanceGroupService, error) {
	if err := d.validate(); err != nil {
		return nil, err
	}
	return &NVLInstanceGroupService{d: d}, nil
}

// CreateNVLInstanceGroupCmd is the input of NVLInstanceGroupService.Create.
type CreateNVLInstanceGroupCmd struct {
	Metadata *commonpb.ResourceMetadata
	Spec     *computepb.NVLInstanceGroupSpec
	Request  proto.Message // echoed in Operation.request
}

// UpdateNVLInstanceGroupCmd is the input of NVLInstanceGroupService.Update
// (full replace guarded by a reset mask).
type UpdateNVLInstanceGroupCmd struct {
	Metadata  *commonpb.ResourceMetadata
	Spec      *computepb.NVLInstanceGroupSpec
	ResetMask *mask.Mask
	Request   proto.Message
}

// Get returns an NVL instance group by ID.
func (s *NVLInstanceGroupService) Get(ctx context.Context, id string) (*computepb.NVLInstanceGroup, error) {
	if id == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "id", Message: "required"})
	}
	g, err := s.d.Groups.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.withMemberStates(ctx, g)
}

// GetByName returns an NVL instance group by its name within a project.
func (s *NVLInstanceGroupService) GetByName(ctx context.Context, parentID, name string) (*computepb.NVLInstanceGroup, error) {
	if parentID == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "parent_id", Message: "required"})
	}
	g, err := s.d.Groups.GetByName(ctx, parentID, name)
	if err != nil {
		return nil, err
	}
	return s.withMemberStates(ctx, g)
}

// List returns one page of a project's NVL instance groups in creation order.
func (s *NVLInstanceGroupService) List(ctx context.Context, parentID string, size int64, token string) (NVLInstanceGroupPage, error) {
	if parentID == "" {
		return NVLInstanceGroupPage{}, errs.BadRequest(errs.Violation{Field: "parent_id", Message: "required"})
	}
	n, err := pageSize(size)
	if err != nil {
		return NVLInstanceGroupPage{}, err
	}
	page, err := s.d.Groups.List(ctx, parentID, n, token)
	if err != nil {
		return NVLInstanceGroupPage{}, err
	}
	for i, g := range page.Items {
		if page.Items[i], err = s.withMemberStates(ctx, g); err != nil {
			return NVLInstanceGroupPage{}, err
		}
	}
	return page, nil
}

// Create validates the request and stores the group, reconciling until the
// operation completes.
func (s *NVLInstanceGroupService) Create(ctx context.Context, cmd CreateNVLInstanceGroupCmd) (*operations.Operation, error) {
	g, err := s.newGroup(ctx, cmd)
	if err != nil {
		return nil, err
	}
	id := g.GetMetadata().GetId()
	return s.d.Ops.Submit(ctx, operations.Spec{
		ResourceID:  id,
		Description: "Create NVL instance group",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     cmd.Request,
		Prepare:     func(ctx context.Context) error { return s.d.Groups.Create(ctx, g) },
		Work: func(ctx context.Context) error {
			_, err := s.d.Groups.Mutate(ctx, id, func(g *computepb.NVLInstanceGroup) error {
				g.Status.Reconciling = false
				return nil
			})
			return err
		},
	})
}

// Update applies a full-replace update with reset-mask semantics. Name and
// labels may change; spec.type and spec.size are immutable.
func (s *NVLInstanceGroupService) Update(ctx context.Context, cmd UpdateNVLInstanceGroupCmd) (*operations.Operation, error) {
	id := cmd.Metadata.GetId()
	if id == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.id", Message: "required"})
	}
	if _, err := s.d.Groups.Get(ctx, id); err != nil {
		return nil, err
	}
	return s.d.Ops.Submit(ctx, operations.Spec{
		ResourceID:  id,
		Description: "Update NVL instance group",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     cmd.Request,
		Prepare: func(ctx context.Context) error {
			_, err := s.d.Groups.Mutate(ctx, id, func(g *computepb.NVLInstanceGroup) error {
				updated, err := s.patchGroup(g, cmd)
				if err != nil {
					return err
				}
				if updated != g {
					proto.Reset(g)
					proto.Merge(g, updated)
				}
				return nil
			})
			return err
		},
		Work: func(context.Context) error { return nil },
	})
}

// Delete removes the group when the operation completes. It fails with
// FAILED_PRECONDITION while instances belong to it. The emptiness check and
// the deletion run in one store transaction, so an instance that joins
// meanwhile fails the operation instead of being orphaned.
func (s *NVLInstanceGroupService) Delete(ctx context.Context, id string, request proto.Message) (*operations.Operation, error) {
	if id == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "id", Message: "required"})
	}
	if _, err := s.d.Groups.Get(ctx, id); err != nil {
		return nil, err
	}
	return s.d.Ops.Submit(ctx, operations.Spec{
		ResourceID:  id,
		Description: "Delete NVL instance group",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     request,
		Prepare: func(ctx context.Context) error {
			g, err := s.d.Groups.Get(ctx, id)
			if err != nil {
				return err
			}
			return CheckNVLInstanceGroupDeletable(g)
		},
		Work: func(ctx context.Context) error { return s.d.Groups.DeleteIf(ctx, id, CheckNVLInstanceGroupDeletable) },
	})
}

// CheckNVLInstanceGroupDeletable reports why a group cannot be deleted, or nil.
func CheckNVLInstanceGroupDeletable(g *computepb.NVLInstanceGroup) error {
	if members := slices.Sorted(maps.Keys(g.GetStatus().GetInstances())); len(members) > 0 {
		return errs.BadState(g.GetMetadata().GetId(), fmt.Sprintf(
			"nvl instance group has instances (%s): delete them first", strings.Join(members, ", ")))
	}
	return nil
}

// withMemberStates returns a copy of g whose status.instances carry the
// members' current instance_state, and whose reconciling is also set while
// any member is in a transitional state.
func (s *NVLInstanceGroupService) withMemberStates(ctx context.Context, g *computepb.NVLInstanceGroup) (*computepb.NVLInstanceGroup, error) {
	out := proto.CloneOf(g)
	if out.GetStatus() == nil {
		out.Status = &computepb.NVLInstanceGroupStatus{}
	}
	st := out.GetStatus()
	for id := range st.GetInstances() {
		inst, err := s.d.Instances.Get(ctx, id)
		if errs.IsKind(err, errs.KindNotFound) {
			delete(st.GetInstances(), id) // removed since the group was read
			continue
		}
		if err != nil {
			return nil, err
		}
		st.GetInstances()[id] = &computepb.NVLInstanceGroupStatus_InstanceInfo{InstanceState: inst.GetStatus().GetState()}
		if inst.GetStatus().GetReconciling() {
			st.Reconciling = true
		}
	}
	return out, nil
}

// newGroup validates a Create request and builds the new group.
func (s *NVLInstanceGroupService) newGroup(ctx context.Context, cmd CreateNVLInstanceGroupCmd) (*computepb.NVLInstanceGroup, error) {
	md, spec := cmd.Metadata, cmd.Spec
	if md.GetId() != "" {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.id", Message: "must not be set on create"})
	}
	parentID := md.GetParentId()
	if parentID == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.parent_id", Message: "required"})
	}
	if _, err := s.d.Projects.Parent(ctx, parentID); err != nil {
		return nil, fmt.Errorf("nvl instance group parent %s: %w", parentID, err)
	}
	if err := checkNVLSpec(spec); err != nil {
		return nil, err
	}
	id, err := s.d.IDs.New(KindNVLInstanceGroup)
	if err != nil {
		return nil, fmt.Errorf("new nvl instance group id: %w", err)
	}
	now := timestamppb.New(s.d.Clock.Now())
	return &computepb.NVLInstanceGroup{
		Metadata: &commonpb.ResourceMetadata{
			Id: id, ParentId: parentID, Name: md.GetName(), Labels: maps.Clone(md.GetLabels()),
			ResourceVersion: 1, CreatedAt: now, UpdatedAt: now,
		},
		Spec:   proto.CloneOf(spec), // echoed as sent
		Status: &computepb.NVLInstanceGroupStatus{Reconciling: true},
	}, nil
}

// checkNVLSpec requires a known type (the group's platform family) and a
// positive size.
func checkNVLSpec(spec *computepb.NVLInstanceGroupSpec) error {
	switch t := spec.GetType(); {
	case t == computepb.NVLInstanceGroupSpec_UNSPECIFIED:
		return errs.BadRequest(errs.Violation{Field: "spec.type", Message: "required: one of " + nvlTypeNames()})
	case !slices.Contains(nvlTypes, t):
		return errs.BadRequest(errs.Violation{Field: "spec.type", Message: fmt.Sprintf("unknown type %d: one of %s", t, nvlTypeNames())})
	}
	if spec.GetSize() <= 0 {
		return errs.BadRequest(errs.Violation{Field: "spec.size", Message: "must be greater than 0"})
	}
	return nil
}

// patchGroup returns stored with the update applied, or stored itself when
// nothing changed. Status (members) is server-owned and kept. An immutable
// field left empty by the patch keeps its stored value.
func (s *NVLInstanceGroupService) patchGroup(stored *computepb.NVLInstanceGroup, cmd UpdateNVLInstanceGroupCmd) (*computepb.NVLInstanceGroup, error) {
	if err := checkMetadataUpdatable(stored.GetMetadata(), cmd.Metadata); err != nil {
		return nil, err
	}
	updated, err := patchResource(stored, &computepb.NVLInstanceGroup{
		Metadata: &commonpb.ResourceMetadata{Name: cmd.Metadata.GetName(), Labels: cmd.Metadata.GetLabels()},
		Spec:     cmd.Spec,
	}, cmd.ResetMask)
	if err != nil {
		return nil, err
	}
	if updated.GetSpec() == nil {
		updated.Spec = &computepb.NVLInstanceGroupSpec{}
	}
	prev, next := stored.GetSpec(), updated.GetSpec()
	var v []errs.Violation
	switch {
	case next.GetType() == computepb.NVLInstanceGroupSpec_UNSPECIFIED:
		next.Type = prev.GetType()
	case next.GetType() != prev.GetType():
		v = append(v, errs.Violation{Field: "spec.type", Message: "field is immutable"})
	}
	switch {
	case next.GetSize() == 0:
		next.Size = prev.GetSize()
	case next.GetSize() != prev.GetSize():
		v = append(v, errs.Violation{Field: "spec.size", Message: "field is immutable"})
	}
	if len(v) > 0 {
		return nil, errs.BadRequest(v...)
	}
	updated.Status = proto.CloneOf(stored.GetStatus())
	if proto.Equal(updated, stored) {
		return stored, nil // nothing changed: keep resource_version and updated_at
	}
	updated.Metadata.ResourceVersion = stored.GetMetadata().GetResourceVersion() + 1
	updated.Metadata.UpdatedAt = timestamppb.New(s.d.Clock.Now())
	return updated, nil
}
