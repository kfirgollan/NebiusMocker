// v1alpha1 is a second view of the v1 resources: the same IDs, the same
// stored objects, the same domain methods. The deprecated nebius.mk8s.v1alpha1
// messages are a field-number-compatible subset of v1, so a request spec is
// converted by a proto binary round-trip and a stored v1 resource is converted
// back the same way; the v1-only fields have no alpha field and are dropped on
// output. TestAlphaMessagesMapOntoV1 pins the compatibility: every alpha field
// has a v1 field with the same number, name and type, so no input field is
// ever silently dropped.

//nolint:staticcheck // SA1019: nebius.mk8s.v1alpha1 is deprecated upstream; serving it is this file's purpose
package api

import (
	"context"
	"fmt"

	"google.golang.org/protobuf/proto"

	"github.com/nebius/gosdk/proto/fieldmask/grpcheader"
	"github.com/nebius/gosdk/proto/fieldmask/mask"
	commonv1alpha1pb "github.com/nebius/gosdk/proto/nebius/common/v1alpha1"
	mk8spb "github.com/nebius/gosdk/proto/nebius/mk8s/v1"
	mk8sv1alpha1pb "github.com/nebius/gosdk/proto/nebius/mk8s/v1alpha1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	opsapi "github.com/kfirgollan/NebiusMocker/pkg/operations/api"
	"github.com/kfirgollan/NebiusMocker/pkg/serviceerror"
	"github.com/kfirgollan/NebiusMocker/services/mk8s/domain"
)

// toV1 re-encodes an alpha request part as the v1 message dst.
func toV1[T proto.Message](ctx context.Context, src proto.Message, dst T) (T, error) {
	b, err := proto.Marshal(src)
	if err == nil {
		err = proto.Unmarshal(b, dst)
	}
	if err != nil {
		var zero T
		return zero, serviceerror.FromDomain(ctx, errs.BadRequest(errs.Violation{
			Field: "spec", Message: fmt.Sprintf("not convertible to nebius.mk8s.v1: %v", err),
		}))
	}
	return dst, nil
}

// alphaView converts a stored v1 resource to the alpha message dst; v1-only
// fields are dropped (the alpha output is a subset by design).
func alphaView[T proto.Message](src proto.Message, dst T) T {
	b, err := proto.Marshal(src)
	if err != nil { // a stored resource always encodes
		return dst
	}
	if err := (proto.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(b, dst); err != nil {
		return dst
	}
	return dst
}

// alphaOp maps a started operation to its v1alpha1 proto.
func alphaOp(ctx context.Context, op *operations.Operation, err error) (*commonv1alpha1pb.Operation, error) {
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.AlphaToProto(ctx, op), nil
}

// alphaResetMask reads X-ResetMask. The alpha field names equal the v1 ones.
func alphaResetMask(ctx context.Context) (*mask.Mask, error) {
	m, err := grpcheader.GetResetMaskFromIncomingContext(ctx)
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, errs.BadRequest(errs.Violation{Field: grpcheader.ResetMask, Message: err.Error()}))
	}
	return m, nil
}

// AlphaClusterHandler implements nebius.mk8s.v1alpha1.ClusterService.
type AlphaClusterHandler struct {
	mk8sv1alpha1pb.UnimplementedClusterServiceServer
	svc *domain.Service
}

var _ mk8sv1alpha1pb.ClusterServiceServer = (*AlphaClusterHandler)(nil)

// NewAlphaClusterHandler returns a handler backed by svc.
func NewAlphaClusterHandler(svc *domain.Service) *AlphaClusterHandler {
	return &AlphaClusterHandler{svc: svc}
}

// Get returns a cluster by ID.
func (h *AlphaClusterHandler) Get(ctx context.Context, req *mk8sv1alpha1pb.GetClusterRequest) (*mk8sv1alpha1pb.Cluster, error) {
	c, err := h.svc.GetCluster(ctx, req.GetId())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return alphaView(c, &mk8sv1alpha1pb.Cluster{}), nil
}

// GetByName returns a cluster by project and name.
func (h *AlphaClusterHandler) GetByName(ctx context.Context, req *mk8sv1alpha1pb.GetClusterByNameRequest) (*mk8sv1alpha1pb.Cluster, error) {
	c, err := h.svc.GetClusterByName(ctx, req.GetParentId(), req.GetName())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return alphaView(c, &mk8sv1alpha1pb.Cluster{}), nil
}

