//go:build e2e

package iam

import (
	"fmt"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"
	iamsvc "github.com/nebius/gosdk/services/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/iam"
)

// invitationConfig invites an engineer, makes the invited user a member of a
// group straight away (the docs example: member_id is the invitation's
// status.tenant_user_account_id), grants the group a role, and reads everything
// back through data sources.
func invitationConfig(tenant, email, description string) string {
	return fmt.Sprintf(`
resource "nebius_iam_v1_invitation" "eng" {
  parent_id   = %[1]q
  email       = %[2]q
  description = %[3]q
}

resource "nebius_iam_v1_group" "reviewers" {
  parent_id = %[1]q
  name      = "reviewers"
}

resource "nebius_iam_v1_group_membership" "eng_member" {
  parent_id = nebius_iam_v1_group.reviewers.id
  member_id = nebius_iam_v1_invitation.eng.status.tenant_user_account_id
}

resource "nebius_iam_v1_access_permit" "view_tenant" {
  parent_id   = nebius_iam_v1_group.reviewers.id
  resource_id = %[1]q
  role        = "viewer"
}

data "nebius_iam_v1_invitation" "by_id" {
  id = nebius_iam_v1_invitation.eng.id
}

data "nebius_iam_v1_tenant_user_account" "invited" {
  id = nebius_iam_v1_invitation.eng.status.tenant_user_account_id
}

data "nebius_iam_v1_tenant_user_account" "admin" {
  id = %[4]q
}

data "nebius_iam_v1_group_membership" "by_id" {
  id = nebius_iam_v1_group_membership.eng_member.id
}

data "nebius_iam_v1_access_permit" "by_id" {
  id = nebius_iam_v1_access_permit.view_tenant.id
}

output "inv_id"            { value = nebius_iam_v1_invitation.eng.id }
output "inv_state"         { value = nebius_iam_v1_invitation.eng.status.state }
output "inv_tua"           { value = nebius_iam_v1_invitation.eng.status.tenant_user_account_id }
output "ds_inv_id"         { value = data.nebius_iam_v1_invitation.by_id.id }
output "ds_inv_state"      { value = data.nebius_iam_v1_invitation.by_id.status.state }
output "ds_inv_tua"        { value = data.nebius_iam_v1_invitation.by_id.status.tenant_user_account_id }
output "ds_inv_expires"    { value = data.nebius_iam_v1_invitation.by_id.status.expires_at }
output "ds_inv_desc"       { value = data.nebius_iam_v1_invitation.by_id.description }
output "ds_inv_parent"     { value = data.nebius_iam_v1_invitation.by_id.parent_id }
output "ds_tua_id"         { value = data.nebius_iam_v1_tenant_user_account.invited.id }
output "ds_tua_state"      { value = data.nebius_iam_v1_tenant_user_account.invited.status.state }
output "ds_tua_inv"        { value = data.nebius_iam_v1_tenant_user_account.invited.status.invitation_id }
output "ds_tua_parent"     { value = data.nebius_iam_v1_tenant_user_account.invited.parent_id }
output "ds_admin_state"    { value = data.nebius_iam_v1_tenant_user_account.admin.status.state }
output "ds_admin_uas"      { value = data.nebius_iam_v1_tenant_user_account.admin.status.user_account_state }
output "membership_id"     { value = nebius_iam_v1_group_membership.eng_member.id }
output "ds_member_id"      { value = data.nebius_iam_v1_group_membership.by_id.id }
output "ds_member_member"  { value = data.nebius_iam_v1_group_membership.by_id.member_id }
output "ds_member_parent"  { value = data.nebius_iam_v1_group_membership.by_id.parent_id }
output "group_id"          { value = nebius_iam_v1_group.reviewers.id }
output "permit_id"         { value = nebius_iam_v1_access_permit.view_tenant.id }
output "ds_permit_id"      { value = data.nebius_iam_v1_access_permit.by_id.id }
output "ds_permit_role"    { value = data.nebius_iam_v1_access_permit.by_id.role }
output "ds_permit_res"     { value = data.nebius_iam_v1_access_permit.by_id.resource_id }
output "ds_permit_parent"  { value = data.nebius_iam_v1_access_permit.by_id.parent_id }
`, tenant, email, description, config.DefaultDevPrincipal)
}

