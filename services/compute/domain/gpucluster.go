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
	"github.com/kfirgollan/NebiusMocker/pkg/quota"
	"github.com/kfirgollan/NebiusMocker/pkg/reqctx"
)

// GpuClusterDeps are the dependencies of GpuClusterService. All are required.
type GpuClusterDeps struct {
	Clusters GpuClusterRepository
	Fabrics  FabricCatalog
	// OpIndex records GPU cluster operations per project (ListOperationsByParent).
	OpIndex  OperationIndex
	Projects Projects
	Regions  ProjectRegions
	Ops      Operations
	IDs      IDGenerator
	Clock    clock.Clock
	// Quota admits GPU cluster creations.
	Quota QuotaGate
}

func (d GpuClusterDeps) validate() error {
	if d.Clusters == nil || d.OpIndex == nil || d.Projects == nil || d.Regions == nil ||
		d.Ops == nil || d.IDs == nil || d.Clock == nil || d.Quota == nil {
		return errors.New("compute: gpu cluster service dependencies must not be nil")
	}
	return nil
}

// GpuClusterService implements nebius.compute.v1.GpuClusterService behavior.
// It keeps the inventory only: no GPU, fabric or topology is simulated.
// Members are added and removed by InstanceService through GpuClusterMembership.
type GpuClusterService struct{ d GpuClusterDeps }

// NewGpuClusterService wires a GpuClusterService.
func NewGpuClusterService(d GpuClusterDeps) (*GpuClusterService, error) {
	if err := d.validate(); err != nil {
		return nil, err
	}
	return &GpuClusterService{d: d}, nil
}

// CreateGpuClusterCmd is the input of GpuClusterService.Create.
type CreateGpuClusterCmd struct {
	Metadata *commonpb.ResourceMetadata
	Spec     *computepb.GpuClusterSpec
	Request  proto.Message // echoed in Operation.request
}

// UpdateGpuClusterCmd is the input of GpuClusterService.Update (full replace
// guarded by a reset mask).
type UpdateGpuClusterCmd struct {
	Metadata  *commonpb.ResourceMetadata
	Spec      *computepb.GpuClusterSpec
	ResetMask *mask.Mask
	Request   proto.Message
}

// Get returns a GPU cluster by ID.
func (s *GpuClusterService) Get(ctx context.Context, id string) (*computepb.GpuCluster, error) {
	if id == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "id", Message: "required"})
	}
	return s.d.Clusters.Get(ctx, id)
}

// GetByName returns a GPU cluster by its name within a project.
func (s *GpuClusterService) GetByName(ctx context.Context, parentID, name string) (*computepb.GpuCluster, error) {
	if parentID == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "parent_id", Message: "required"})
	}
	return s.d.Clusters.GetByName(ctx, parentID, name)
}

// List returns one page of a project's GPU clusters in creation order. The
// upstream filter field is deprecated and "not implemented", so a non-empty
// filter is rejected rather than ignored.
func (s *GpuClusterService) List(ctx context.Context, parentID string, size int64, token, filter string) (GpuClusterPage, error) {
	if parentID == "" {
		return GpuClusterPage{}, errs.BadRequest(errs.Violation{Field: "parent_id", Message: "required"})
	}
	if filter != "" {
		return GpuClusterPage{}, errs.BadRequest(errs.Violation{
			Field: "filter", Message: "filter is not supported (deprecated upstream); filter on the client side",
		})
	}
	n, err := pageSize(size)
	if err != nil {
		return GpuClusterPage{}, err
	}
	return s.d.Clusters.List(ctx, parentID, n, token)
}

// Create validates the request and stores the cluster, reconciling until the
// operation completes.
func (s *GpuClusterService) Create(ctx context.Context, cmd CreateGpuClusterCmd) (*operations.Operation, error) {
	cluster, err := s.newCluster(ctx, cmd)
	if err != nil {
		return nil, err
	}
	id := cluster.GetMetadata().GetId()
	return s.submit(ctx, cluster.GetMetadata().GetParentId(), operations.Spec{
		ResourceID:  id,
		Description: "Create GPU cluster",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     cmd.Request,
		Prepare: func(ctx context.Context) error {
			return admit(ctx, s.d.Quota, cluster.GetMetadata().GetParentId(), map[string]int64{quota.ComputeGPUClusterCount: 1},
				func(ctx context.Context) error { return s.d.Clusters.Create(ctx, cluster) })
		},
		Work: func(ctx context.Context) error {
			_, err := s.d.Clusters.Mutate(ctx, id, func(c *computepb.GpuCluster) error {
				c.Status.Reconciling = false
				return nil
			})
			return err
		},
	})
}

