package api

import (
	"context"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/serviceerror"
	"github.com/kfirgollan/NebiusMocker/services/iam/domain"
)

const (
	tenantUserAccountService               = "nebius.iam.v1.TenantUserAccountService"
	tenantUserAccountWithAttributesService = "nebius.iam.v1.TenantUserAccountWithAttributesService"
)

// RegisterTenantUserAccountRules declares the permissions of the tenant user
// account RPCs (with and without attributes).
func RegisterTenantUserAccountRules(r *authz.Rules) {
	perm := func(verb string) authz.Permission { return authz.Perm("iam", "tenantUserAccounts", verb) }
	for _, svc := range []string{tenantUserAccountService, tenantUserAccountWithAttributesService} {
		r.Add("/"+svc+"/Get", authz.OnID(perm(authz.VerbGet)))
		r.Add("/"+svc+"/List", authz.OnParent(perm(authz.VerbList)))
	}
	r.Add("/"+tenantUserAccountService+"/Block", authz.OnID(perm(authz.VerbUpdate)))
	r.Add("/"+tenantUserAccountService+"/Unblock", authz.OnID(perm(authz.VerbUpdate)))
}

// TenantUserAccountHandler implements nebius.iam.v1.TenantUserAccountService.
type TenantUserAccountHandler struct {
	iampb.UnimplementedTenantUserAccountServiceServer
	svc *domain.TenantUserAccountService
}

var _ iampb.TenantUserAccountServiceServer = (*TenantUserAccountHandler)(nil)

// NewTenantUserAccountHandler returns a handler backed by svc.
func NewTenantUserAccountHandler(svc *domain.TenantUserAccountService) *TenantUserAccountHandler {
	return &TenantUserAccountHandler{svc: svc}
}

// Get returns a tenant user account.
func (h *TenantUserAccountHandler) Get(ctx context.Context, req *iampb.GetTenantUserAccountRequest) (*iampb.TenantUserAccount, error) {
	a, err := h.svc.Get(ctx, req.GetId())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return a, nil
}

// List returns one page of a tenant's accounts.
func (h *TenantUserAccountHandler) List(ctx context.Context, req *iampb.ListTenantUserAccountsRequest) (*iampb.ListTenantUserAccountsResponse, error) {
	page, err := h.svc.List(ctx, req.GetParentId(), req.GetPageSize(), req.GetPageToken(), req.GetFilter())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return &iampb.ListTenantUserAccountsResponse{Items: page.Items, NextPageToken: page.NextPageToken}, nil
}

// Block starts an operation that blocks the account.
func (h *TenantUserAccountHandler) Block(ctx context.Context, req *iampb.BlockTenantUserAccountRequest) (*commonpb.Operation, error) {
	return opResult(ctx)(h.svc.Block(ctx, req.GetId(), req))
}

// Unblock starts an operation that unblocks the account.
func (h *TenantUserAccountHandler) Unblock(ctx context.Context, req *iampb.UnblockTenantUserAccountRequest) (*commonpb.Operation, error) {
	return opResult(ctx)(h.svc.Unblock(ctx, req.GetId(), req))
}

// TenantUserAccountWithAttributesHandler implements
// nebius.iam.v1.TenantUserAccountWithAttributesService.
type TenantUserAccountWithAttributesHandler struct {
	iampb.UnimplementedTenantUserAccountWithAttributesServiceServer
	svc *domain.TenantUserAccountService
}

var _ iampb.TenantUserAccountWithAttributesServiceServer = (*TenantUserAccountWithAttributesHandler)(nil)

// NewTenantUserAccountWithAttributesHandler returns a handler backed by svc.
func NewTenantUserAccountWithAttributesHandler(svc *domain.TenantUserAccountService) *TenantUserAccountWithAttributesHandler {
	return &TenantUserAccountWithAttributesHandler{svc: svc}
}

// Get returns an account with its attributes.
func (h *TenantUserAccountWithAttributesHandler) Get(ctx context.Context, req *iampb.GetTenantUserAccountWithAttributesRequest) (*iampb.TenantUserAccountWithAttributes, error) {
	a, err := h.svc.GetWithAttributes(ctx, req.GetId())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return a, nil
}

// List returns one page of a tenant's accounts with attributes.
func (h *TenantUserAccountWithAttributesHandler) List(ctx context.Context, req *iampb.ListTenantUserAccountsWithAttributesRequest) (*iampb.ListTenantUserAccountsWithAttributesResponse, error) {
	page, err := h.svc.ListWithAttributes(ctx, req.GetParentId(), req.GetPageSize(), req.GetPageToken(), req.GetFilter())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return &iampb.ListTenantUserAccountsWithAttributesResponse{Items: page.Items, NextPageToken: page.NextPageToken}, nil
}
