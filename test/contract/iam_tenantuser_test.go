package contract

import (
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"

	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/config"
)

func TestTenantUserAccountReads(t *testing.T) {
	t.Parallel()
	e := startInvEnv(t)
	ctx := t.Context()
	seeded, err := e.users.Get(ctx, &iampb.GetTenantUserAccountRequest{Id: config.DefaultDevPrincipal})
	if err != nil {
		t.Fatalf("Get seeded dev user: %v", err)
	}
	if ts := seeded.GetStatus(); ts.GetState() != iampb.TenantUserAccountStatus_ACTIVE || ts.GetUserAccountState() != iampb.UserAccountStatus_ACTIVE ||
		ts.GetInvitationId() != "" || seeded.GetMetadata().GetParentId() != e.inst.TenantID {
		t.Fatalf("seeded user = %v", seeded)
	}
	_, err = e.users.Get(ctx, &iampb.GetTenantUserAccountRequest{Id: "tenantuseraccount-e00mock0000000nobody"})
	assertCode(t, err, codes.NotFound)

	ids := func(items []*iampb.TenantUserAccount) map[string]bool {
		m := map[string]bool{}
		for _, a := range items {
			m[a.GetMetadata().GetId()] = true
		}
		return m
	}
	list, err := e.users.List(ctx, &iampb.ListTenantUserAccountsRequest{ParentId: e.inst.TenantID})
	got := ids(list.GetItems())
	if err != nil || !got[config.DefaultDevPrincipal] || !got[config.DefaultEditorPrincipal] || !got[config.DefaultViewerPrincipal] || !got[invBlockedID] {
		t.Fatalf("List = %v, %v; want every static tenant user principal", got, err)
	}
	paged, err := e.users.List(ctx, &iampb.ListTenantUserAccountsRequest{ParentId: e.inst.TenantID, PageSize: 1})
	if err != nil || len(paged.GetItems()) != 1 || paged.GetNextPageToken() == "" {
		t.Fatalf("paged List = %v, %v", paged.GetItems(), err)
	}
	_, err = e.users.List(ctx, &iampb.ListTenantUserAccountsRequest{ParentId: e.inst.TenantID, Filter: "x"})
	authKeyWantInvalid(t, "List filter", err, "filter")

	one, err := e.attrs.Get(ctx, &iampb.GetTenantUserAccountWithAttributesRequest{Id: config.DefaultDevPrincipal})
	if err != nil || one.GetAttributes().GetSub() != config.DefaultDevPrincipal || !proto.Equal(one.GetTenantUserAccount(), seeded) {
		t.Fatalf("GetWithAttributes = %v, %v", one, err)
	}
	all, err := e.attrs.List(ctx, &iampb.ListTenantUserAccountsWithAttributesRequest{ParentId: e.inst.TenantID})
	if err != nil || len(all.GetItems()) != len(list.GetItems()) {
		t.Fatalf("List with attributes = %d items, %v; want %d", len(all.GetItems()), err, len(list.GetItems()))
	}
	_, err = e.attrs.List(ctx, &iampb.ListTenantUserAccountsWithAttributesRequest{ParentId: e.inst.TenantID, Filter: "x"})
	authKeyWantInvalid(t, "List with attributes filter", err, "filter")
	_, err = e.attrs.Get(ctx, &iampb.GetTenantUserAccountWithAttributesRequest{Id: "tenantuseraccount-e00mock0000000nobody"})
	assertCode(t, err, codes.NotFound)
}

func TestTenantUserAccountBlock(t *testing.T) {
	t.Parallel()
	e := startInvEnv(t)
	ctx := t.Context()
	if err := e.addMember(t, e.group(t, "admins"), invBlockedID); err != nil {
		t.Fatal(err)
	}
	block := func(what string) {
		t.Helper()
		op, err := e.users.Block(ctx, &iampb.BlockTenantUserAccountRequest{Id: invBlockedID})
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if _, err := op.Wait(ctx); err != nil {
			t.Fatalf("%s wait: %v", what, err)
		}
	}
	unblock := func() {
		t.Helper()
		op, err := e.users.Unblock(ctx, &iampb.UnblockTenantUserAccountRequest{Id: invBlockedID})
		if err != nil {
			t.Fatalf("Unblock: %v", err)
		}
		if _, err := op.Wait(ctx); err != nil {
			t.Fatalf("Unblock wait: %v", err)
		}
	}
	list := &iampb.ListGroupsRequest{ParentId: e.inst.TenantID}
	if _, err := e.blockedGroups.List(ctx, list); err != nil {
		t.Fatalf("admin member before Block: %v", err)
	}

	block("Block")
	if a, _ := e.users.Get(ctx, &iampb.GetTenantUserAccountRequest{Id: invBlockedID}); a.GetStatus().GetState() != iampb.TenantUserAccountStatus_BLOCKED {
		t.Fatalf("state after Block = %v", a.GetStatus().GetState())
	}
	_, err := e.blockedGroups.List(ctx, list)
	assertCode(t, err, codes.PermissionDenied)
	// The membership shows the member's current status.
	admins := e.group(t, "admins")
	ms, err := e.members.ListMembers(ctx, &iampb.ListGroupMembershipsRequest{ParentId: admins})
	if err != nil {
		t.Fatal(err)
	}
	var seen bool
	for _, m := range ms.GetMemberships() {
		if m.GetSpec().GetMemberId() != invBlockedID {
			continue
		}
		seen = true
		if got := m.GetStatus().GetTenantUserAccountStatus().GetState(); got != iampb.TenantUserAccountStatus_BLOCKED {
			t.Errorf("ListMembers status of the blocked member = %v, want BLOCKED", got)
		}
		one, err := e.members.Get(ctx, &iampb.GetGroupMembershipRequest{Id: m.GetMetadata().GetId()})
		if err != nil || one.GetStatus().GetTenantUserAccountStatus().GetState() != iampb.TenantUserAccountStatus_BLOCKED {
			t.Errorf("Get status = %v, %v; want BLOCKED", one.GetStatus(), err)
		}
		wa, err := e.members.GetWithAttributes(ctx, &iampb.GetGroupMembershipRequest{Id: m.GetMetadata().GetId()})
		if err != nil || wa.GetGroupMembership().GetStatus().GetTenantUserAccountStatus().GetState() != iampb.TenantUserAccountStatus_BLOCKED {
			t.Errorf("GetWithAttributes status = %v, %v; want BLOCKED", wa.GetGroupMembership().GetStatus(), err)
		}
	}
	if !seen {
		t.Fatalf("blocked member missing from the admins group")
	}

	block("Block again")
	unblock()
	if _, err := e.blockedGroups.List(ctx, list); err != nil {
		t.Fatalf("after Unblock: %v", err)
	}
	if a, _ := e.users.Get(ctx, &iampb.GetTenantUserAccountRequest{Id: invBlockedID}); a.GetStatus().GetState() != iampb.TenantUserAccountStatus_ACTIVE {
		t.Fatalf("state after Unblock = %v", a.GetStatus().GetState())
	}

	_, err = e.users.Block(ctx, &iampb.BlockTenantUserAccountRequest{Id: "tenantuseraccount-e00mock0000000nobody"})
	assertCode(t, err, codes.NotFound)
	_, err = e.users.Unblock(ctx, &iampb.UnblockTenantUserAccountRequest{Id: "tenantuseraccount-e00mock0000000nobody"})
	assertCode(t, err, codes.NotFound)
}
