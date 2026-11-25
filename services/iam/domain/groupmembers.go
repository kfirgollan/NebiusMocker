package domain

import (
	"context"
	"fmt"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
)

// GroupMembers implements peer.GroupMembers over the membership repository.
type GroupMembers struct {
	memberships MembershipRepository
	clk         clock.Clock
}

var _ peer.GroupMembers = (*GroupMembers)(nil)

// NewGroupMembers returns a GroupMembers.
func NewGroupMembers(memberships MembershipRepository, clk clock.Clock) *GroupMembers {
	return &GroupMembers{memberships: memberships, clk: clk}
}

// IsMember implements peer.GroupMembers.
func (g *GroupMembers) IsMember(ctx context.Context, principalID, groupID string) (bool, error) {
	ms, err := g.memberships.ListByMember(ctx, principalID)
	if err != nil {
		return false, fmt.Errorf("memberships of %s: %w", principalID, err)
	}
	now := g.clk.Now()
	for _, m := range ms {
		if at := m.GetRevokeAt(); at != nil && !at.AsTime().After(now) {
			continue
		}
		if m.GetMetadata().GetParentId() == groupID {
			return true, nil
		}
	}
	return false, nil
}