// TestInvitationLifecycle invites a user with the official provider, reads the
// invitation, the invited tenant user account, the group membership and the
// access permit back through data sources, updates the invitation in place and
// destroys everything, checking the mock's state with the Go SDK after each step.
func TestInvitationLifecycle(t *testing.T) {
	inst := testkit.StartT(t, []mocker.Module{iam.New()}, mockerOptions(
		testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce))...)
	sdk := inst.SDK(t)
	invs := iamsvc.NewInvitationService(sdk)
	users := iamsvc.NewTenantUserAccountService(sdk)
	members := iamsvc.NewGroupMembershipService(sdk)
	ctx := t.Context()

	tf := newTerraform(t, inst)
	tf.writeConfig(t, invitationConfig(inst.TenantID, "ada@example.com", "first"))
	tf.init(t)
	tf.expectPlan(t, map[string]string{
		"eng": "create", "eng_member": "create", "reviewers": "create", "view_tenant": "create",
		"by_id": "read", "invited": "read",
	})
	tf.apply(t)
	tf.expectNoChanges(t)

	invID := tf.outputString(t, "inv_id")
	tuaID := tf.outputString(t, "inv_tua")
	inv, err := invs.Get(ctx, &iampb.GetInvitationRequest{Id: invID})
	if err != nil {
		t.Fatalf("Get invitation: %v", err)
	}
	if inv.GetSpec().GetEmail() != "ada@example.com" || inv.GetSpec().GetDescription() != "first" ||
		inv.GetStatus().GetState() != iampb.InvitationStatus_PENDING || inv.GetStatus().GetTenantUserAccountId() != tuaID ||
		inv.GetMetadata().GetParentId() != inst.TenantID {
		t.Errorf("invitation = %v", inv)
	}
	tua, err := users.Get(ctx, &iampb.GetTenantUserAccountRequest{Id: tuaID})
	if err != nil || tua.GetStatus().GetInvitationId() != invID || tua.GetStatus().GetState() != iampb.TenantUserAccountStatus_ACTIVE {
		t.Errorf("invited account = %v, %v", tua, err)
	}
	memID := tf.outputString(t, "membership_id")
	if m, err := members.Get(ctx, &iampb.GetGroupMembershipRequest{Id: memID}); err != nil || m.GetSpec().GetMemberId() != tuaID {
		t.Errorf("membership = %v, %v; want member %s", m, err, tuaID)
	}

	// The data sources return what the resources report.
	for out, want := range map[string]string{
		"inv_state": "PENDING", "ds_inv_id": invID, "ds_inv_state": "PENDING", "ds_inv_tua": tuaID, "ds_inv_desc": "first",
		"ds_inv_parent": inst.TenantID,
		"ds_tua_id":     tuaID, "ds_tua_state": "ACTIVE", "ds_tua_inv": invID, "ds_tua_parent": inst.TenantID,
		"ds_admin_state": "ACTIVE", "ds_admin_uas": "ACTIVE",
		"ds_member_id": memID, "ds_member_member": tuaID, "ds_member_parent": tf.outputString(t, "group_id"),
		"ds_permit_id": tf.outputString(t, "permit_id"), "ds_permit_role": "viewer", "ds_permit_res": inst.TenantID,
		"ds_permit_parent": tf.outputString(t, "group_id"),
	} {
		if got := tf.outputString(t, out); got != want {
			t.Errorf("output %s = %q, want %q", out, got, want)
		}
	}

	expires, err := time.Parse(time.RFC3339Nano, tf.outputString(t, "ds_inv_expires"))
	if err != nil || !expires.Equal(inv.GetStatus().GetExpiresAt().AsTime()) {
		t.Errorf("data source expires_at = %v (%v), want %v", expires, err, inv.GetStatus().GetExpiresAt().AsTime())
	}

	// In-place update of description and email keeps the invitation and its
	// account. The provider marks status unknown on every update, so the
	// membership (whose immutable member_id reads status) is replaced, exactly
	// as it would be against the real API.
	tf.writeConfig(t, invitationConfig(inst.TenantID, "grace@example.com", "second"))
	tf.expectPlan(t, map[string]string{
		"eng": "update", "eng_member": "replace", "reviewers": "no-op", "view_tenant": "no-op",
		"by_id": "read", "invited": "read",
	})
	tf.apply(t)
	tf.expectNoChanges(t)
	if got := tf.outputString(t, "inv_id"); got != invID {
		t.Errorf("in-place update changed the invitation id: %s -> %s", invID, got)
	}
	if got := tf.outputString(t, "inv_tua"); got != tuaID {
		t.Errorf("in-place update changed the tenant user account: %s -> %s", tuaID, got)
	}
	inv, err = invs.Get(ctx, &iampb.GetInvitationRequest{Id: invID})
	if err != nil || inv.GetSpec().GetEmail() != "grace@example.com" || inv.GetSpec().GetDescription() != "second" ||
		inv.GetStatus().GetState() != iampb.InvitationStatus_PENDING {
		t.Errorf("invitation after update = %v, %v", inv, err)
	}
	oldMem := memID
	if memID = tf.outputString(t, "membership_id"); memID == oldMem {
		t.Errorf("membership was not replaced")
	}
	if m, err := members.Get(ctx, &iampb.GetGroupMembershipRequest{Id: memID}); err != nil || m.GetSpec().GetMemberId() != tuaID {
		t.Errorf("replacement membership = %v, %v; want member %s", m, err, tuaID)
	}
	if _, err := members.Get(ctx, &iampb.GetGroupMembershipRequest{Id: oldMem}); status.Code(err) != codes.NotFound {
		t.Errorf("replaced membership Get: %v, want NotFound", err)
	}
	if got := tf.outputString(t, "ds_inv_desc"); got != "second" {
		t.Errorf("data source description after update = %q", got)
	}

	// Destroy removes the invitation, the invited account and its membership.
	tf.destroy(t)
	_, err = invs.Get(ctx, &iampb.GetInvitationRequest{Id: invID})
	if status.Code(err) != codes.NotFound {
		t.Errorf("invitation Get after destroy: %v, want NotFound", err)
	}
	_, err = users.Get(ctx, &iampb.GetTenantUserAccountRequest{Id: tuaID})
	if status.Code(err) != codes.NotFound {
		t.Errorf("tenant user account Get after destroy: %v, want NotFound", err)
	}
	_, err = members.Get(ctx, &iampb.GetGroupMembershipRequest{Id: memID})
	if status.Code(err) != codes.NotFound {
		t.Errorf("membership Get after destroy: %v, want NotFound", err)
	}
	if page, err := invs.List(ctx, &iampb.ListInvitationsRequest{ParentId: inst.TenantID}); err != nil || len(page.GetItems()) != 0 {
		t.Errorf("invitations after destroy = %v, %v", page.GetItems(), err)
	}
}
