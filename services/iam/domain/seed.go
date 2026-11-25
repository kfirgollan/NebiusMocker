package domain

import (
	"context"
	"fmt"

	"google.golang.org/protobuf/types/known/timestamppb"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

// DefaultGroups are seeded in every tenant, each with an access permit for the
// matching role on the tenant.
var DefaultGroups = []struct{ Name, Role string }{
	{"admins", authz.RoleAdmin},
	{"editors", authz.RoleEditor},
	{"viewers", authz.RoleViewer},
}

// AuthzSeed is the data needed to seed default groups.
type AuthzSeed struct {
	TenantID string
	// Members maps a default group name to the principals placed in it.
	Members map[string][]string
}

// AuthzRepos are the repositories seeding writes to.
type AuthzRepos struct {
	Groups      GroupRepository
	Memberships MembershipRepository
	Permits     PermitRepository
}

// SeedAuthz creates the default groups, their tenant-level permits and the
// requested memberships. It is idempotent: existing groups are reused.
func SeedAuthz(ctx context.Context, r AuthzRepos, ids IDGenerator, clk clock.Clock, seed AuthzSeed) error {
	now := timestamppb.New(clk.Now())
	md := func(id, parentID, name string) *commonpb.ResourceMetadata {
		return &commonpb.ResourceMetadata{Id: id, ParentId: parentID, Name: name, ResourceVersion: 1, CreatedAt: now, UpdatedAt: now}
	}
	for _, dg := range DefaultGroups {
		if _, err := r.Groups.GetByName(ctx, seed.TenantID, dg.Name); err == nil {
			continue // already seeded
		} else if !errs.IsKind(err, errs.KindNotFound) {
			return fmt.Errorf("seed group %s: %w", dg.Name, err)
		}
		groupID, err := ids.New(KindGroup)
		if err != nil {
			return err
		}
		if err := r.Groups.Create(ctx, &iampb.Group{
			Metadata: md(groupID, seed.TenantID, dg.Name), Spec: &iampb.GroupSpec{},
			Status: &iampb.GroupStatus{State: iampb.GroupStatus_ACTIVE},
		}); err != nil {
			return fmt.Errorf("seed group %s: %w", dg.Name, err)
		}
		permitID, err := ids.New(KindAccessPermit)
		if err != nil {
			return err
		}
		if err := r.Permits.Create(ctx, &iampb.AccessPermit{
			Metadata: md(permitID, groupID, ""),
			Spec:     &iampb.AccessPermitSpec{ResourceId: seed.TenantID, Role: dg.Role},
			Status:   &iampb.AccessPermitStatus{},
		}); err != nil {
			return fmt.Errorf("seed permit for %s: %w", dg.Name, err)
		}
		for _, member := range seed.Members[dg.Name] {
			mid, err := ids.New(KindGroupMembership)
			if err != nil {
				return err
			}
			if err := r.Memberships.Create(ctx, &iampb.GroupMembership{
				Metadata: md(mid, groupID, ""),
				Spec:     &iampb.GroupMembershipSpec{MemberId: member},
				Status:   &iampb.GroupMembershipStatus{GroupMemberMetadata: &commonpb.ResourceMetadata{Id: member}},
			}); err != nil {
				return fmt.Errorf("seed membership %s in %s: %w", member, dg.Name, err)
			}
		}
	}
	return nil
}