// Update applies a full-replace update with reset-mask semantics. Name and
// labels may change; spec.infiniband_fabric is immutable.
func (s *GpuClusterService) Update(ctx context.Context, cmd UpdateGpuClusterCmd) (*operations.Operation, error) {
	id := cmd.Metadata.GetId()
	if id == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.id", Message: "required"})
	}
	stored, err := s.d.Clusters.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.submit(ctx, stored.GetMetadata().GetParentId(), operations.Spec{
		ResourceID:  id,
		Description: "Update GPU cluster",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     cmd.Request,
		Prepare: func(ctx context.Context) error {
			_, err := s.d.Clusters.Mutate(ctx, id, func(c *computepb.GpuCluster) error {
				updated, err := s.patchCluster(c, cmd)
				if err != nil {
					return err
				}
				if updated != c {
					proto.Reset(c)
					proto.Merge(c, updated)
				}
				return nil
			})
			return err
		},
		Work: func(context.Context) error { return nil },
	})
}

// Delete removes the cluster when the operation completes. It fails with
// FAILED_PRECONDITION while instances belong to it: they must be deleted or
// moved to another cluster first. The emptiness check and the deletion run in
// one store transaction, so an instance that joins meanwhile fails the
// operation instead of being orphaned.
func (s *GpuClusterService) Delete(ctx context.Context, id string, request proto.Message) (*operations.Operation, error) {
	if id == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "id", Message: "required"})
	}
	stored, err := s.d.Clusters.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.submit(ctx, stored.GetMetadata().GetParentId(), operations.Spec{
		ResourceID:  id,
		Description: "Delete GPU cluster",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     request,
		Prepare: func(ctx context.Context) error {
			c, err := s.d.Clusters.Get(ctx, id)
			if err != nil {
				return err
			}
			return CheckGpuClusterDeletable(c)
		},
		Work: func(ctx context.Context) error { return s.d.Clusters.DeleteIf(ctx, id, CheckGpuClusterDeletable) },
	})
}

// CheckGpuClusterDeletable reports why a GPU cluster cannot be deleted, or nil.
func CheckGpuClusterDeletable(c *computepb.GpuCluster) error {
	if members := c.GetStatus().GetInstances(); len(members) > 0 {
		return errs.BadState(c.GetMetadata().GetId(), fmt.Sprintf(
			"gpu cluster has instances (%s): delete them or move them to another cluster first",
			strings.Join(members, ", ")))
	}
	return nil
}

// ListOperationsByParent returns the operations on GPU clusters of a project,
// newest first, including operations on clusters deleted since.
func (s *GpuClusterService) ListOperationsByParent(ctx context.Context, parentID string, size int64, token string) (OperationPage, error) {
	if parentID == "" {
		return OperationPage{}, errs.BadRequest(errs.Violation{Field: "parent_id", Message: "required"})
	}
	n, err := pageSize(size)
	if err != nil {
		return OperationPage{}, err
	}
	offset, err := decodeOffset(token)
	if err != nil {
		return OperationPage{}, err
	}
	ids, err := s.d.OpIndex.IDs(ctx, parentID)
	if err != nil {
		return OperationPage{}, err
	}
	slices.Reverse(ids)
	var page OperationPage
	for i := offset; i < len(ids) && len(page.Items) < n; i++ {
		op, err := s.d.Ops.Get(ctx, ids[i])
		if err != nil {
			return OperationPage{}, err
		}
		page.Items = append(page.Items, op)
	}
	if next := offset + n; next < len(ids) {
		page.NextPageToken = encodeOffset(next)
	}
	return page, nil
}

func (s *GpuClusterService) submit(ctx context.Context, parentID string, spec operations.Spec) (*operations.Operation, error) {
	op, err := s.d.Ops.Submit(ctx, spec)
	if err != nil {
		return nil, err
	}
	if err := s.d.OpIndex.Add(ctx, parentID, op.ID); err != nil {
		return nil, fmt.Errorf("index operation %s: %w", op.ID, err)
	}
	return op, nil
}

