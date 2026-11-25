package domain

import (
	"context"
	"fmt"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
)

// Bindings answers "which roles does a principal hold on a resource" from group
// memberships and access permits. It is the authz.Bindings source.
type Bindings struct {
	memberships MembershipRepository
	permits     PermitRepository
	clk         clock.Clock
}

var _ authz.Bindings = (*Bindings)(nil)

// NewBindings returns a Bindings over the repositories.
func NewBindings(memberships MembershipRepository, permits PermitRepository, clk clock.Clock) *Bindings {
	return &Bindings{memberships: memberships, permits: permits, clk: clk}
}

// RolesOn returns the roles principalID holds directly on resourceID through
// any group it belongs to. Expired memberships (revoke_at in the past) are ignored.
func (b *Bindings) RolesOn(ctx context.Context, principalID, resourceID string) ([]string, error) {
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
