package domain

import (
	"context"
	"errors"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/reqctx"
)

// SessionRevoker denies the access tokens issued for a principal (satisfied by
// *auth.Issuer).
type SessionRevoker interface {
	RevokePrincipal(principalID string)
}

// SessionService implements nebius.iam.v1.SessionManagementService. It revokes
// the access tokens the mocker issued (token exchange). The static dev tokens
// from the configuration are not sessions: they are not issued, so revoking
// their principal leaves them valid.
type SessionService struct {
	sas         ServiceAccountRepository
	tenantUsers TenantUserAccountRepository
	revoker     SessionRevoker
}

// NewSessionService wires a SessionService.
func NewSessionService(sas ServiceAccountRepository, tenantUsers TenantUserAccountRepository, revoker SessionRevoker) (*SessionService, error) {
	if sas == nil || tenantUsers == nil || revoker == nil {
		return nil, errors.New("iam: session service dependencies must not be nil")
	}
	return &SessionService{sas: sas, tenantUsers: tenantUsers, revoker: revoker}, nil
}

// RevokeSessionCmd is the input of Revoke: exactly one target must be set.
type RevokeSessionCmd struct {
	// ServiceAccountID revokes every issued token of that service account.
	ServiceAccountID string
	// TenantUserAccountID revokes every issued token of that tenant user account.
	TenantUserAccountID string
	// AllMyActive revokes the caller's own issued tokens.
	AllMyActive bool
}

// Revoke denies the tokens of the selected principal. It is synchronous.
func (s *SessionService) Revoke(ctx context.Context, cmd RevokeSessionCmd) error {
	n := 0
	for _, set := range []bool{cmd.ServiceAccountID != "", cmd.TenantUserAccountID != "", cmd.AllMyActive} {
		if set {
			n++
		}
	}
	if n != 1 {
		return errs.BadRequest(errs.Violation{
			Field:   "revoke",
			Message: "exactly one of service_account_id, all_my_active or tenant_user_account_id is required",
		})
	}
	switch {
	case cmd.ServiceAccountID != "":
		if _, err := s.sas.Get(ctx, cmd.ServiceAccountID); err != nil {
			return err
		}
		s.revoker.RevokePrincipal(cmd.ServiceAccountID)
	case cmd.TenantUserAccountID != "":
		if _, err := s.tenantUsers.Get(ctx, cmd.TenantUserAccountID); err != nil {
			return err
		}
		s.revoker.RevokePrincipal(cmd.TenantUserAccountID)
	default:
		id := reqctx.PrincipalFrom(ctx).ID
		if id == "" {
			return errs.Unauthenticated("the request has no authenticated caller")
		}
		s.revoker.RevokePrincipal(id)
	}
	return nil
}