// newCluster validates a Create request and builds the new cluster.
func (s *GpuClusterService) newCluster(ctx context.Context, cmd CreateGpuClusterCmd) (*computepb.GpuCluster, error) {
	md := cmd.Metadata
	if md.GetId() != "" {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.id", Message: "must not be set on create"})
	}
	parentID := md.GetParentId()
	if parentID == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.parent_id", Message: "required"})
	}
	if _, err := s.d.Projects.Parent(ctx, parentID); err != nil {
		return nil, fmt.Errorf("gpu cluster parent %s: %w", parentID, err)
	}
	if err := s.checkFabric(ctx, parentID, cmd.Spec.GetInfinibandFabric()); err != nil {
		return nil, err
	}
	id, err := s.d.IDs.New(KindGpuCluster)
	if err != nil {
		return nil, fmt.Errorf("new gpu cluster id: %w", err)
	}
	now := timestamppb.New(s.d.Clock.Now())
	return &computepb.GpuCluster{
		Metadata: &commonpb.ResourceMetadata{
			Id: id, ParentId: parentID, Name: md.GetName(), Labels: maps.Clone(md.GetLabels()),
			ResourceVersion: 1, CreatedAt: now, UpdatedAt: now,
		},
		Spec:   proto.CloneOf(cmd.Spec), // echoed as sent
		Status: &computepb.GpuClusterStatus{Reconciling: true},
	}, nil
}

// checkFabric checks that fabric is a known fabric of the project's region.
func (s *GpuClusterService) checkFabric(ctx context.Context, parentID, fabric string) error {
	const field = "spec.infiniband_fabric"
	region, err := s.d.Regions.Region(ctx, parentID)
	if err != nil {
		return fmt.Errorf("region of project %s: %w", parentID, err)
	}
	available := strings.Join(s.d.Fabrics.InRegion(region), ", ")
	if available == "" {
		available = "none"
	}
	if fabric == "" {
		return errs.BadRequest(errs.Violation{
			Field: field, Message: fmt.Sprintf("spec.infiniband_fabric is required; fabrics in %s: %s", region, available),
		})
	}
	f, ok := s.d.Fabrics.Lookup(fabric)
	switch {
	case !ok:
		return errs.BadRequest(errs.Violation{
			Field: field, Message: fmt.Sprintf("spec.infiniband_fabric: unknown fabric %q; fabrics in %s: %s", fabric, region, available),
		})
	case f.Region != region:
		return errs.BadRequest(errs.Violation{Field: field, Message: fmt.Sprintf(
			"spec.infiniband_fabric: fabric %s is in region %s, but project %s is in %s; fabrics in %s: %s",
			fabric, f.Region, parentID, region, region, available)})
	}
	return nil
}

// patchCluster returns stored with the update applied, or stored itself when
// nothing changed. Status (members, topology) is server-owned and kept.
func (s *GpuClusterService) patchCluster(stored *computepb.GpuCluster, cmd UpdateGpuClusterCmd) (*computepb.GpuCluster, error) {
	if err := checkMetadataUpdatable(stored.GetMetadata(), cmd.Metadata); err != nil {
		return nil, err
	}
	updated, err := patchResource(stored, &computepb.GpuCluster{
		Metadata: &commonpb.ResourceMetadata{Name: cmd.Metadata.GetName(), Labels: cmd.Metadata.GetLabels()},
		Spec:     cmd.Spec,
	}, cmd.ResetMask)
	if err != nil {
		return nil, err
	}
	if updated.GetSpec() == nil {
		updated.Spec = &computepb.GpuClusterSpec{}
	}
	switch prev, next := stored.GetSpec().GetInfinibandFabric(), updated.GetSpec().GetInfinibandFabric(); {
	case next == "":
		updated.Spec.InfinibandFabric = prev // unset keeps the stored fabric
	case next != prev:
		return nil, errs.BadRequest(errs.Violation{Field: "spec.infiniband_fabric", Message: "field is immutable"})
	}
	updated.Status = proto.CloneOf(stored.GetStatus())
	if proto.Equal(updated, stored) {
		return stored, nil // nothing changed: keep resource_version and updated_at
	}
	updated.Metadata.ResourceVersion = stored.GetMetadata().GetResourceVersion() + 1
	updated.Metadata.UpdatedAt = timestamppb.New(s.d.Clock.Now())
	return updated, nil
}
