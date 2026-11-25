package api

import (
	"context"

	"google.golang.org/grpc/status"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	opsapi "github.com/kfirgollan/NebiusMocker/pkg/operations/api"
	"github.com/kfirgollan/NebiusMocker/pkg/serviceerror"
	"github.com/kfirgollan/NebiusMocker/services/compute/domain"
)

const instanceService = "nebius.compute.v1.InstanceService"

// RegisterInstanceAuthzRules declares the permission each InstanceService
// RPC needs: compute.instances.* (plus start and stop) on the project.
// BatchGet authorizes each ID in the domain (compute.instances.get), so one
// unreadable ID yields an error entry instead of failing the call.
func RegisterInstanceAuthzRules(r *authz.Rules) {
	r.CRUD(instanceService, "compute", "instances")
	m := func(method string) string { return "/" + instanceService + "/" + method }
	r.Add(m("ListOperationsByParent"), authz.OnParent(authz.Perm("compute", "instances", authz.VerbList)))
	r.Add(m("Start"), authz.OnID(authz.Perm("compute", "instances", authz.VerbStart)))
	r.Add(m("Stop"), authz.OnID(authz.Perm("compute", "instances", authz.VerbStop)))
	r.Add(m("BatchGet"), authz.Rule{
		Permission: authz.Perm("compute", "instances", authz.VerbGet),
		AllowIf:    func(context.Context, string, any) bool { return true },
		Target:     func(context.Context, any) (string, error) { return "", nil },
	})
	// The group is the target: NVL instance groups are not implemented, so
	// the lookup of any ID is NOT_FOUND.
	r.Add(m("ListInstancesByNVLInstanceGroup"), authz.Rule{
		Permission: authz.Perm("compute", "instances", authz.VerbList),
		Target: func(_ context.Context, req any) (string, error) {
			if g, ok := req.(*computepb.ListInstancesByNVLInstanceGroupRequest); ok && g.GetNvlInstanceGroupId() != "" {
				return g.GetNvlInstanceGroupId(), nil
			}
			return "", errs.BadRequest(errs.Violation{Field: "nvl_instance_group_id", Message: "required"})
		},
	})
}

// InstanceHandler implements nebius.compute.v1.InstanceService.
type InstanceHandler struct {
	computepb.UnimplementedInstanceServiceServer
	svc *domain.InstanceService
}

var _ computepb.InstanceServiceServer = (*InstanceHandler)(nil)

// NewInstanceHandler returns a handler backed by svc.
func NewInstanceHandler(svc *domain.InstanceService) *InstanceHandler {
	return &InstanceHandler{svc: svc}
}

// Get returns an instance by ID.
func (h *InstanceHandler) Get(ctx context.Context, req *computepb.GetInstanceRequest) (*computepb.Instance, error) {
	inst, err := h.svc.Get(ctx, req.GetId())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return inst, nil
}

// GetByName returns an instance by parent and name.
func (h *InstanceHandler) GetByName(ctx context.Context, req *commonpb.GetByNameRequest) (*computepb.Instance, error) {
	inst, err := h.svc.GetByName(ctx, req.GetParentId(), req.GetName())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return inst, nil
}

// BatchGet returns the instances by ID, with an error in place of each one
// that cannot be read.
func (h *InstanceHandler) BatchGet(ctx context.Context, req *computepb.BatchGetRequest) (*computepb.BatchGetResponse, error) {
	results, err := h.svc.BatchGet(ctx, req.GetInstanceIds())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	resp := &computepb.BatchGetResponse{Items: make(map[string]*computepb.BatchGetResponse_BatchGetResult, len(results))}
	for id, r := range results {
		if r.Err != nil {
			resp.Items[id] = &computepb.BatchGetResponse_BatchGetResult{
				Result: &computepb.BatchGetResponse_BatchGetResult_Error{Error: status.Convert(serviceerror.FromDomain(ctx, r.Err)).Proto()},
			}
			continue
		}
		resp.Items[id] = &computepb.BatchGetResponse_BatchGetResult{
			Result: &computepb.BatchGetResponse_BatchGetResult_Instance{Instance: r.Instance},
		}
	}
	return resp, nil
}

// List returns one page of a project's instances.
func (h *InstanceHandler) List(ctx context.Context, req *computepb.ListInstancesRequest) (*computepb.ListInstancesResponse, error) {
	page, err := h.svc.List(ctx, req.GetParentId(), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return &computepb.ListInstancesResponse{Items: page.Items, NextPageToken: page.NextPageToken}, nil
}

// ListInstancesByNVLInstanceGroup lists the instances of an NVL instance group.
func (h *InstanceHandler) ListInstancesByNVLInstanceGroup(ctx context.Context, req *computepb.ListInstancesByNVLInstanceGroupRequest) (*computepb.ListInstancesResponse, error) {
	page, err := h.svc.ListByNVLInstanceGroup(ctx, req.GetNvlInstanceGroupId(), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return &computepb.ListInstancesResponse{Items: page.Items, NextPageToken: page.NextPageToken}, nil
}

// Create starts an instance creation operation.
func (h *InstanceHandler) Create(ctx context.Context, req *computepb.CreateInstanceRequest) (*commonpb.Operation, error) {
	op, err := h.svc.Create(ctx, domain.CreateInstanceCmd{Metadata: req.GetMetadata(), Spec: req.GetSpec(), Request: req})
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// Update starts an instance update operation; X-ResetMask selects fields to clear.
func (h *InstanceHandler) Update(ctx context.Context, req *computepb.UpdateInstanceRequest) (*commonpb.Operation, error) {
	m, err := resetMask(ctx)
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	op, err := h.svc.Update(ctx, domain.UpdateInstanceCmd{Metadata: req.GetMetadata(), Spec: req.GetSpec(), ResetMask: m, Request: req})
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// Delete starts an instance deletion operation.
func (h *InstanceHandler) Delete(ctx context.Context, req *computepb.DeleteInstanceRequest) (*commonpb.Operation, error) {
	op, err := h.svc.Delete(ctx, req.GetId(), req)
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// Start starts a stopped instance.
func (h *InstanceHandler) Start(ctx context.Context, req *computepb.StartInstanceRequest) (*commonpb.Operation, error) {
	op, err := h.svc.Start(ctx, req.GetId(), req)
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// Stop stops a running instance.
func (h *InstanceHandler) Stop(ctx context.Context, req *computepb.StopInstanceRequest) (*commonpb.Operation, error) {
	op, err := h.svc.Stop(ctx, req.GetId(), req)
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// ListOperationsByParent lists the operations on a project's instances.
func (h *InstanceHandler) ListOperationsByParent(ctx context.Context, req *computepb.ListOperationsByParentRequest) (*commonpb.ListOperationsResponse, error) {
	page, err := h.svc.ListOperationsByParent(ctx, req.GetParentId(), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	resp := &commonpb.ListOperationsResponse{NextPageToken: page.NextPageToken}
	for _, op := range page.Items {
		resp.Operations = append(resp.Operations, opsapi.ToProto(ctx, op))
	}
	return resp, nil
}
