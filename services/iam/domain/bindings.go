package domain

import (
	"context"
	"fmt"
	"strings"

	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

// Bindings answers "which roles does a principal hold on a resource" from group
// memberships and access permits. It is the authz.Bindings source.
type Bindings struct {
	memberships MembershipRepository
	permits     PermitRepository
	users       TenantUserAccountRepository
	clk         clock.Clock
}

var _ authz.Bindings = (*Bindings)(nil)

// NewBindings returns a Bindings over the repositories.
func NewBindings(memberships MembershipRepository, permits PermitRepository, users TenantUserAccountRepository, clk clock.Clock) *Bindings {
	return &Bindings{memberships: memberships, permits: permits, users: users, clk: clk}
}

// RolesOn returns the roles principalID holds directly on resourceID through
// any group it belongs to. Expired memberships (revoke_at in the past) are
// ignored, and a blocked tenant user account holds no roles at all.
func (b *Bindings) RolesOn(ctx context.Context, principalID, resourceID string) ([]string, error) {
	if blocked, err := b.blocked(ctx, principalID); err != nil || blocked {
		return nil, err
	}
	ms, err := b.memberships.ListByMember(ctx, principalID)
	if err != nil {
		return nil, fmt.Errorf("memberships of %s: %w", principalID, err)
	}
	now := b.clk.Now()
	var roles []string
	for _, m := range ms {
		if at := m.GetRevokeAt(); at != nil && !at.AsTime().After(now) {
			continue
		}
		groupRoles, err := b.groupRolesOn(ctx, m.GetMetadata().GetParentId(), resourceID)
		if err != nil {
			return nil, err
		}
		roles = append(roles, groupRoles...)
	}
	return roles, nil
}

func (b *Bindings) groupRolesOn(ctx context.Context, groupID, resourceID string) ([]string, error) {
	var roles []string
	token := ""
	for {
		page, err := b.permits.ListByGroup(ctx, groupID, maxPageSize, token)
		if err != nil {
			return nil, fmt.Errorf("permits of %s: %w", groupID, err)
		}
		for _, p := range page.Items {
			if p.GetSpec().GetResourceId() == resourceID {
				roles = append(roles, p.GetSpec().GetRole())
			}
		}
		if token = page.NextPageToken; token == "" {
			return roles, nil
		}
	}
}

// blocked reports whether principalID is a tenant user account in state BLOCKED.
// Principals without an account record (service accounts, unknown users) are not.
func (b *Bindings) blocked(ctx context.Context, principalID string) (bool, error) {
	if !strings.HasPrefix(principalID, KindTenantUserAccount+"-") {
		return false, nil
	}
	tua, err := b.users.Get(ctx, principalID)
	switch {
	case errs.IsKind(err, errs.KindNotFound):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("tenant user account %s: %w", principalID, err)
	}
	return tua.GetStatus().GetState() == iampb.TenantUserAccountStatus_BLOCKED, nil
}
