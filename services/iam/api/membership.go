package api

import (
	"context"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"

	opsapi "github.com/kfirgollan/NebiusMocker/pkg/operations/api"
	"github.com/kfirgollan/NebiusMocker/pkg/serviceerror"
	"github.com/kfirgollan/NebiusMocker/services/iam/domain"
)

// MembershipHandler implements nebius.iam.v1.GroupMembershipService.
type MembershipHandler struct {
	iampb.UnimplementedGroupMembershipServiceServer
	svc *domain.MembershipService
}

var _ iampb.GroupMembershipServiceServer = (*MembershipHandler)(nil)

// NewMembershipHandler returns a handler backed by svc.
func NewMembershipHandler(svc *domain.MembershipService) *MembershipHandler {
	return &MembershipHandler{svc: svc}
}

// Create adds a member to a group.
func (h *MembershipHandler) Create(ctx context.Context, req *iampb.CreateGroupMembershipRequest) (*commonpb.Operation, error) {
	op, err := h.svc.Create(ctx, domain.CreateMembershipCmd{
		Metadata: req.GetMetadata(), Spec: req.GetSpec(), RevokeAfterHours: req.GetRevokeAfterHours(), Request: req,
	})
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// Get returns a membership.
func (h *MembershipHandler) Get(ctx context.Context, req *iampb.GetGroupMembershipRequest) (*iampb.GroupMembership, error) {
	m, err := h.svc.Get(ctx, req.GetId())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return m, nil
}

// Delete removes a member from a group.
func (h *MembershipHandler) Delete(ctx context.Context, req *iampb.DeleteGroupMembershipRequest) (*commonpb.Operation, error) {
	op, err := h.svc.Delete(ctx, req.GetId(), req)
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// ListMembers returns one page of a group's memberships.
func (h *MembershipHandler) ListMembers(ctx context.Context, req *iampb.ListGroupMembershipsRequest) (*iampb.ListGroupMembershipsResponse, error) {
	page, err := h.svc.ListMembers(ctx, req.GetParentId(), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return &iampb.ListGroupMembershipsResponse{Memberships: page.Items, NextPageToken: page.NextPageToken}, nil
}

// ListMemberOf returns the groups a principal belongs to (not paged yet).
func (h *MembershipHandler) ListMemberOf(ctx context.Context, req *iampb.ListMemberOfRequest) (*iampb.ListMemberOfResponse, error) {
	groups, err := h.svc.ListMemberOf(ctx, req.GetSubjectId())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return &iampb.ListMemberOfResponse{Items: groups}, nil
}

// GetWithAttributes returns a membership with its member's kind and attributes.
func (h *MembershipHandler) GetWithAttributes(ctx context.Context, req *iampb.GetGroupMembershipRequest) (*iampb.GroupMembershipWithAttributes, error) {
	m, err := h.svc.GetWithAttributes(ctx, req.GetId())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return m, nil
}

// ListMembersWithAttributes returns one page of a group's memberships with
// their members' kinds and attributes.
func (h *MembershipHandler) ListMembersWithAttributes(ctx context.Context, req *iampb.ListGroupMembershipsRequest) (*iampb.ListGroupMembershipsWithAttributesResponse, error) {
	page, err := h.svc.ListMembersWithAttributes(ctx, req.GetParentId(), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return &iampb.ListGroupMembershipsWithAttributesResponse{Memberships: page.Items, NextPageToken: page.NextPageToken}, nil
}
