package api

import (
	"context"

	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/serviceerror"
	"github.com/kfirgollan/NebiusMocker/services/iam/domain"
)

const sessionService = "nebius.iam.v1.SessionManagementService"

// SessionHandler implements nebius.iam.v1.SessionManagementService.
type SessionHandler struct {
	iampb.UnimplementedSessionManagementServiceServer
	svc *domain.SessionService
}

var _ iampb.SessionManagementServiceServer = (*SessionHandler)(nil)

// NewSessionHandler returns a handler backed by svc.
func NewSessionHandler(svc *domain.SessionService) *SessionHandler { return &SessionHandler{svc: svc} }

// Revoke denies the issued tokens of a service account, a tenant user account
// or the caller.
func (h *SessionHandler) Revoke(ctx context.Context, req *iampb.RevokeSessionRequest) (*iampb.RevokeSessionResponse, error) {
	var cmd domain.RevokeSessionCmd
	switch r := req.GetRevoke().(type) {
	case *iampb.RevokeSessionRequest_ServiceAccountId:
		cmd.ServiceAccountID = r.ServiceAccountId
	case *iampb.RevokeSessionRequest_TenantUserAccountId:
		cmd.TenantUserAccountID = r.TenantUserAccountId
	case *iampb.RevokeSessionRequest_AllMyActive:
		if !r.AllMyActive {
			return nil, serviceerror.FromDomain(ctx, errs.BadRequest(errs.Violation{
				Field: "all_my_active", Message: "must be true when set",
			}))
		}
		cmd.AllMyActive = true
	}
	if err := h.svc.Revoke(ctx, cmd); err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return &iampb.RevokeSessionResponse{}, nil
}

// RegisterSessionRules declares the permissions of SessionManagementService.
// Revoking another principal's sessions needs the custom revokeSessions verb
// on that principal (only the admin role's "*" grants it); every
// authenticated caller may revoke their own sessions (all_my_active).
//
// A rule carries one static permission, so the primary one is
// iam.serviceAccounts.revokeSessions and a tenant user account request also
// needs iam.tenantUserAccounts.revokeSessions (Also), both on the named account.
func RegisterSessionRules(r *authz.Rules) {
	r.Add("/"+sessionService+"/Revoke", authz.Rule{
		Permission: authz.Perm("iam", "serviceAccounts", "revokeSessions"),
		Target: func(_ context.Context, req any) (string, error) {
			rr, _ := req.(*iampb.RevokeSessionRequest)
			if id := rr.GetServiceAccountId(); id != "" {
				return id, nil
			}
			if id := rr.GetTenantUserAccountId(); id != "" {
				return id, nil
			}
			return "", errs.BadRequest(errs.Violation{
				Field: "revoke", Message: "exactly one of service_account_id, all_my_active or tenant_user_account_id is required",
			})
		},
		AllowIf: func(_ context.Context, _ string, req any) bool {
			rr, _ := req.(*iampb.RevokeSessionRequest)
			return rr.GetAllMyActive()
		},
		Also: func(req any) authz.Permission {
			rr, _ := req.(*iampb.RevokeSessionRequest)
			if rr.GetTenantUserAccountId() != "" {
				return authz.Perm("iam", "tenantUserAccounts", "revokeSessions")
			}
			return ""
		},
	})
}