// List returns one page of a project's clusters.
func (h *AlphaClusterHandler) List(ctx context.Context, req *mk8sv1alpha1pb.ListClustersRequest) (*mk8sv1alpha1pb.ListClustersResponse, error) {
	page, err := h.svc.ListClusters(ctx, req.GetParentId(), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	out := &mk8sv1alpha1pb.ListClustersResponse{NextPageToken: page.NextPageToken}
	for _, c := range page.Items {
		out.Items = append(out.Items, alphaView(c, &mk8sv1alpha1pb.Cluster{}))
	}
	return out, nil
}

// Create starts a cluster creation operation.
func (h *AlphaClusterHandler) Create(ctx context.Context, req *mk8sv1alpha1pb.CreateClusterRequest) (*commonv1alpha1pb.Operation, error) {
	spec, err := toV1(ctx, req.GetSpec(), &mk8spb.ClusterSpec{})
	if err != nil {
		return nil, err
	}
	op, err := h.svc.CreateCluster(ctx, domain.CreateClusterCmd{Metadata: req.GetMetadata(), Spec: spec, Request: req})
	return alphaOp(ctx, op, err)
}

// Update starts a cluster update operation; X-ResetMask selects fields to clear.
func (h *AlphaClusterHandler) Update(ctx context.Context, req *mk8sv1alpha1pb.UpdateClusterRequest) (*commonv1alpha1pb.Operation, error) {
	m, err := alphaResetMask(ctx)
	if err != nil {
		return nil, err
	}
	spec, err := toV1(ctx, req.GetSpec(), &mk8spb.ClusterSpec{})
	if err != nil {
		return nil, err
	}
	op, err := h.svc.UpdateCluster(ctx, domain.UpdateClusterCmd{Metadata: req.GetMetadata(), Spec: spec, ResetMask: m, Request: req})
	return alphaOp(ctx, op, err)
}

// Delete starts a cluster deletion operation.
func (h *AlphaClusterHandler) Delete(ctx context.Context, req *mk8sv1alpha1pb.DeleteClusterRequest) (*commonv1alpha1pb.Operation, error) {
	op, err := h.svc.DeleteCluster(ctx, req.GetId(), req)
	return alphaOp(ctx, op, err)
}

// ListControlPlaneVersions lists the Kubernetes versions clusters can run.
func (h *AlphaClusterHandler) ListControlPlaneVersions(context.Context, *mk8sv1alpha1pb.ListClusterControlPlaneVersionsRequest) (*mk8sv1alpha1pb.ListClusterControlPlaneVersionsResponse, error) {
	out := &mk8sv1alpha1pb.ListClusterControlPlaneVersionsResponse{}
	for _, v := range h.svc.ControlPlaneVersions() {
		out.Items = append(out.Items, alphaView(v, &mk8sv1alpha1pb.ClusterControlPlaneVersion{}))
	}
	return out, nil
}

// AlphaNodeGroupHandler implements nebius.mk8s.v1alpha1.NodeGroupService.
type AlphaNodeGroupHandler struct {
	mk8sv1alpha1pb.UnimplementedNodeGroupServiceServer
	svc *domain.Service
}

var _ mk8sv1alpha1pb.NodeGroupServiceServer = (*AlphaNodeGroupHandler)(nil)

// NewAlphaNodeGroupHandler returns a handler backed by svc.
func NewAlphaNodeGroupHandler(svc *domain.Service) *AlphaNodeGroupHandler {
	return &AlphaNodeGroupHandler{svc: svc}
}

// Get returns a node group by ID.
func (h *AlphaNodeGroupHandler) Get(ctx context.Context, req *mk8sv1alpha1pb.GetNodeGroupRequest) (*mk8sv1alpha1pb.NodeGroup, error) {
	g, err := h.svc.GetNodeGroup(ctx, req.GetId())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return alphaView(g, &mk8sv1alpha1pb.NodeGroup{}), nil
}

// GetByName returns a node group by cluster and name.
func (h *AlphaNodeGroupHandler) GetByName(ctx context.Context, req *mk8sv1alpha1pb.GetNodeGroupByNameRequest) (*mk8sv1alpha1pb.NodeGroup, error) {
	g, err := h.svc.GetNodeGroupByName(ctx, req.GetParentId(), req.GetName())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return alphaView(g, &mk8sv1alpha1pb.NodeGroup{}), nil
}

// List returns one page of a cluster's node groups.
func (h *AlphaNodeGroupHandler) List(ctx context.Context, req *mk8sv1alpha1pb.ListNodeGroupsRequest) (*mk8sv1alpha1pb.ListNodeGroupsResponse, error) {
	page, err := h.svc.ListNodeGroups(ctx, req.GetParentId(), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	out := &mk8sv1alpha1pb.ListNodeGroupsResponse{NextPageToken: page.NextPageToken}
	for _, g := range page.Items {
		out.Items = append(out.Items, alphaView(g, &mk8sv1alpha1pb.NodeGroup{}))
	}
	return out, nil
}

// Create starts a node group creation operation.
func (h *AlphaNodeGroupHandler) Create(ctx context.Context, req *mk8sv1alpha1pb.CreateNodeGroupRequest) (*commonv1alpha1pb.Operation, error) {
	spec, err := toV1(ctx, req.GetSpec(), &mk8spb.NodeGroupSpec{})
	if err != nil {
		return nil, err
	}
	op, err := h.svc.CreateNodeGroup(ctx, domain.CreateNodeGroupCmd{Metadata: req.GetMetadata(), Spec: spec, Request: req})
	return alphaOp(ctx, op, err)
}

// Update starts a node group update operation; X-ResetMask selects fields to clear.
func (h *AlphaNodeGroupHandler) Update(ctx context.Context, req *mk8sv1alpha1pb.UpdateNodeGroupRequest) (*commonv1alpha1pb.Operation, error) {
	m, err := alphaResetMask(ctx)
	if err != nil {
		return nil, err
	}
	spec, err := toV1(ctx, req.GetSpec(), &mk8spb.NodeGroupSpec{})
	if err != nil {
		return nil, err
	}
	op, err := h.svc.UpdateNodeGroup(ctx, domain.UpdateNodeGroupCmd{Metadata: req.GetMetadata(), Spec: spec, ResetMask: m, Request: req})
	return alphaOp(ctx, op, err)
}

// Delete starts a node group deletion operation.
func (h *AlphaNodeGroupHandler) Delete(ctx context.Context, req *mk8sv1alpha1pb.DeleteNodeGroupRequest) (*commonv1alpha1pb.Operation, error) {
	op, err := h.svc.DeleteNodeGroup(ctx, req.GetId(), req)
	return alphaOp(ctx, op, err)
}

// Upgrade starts an upgrade to the latest node image (the only upgrade type).
func (h *AlphaNodeGroupHandler) Upgrade(ctx context.Context, req *mk8sv1alpha1pb.UpgradeNodeGroupRequest) (*commonv1alpha1pb.Operation, error) {
	op, err := h.svc.UpgradeNodeGroup(ctx, req.GetId(), req)
	return alphaOp(ctx, op, err)
}
