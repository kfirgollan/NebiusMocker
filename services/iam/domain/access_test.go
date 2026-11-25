package domain_test

import (
	"testing"
	"time"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/services/iam/domain"
)

const (
	userID = "tenantuseraccount-e00test0000000000001"
	saID   = "serviceaccount-e00test00000000000001"
)

type accessFixture struct {
	groups      *fakeGroups
	memberships *fakeMemberships
	permits     *fakePermits
	members     *domain.MembershipService
	grants      *domain.PermitService
	clk         *clock.Manual
	groupID     string
}

func newAccess(t *testing.T) *accessFixture {
	t.Helper()
	f := &accessFixture{
		groups:      &fakeGroups{byID: map[string]*iampb.Group{}},
		memberships: newFakeMemberships(),
		permits:     newFakePermits(),
		clk:         clock.NewManual(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
		groupID:     "group-e00test0000000000001",
	}
	f.groups.byID[f.groupID] = &iampb.Group{Metadata: &commonpb.ResourceMetadata{Id: f.groupID, ParentId: tenantID, Name: "g"}}
	ids := &seqIDs{}
	var err error
	if f.members, err = domain.NewMembershipService(f.memberships, f.groups, inlineOps{}, ids, f.clk); err != nil {
		t.Fatal(err)
	}
	f.grants, err = domain.NewPermitService(domain.PermitDeps{
		Permits: f.permits, Groups: f.groups, Roles: authz.DefaultCatalog(), Ops: inlineOps{}, IDs: ids, Clock: f.clk,
		Hierarchy: fakeHierarchy{known: map[string]bool{tenantID: true, f.groupID: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *accessFixture) addMember(t *testing.T, member string, revokeAfterHours int64) error {
	t.Helper()
	_, err := f.members.Create(t.Context(), domain.CreateMembershipCmd{
		Metadata:         &commonpb.ResourceMetadata{ParentId: f.groupID},
		Spec:             &iampb.GroupMembershipSpec{MemberId: member},
		RevokeAfterHours: revokeAfterHours,
	})
	return err
}

func (f *accessFixture) grant(t *testing.T, resourceID, role string) error {
	t.Helper()
	_, err := f.grants.Create(t.Context(), domain.CreatePermitCmd{
		Metadata: &commonpb.ResourceMetadata{ParentId: f.groupID},
		Spec:     &iampb.AccessPermitSpec{ResourceId: resourceID, Role: role},
	})
	return err
}

func TestMembershipValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		member string
		want   errs.Kind
	}{
		{"user account", userID, errs.KindUnknown},
		{"service account", saID, errs.KindUnknown},
		{"empty", "", errs.KindBadRequest},
		{"not a NID", "bob", errs.KindBadRequest},
		{"wrong kind", "group-e00test0000000000009", errs.KindBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newAccess(t)
			if got := errs.KindOf(f.addMember(t, tt.member, 0)); got != tt.want {
				t.Fatalf("kind = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPermitValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, resource, role string
		want                 errs.Kind
	}{
		{"tenant editor", tenantID, authz.RoleEditor, errs.KindUnknown},
		{"unknown role", tenantID, "superuser", errs.KindBadRequest},
		{"unknown resource", "group-e00test0000000000099", authz.RoleViewer, errs.KindNotFound},
		{"no resource", "", authz.RoleViewer, errs.KindBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newAccess(t)
			if got := errs.KindOf(f.grant(t, tt.resource, tt.role)); got != tt.want {
				t.Fatalf("kind = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBindingsRolesOn(t *testing.T) {
	t.Parallel()
	f := newAccess(t)
	if err := f.addMember(t, userID, 0); err != nil {
		t.Fatal(err)
	}
	if err := f.addMember(t, saID, 1); err != nil { // revoked after one hour
		t.Fatal(err)
	}
	if err := f.grant(t, tenantID, authz.RoleEditor); err != nil {
		t.Fatal(err)
	}
	b := domain.NewBindings(f.memberships, f.permits, f.clk)

	roles, err := b.RolesOn(t.Context(), userID, tenantID)
	if err != nil || len(roles) != 1 || roles[0] != authz.RoleEditor {
		t.Fatalf("user roles on tenant = %v, %v; want [editor]", roles, err)
	}
	if roles, _ := b.RolesOn(t.Context(), userID, f.groupID); len(roles) != 0 {
		t.Errorf("roles on the group itself = %v, want none (inheritance is the engine's job)", roles)
	}
	if roles, _ := b.RolesOn(t.Context(), saID, tenantID); len(roles) != 1 {
		t.Errorf("SA roles before revoke_at = %v, want [editor]", roles)
	}
	f.clk.Advance(2 * time.Hour)
	if roles, _ := b.RolesOn(t.Context(), saID, tenantID); len(roles) != 0 {
		t.Errorf("SA roles after revoke_at = %v, want none", roles)
	}
}

func TestGroupDeleteCascades(t *testing.T) {
	t.Parallel()
	f := newAccess(t)
	if err := f.addMember(t, userID, 0); err != nil {
		t.Fatal(err)
	}
	if err := f.grant(t, tenantID, authz.RoleViewer); err != nil {
		t.Fatal(err)
	}
	svc, err := domain.NewGroupService(domain.GroupDeps{
		Groups: f.groups, Tenants: fakeTenants{}, Memberships: f.memberships, Permits: f.permits,
		Ops: inlineOps{}, IDs: &seqIDs{}, Clock: f.clk,
	})
	if err != nil {
		t.Fatal(err)
	}
	g, err := svc.Get(t.Context(), f.groupID)
	if err != nil || g.GetStatus().GetMembersCount() != 1 || g.GetStatus().GetTenantUserAccountsCount() != 1 {
		t.Fatalf("group status = %v, %v; want 1 user member", g.GetStatus(), err)
	}
	if _, err := svc.Delete(t.Context(), f.groupID, nil); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(f.memberships.items) != 0 || len(f.permits.items) != 0 {
		t.Fatalf("children survived group delete: %d memberships, %d permits", len(f.memberships.items), len(f.permits.items))
	}
}
