package domain

import (
	"context"
	"fmt"
	"time"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
)

var _ peer.CapacityReservations = (*Service)(nil)

// reservationIDsField names the compute field whose values are block group
// IDs (instance.spec.reservation_policy.reservation_ids).
const reservationIDsField = "reservation_policy.reservation_ids"

// Reserve implements peer.CapacityReservations: it draws r.Quantity units
// from the first candidate block group with enough free capacity, within
// both the group's current limit and the project's effective limit.
func (s *Service) Reserve(ctx context.Context, r peer.CapacityRequest) (string, error) {
	if r.ConsumerID == "" || r.Quantity == 0 {
		return "", errs.BadRequest(errs.Violation{Field: "consumer", Message: "a consumer ID and a positive quantity are required"})
	}
	tenant, err := s.projectTenant(ctx, "project_id", r.ProjectID)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	candidates, err := s.candidates(tenant, r)
	if err != nil {
		return "", err
	}
	now := s.clk.Now()
	for _, g := range candidates {
		ok, err := s.fits(ctx, g, r, now)
		if err != nil {
			return "", err
		}
		if ok {
			return g.ID, s.usage.Put(ctx, Usage{ConsumerID: r.ConsumerID, ProjectID: r.ProjectID, BlockGroupID: g.ID, Quantity: r.Quantity})
		}
	}
	return "", &errs.Error{Kind: errs.KindQuotaExceeded, ResourceID: r.ProjectID, Message: fmt.Sprintf(
		"not enough reserved capacity: no capacity block group for %s%s in %s has %d free %ss within the project's limit",
		r.Platform, fabricSuffix(r.Fabric), r.Region, r.Quantity, Unit)}
}

// candidates returns the block groups to try: the named ones, which must
// match, or every group of the tenant that matches.
func (s *Service) candidates(tenant string, r peer.CapacityRequest) ([]*BlockGroup, error) {
	matches := func(g *BlockGroup) bool {
		return g.Region == r.Region && g.Platform == r.Platform && (r.Fabric == "" || g.Fabric == r.Fabric)
	}
	if len(r.ReservationIDs) == 0 {
		var out []*BlockGroup
		for _, g := range s.inv.InTenant(tenant) {
			if matches(g) {
				out = append(out, g)
			}
		}
		return out, nil
	}
	out := make([]*BlockGroup, 0, len(r.ReservationIDs))
	for i, id := range r.ReservationIDs {
		g, err := s.group(id, tenant)
		if err != nil {
			return nil, err
		}
		if !matches(g) {
			return nil, errs.BadRequest(errs.Violation{
				Field: fmt.Sprintf("%s[%d]", reservationIDsField, i),
				Message: fmt.Sprintf("capacity block group %s reserves %s on %s in %s, not %s%s in %s",
					id, g.Platform, g.Fabric, g.Region, r.Platform, fabricSuffix(r.Fabric), r.Region),
			})
		}
		out = append(out, g)
	}
	return out, nil
}

// fits reports whether r fits in g, not counting r.ConsumerID's current
// reservation (so a consumer can be re-reserved in place).
func (s *Service) fits(ctx context.Context, g *BlockGroup, r peer.CapacityRequest, now time.Time) (bool, error) {
	held, err := s.usage.ByBlockGroup(ctx, g.ID)
	if err != nil {
		return false, err
	}
	var total, project uint64
	for _, u := range held {
		if u.ConsumerID == r.ConsumerID {
			continue
		}
		total += u.Quantity
		if u.ProjectID == r.ProjectID {
			project += u.Quantity
		}
	}
	limit := g.limitAt(now)
	if total+r.Quantity > limit {
		return false, nil
	}
	allowance, err := s.projectLimit(ctx, r.ProjectID, g.ID)
	if err != nil {
		return false, err
	}
	return project+r.Quantity <= effectiveLimit(allowance, limit), nil
}

// projectLimit returns the project's allowance limit for the group (nil:
// unlimited, also when the project has no allowance).
func (s *Service) projectLimit(ctx context.Context, projectID, groupID string) (limit *uint64, err error) {
	all, err := s.allowances.ByProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	for _, a := range all {
		if a.GetSpec().GetCapacityBlockGroupId() == groupID {
			return allowanceLimit(a), nil
		}
	}
	return limit, nil
}

// Release implements peer.CapacityReservations.
func (s *Service) Release(ctx context.Context, consumerID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.usage.Delete(ctx, consumerID)
}
