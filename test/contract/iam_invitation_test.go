package contract

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/nebius/gosdk/proto/fieldmask/grpcheader"
	"github.com/nebius/gosdk/proto/fieldmask/mask"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"
	iamsvc "github.com/nebius/gosdk/services/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/iam"
)

const (
	invBlockedToken = "blocked-user-token"
	invBlockedID    = "tenantuseraccount-e00mock00000000blocked"
)

// invEnv is a running enforcing IAM module on a manual clock, with SDK clients
// per role.
type invEnv struct {
	inst *testkit.Instance
	clk  *clock.Manual

	invs, editorInvs, viewerInvs iamsvc.InvitationService
	users, editorUsers           iamsvc.TenantUserAccountService
	viewerUsers                  iamsvc.TenantUserAccountService
	attrs                        iamsvc.TenantUserAccountWithAttributesService
	viewerAttrs                  iamsvc.TenantUserAccountWithAttributesService
	groups                       iamsvc.GroupService
	blockedGroups                iamsvc.GroupService
	members                      iamsvc.GroupMembershipService
	feds                         iamsvc.FederationService
}

func startInvEnv(t *testing.T) *invEnv {
	t.Helper()
	clk := clock.NewManual(time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC))
	inst := testkit.StartT(t, []mocker.Module{iam.New()},
		testkit.WithAuthz(authz.ModeEnforce), testkit.WithClock(clk),
		testkit.WithTokens(map[string]string{invBlockedToken: invBlockedID}))
	admin := inst.SDKAs(t, config.DefaultDevToken)
	editor := inst.SDKAs(t, config.DefaultEditorToken)
	viewer := inst.SDKAs(t, config.DefaultViewerToken)
	return &invEnv{
		inst: inst, clk: clk,
		invs: iamsvc.NewInvitationService(admin), editorInvs: iamsvc.NewInvitationService(editor), viewerInvs: iamsvc.NewInvitationService(viewer),
		users: iamsvc.NewTenantUserAccountService(admin), editorUsers: iamsvc.NewTenantUserAccountService(editor), viewerUsers: iamsvc.NewTenantUserAccountService(viewer),
		attrs: iamsvc.NewTenantUserAccountWithAttributesService(admin), viewerAttrs: iamsvc.NewTenantUserAccountWithAttributesService(viewer),
		groups: iamsvc.NewGroupService(admin), blockedGroups: iamsvc.NewGroupService(inst.SDKAs(t, invBlockedToken)),
		members: iamsvc.NewGroupMembershipService(admin), feds: iamsvc.NewFederationService(admin),
	}
}

func invReq(tenantID, email string) *iampb.CreateInvitationRequest {
	return &iampb.CreateInvitationRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: tenantID},
		Spec:     &iampb.InvitationSpec{Contact: &iampb.InvitationSpec_Email{Email: email}},
	}
}

func (e *invEnv) invite(t *testing.T, req *iampb.CreateInvitationRequest) *iampb.Invitation {
	t.Helper()
	op, err := e.invs.Create(t.Context(), req)
	if err != nil {
		t.Fatalf("Create invitation: %v", err)
	}
	if _, err := op.Wait(t.Context()); err != nil {
		t.Fatalf("Create invitation wait: %v", err)
	}
	return e.getInv(t, op.ResourceID())
}

func (e *invEnv) getInv(t *testing.T, id string) *iampb.Invitation {
	t.Helper()
	inv, err := e.invs.Get(t.Context(), &iampb.GetInvitationRequest{Id: id})
	if err != nil {
		t.Fatalf("Get invitation %s: %v", id, err)
	}
	return inv
}

func (e *invEnv) resend(t *testing.T, id string) {
	t.Helper()
	op, err := e.invs.Resend(t.Context(), &iampb.ResendInvitationRequest{Id: id})
	if err != nil {
		t.Fatalf("Resend: %v", err)
	}
	if _, err := op.Wait(t.Context()); err != nil {
		t.Fatalf("Resend wait: %v", err)
	}
}

// accept calls the mock's invitation acceptance endpoint like the invitee's
// browser would, and returns the status code and decoded JSON body.
func (e *invEnv) accept(t *testing.T, id, federationID string) (code int, body map[string]string) {
	t.Helper()
	form := url.Values{}
	if federationID != "" {
		form.Set("federation_id", federationID)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		"http://"+e.inst.Addr+"/_mocker/iam/invitations/"+id+"/accept", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req) // no Authorization header: the invitee has no token
	if err != nil {
		t.Fatalf("accept request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	body = map[string]string{}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("accept response %q is not a JSON object: %v", raw, err)
	}
	return resp.StatusCode, body
}

func (e *invEnv) addMember(t *testing.T, groupID, memberID string) error {
	t.Helper()
	op, err := e.members.Create(t.Context(), &iampb.CreateGroupMembershipRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: groupID}, Spec: &iampb.GroupMembershipSpec{MemberId: memberID},
	})
	if err != nil {
		return err
	}
	_, err = op.Wait(t.Context())
	return err
}

func (e *invEnv) group(t *testing.T, name string) string {
	t.Helper()
	g, err := e.groups.GetByName(t.Context(), &iampb.GetGroupByNameRequest{ParentId: e.inst.TenantID, Name: name})
	if err != nil {
		t.Fatalf("group %s: %v", name, err)
	}
	return g.GetMetadata().GetId()
}

func TestInvitationCreateGetListDelete(t *testing.T) {
	t.Parallel()
	e := startInvEnv(t)
	ctx := t.Context()
	req := invReq(e.inst.TenantID, "ada@example.com")
	req.Metadata.Name = "ada"
	req.Metadata.Labels = map[string]string{"team": "ml"}
	req.Spec.Description = "welcome"
	inv := e.invite(t, req)
	id := inv.GetMetadata().GetId()
	if !strings.HasPrefix(id, "invitation-") {
		t.Fatalf("id = %q, want an invitation- prefix", id)
	}
	if md := inv.GetMetadata(); md.GetName() != "ada" || md.GetLabels()["team"] != "ml" || md.GetParentId() != e.inst.TenantID || md.GetResourceVersion() != 1 {
		t.Errorf("metadata = %v", md)
	}
	if sp := inv.GetSpec(); sp.GetEmail() != "ada@example.com" || sp.GetDescription() != "welcome" {
		t.Errorf("spec = %v", sp)
	}
	st := inv.GetStatus()
	if st.GetState() != iampb.InvitationStatus_PENDING || !st.GetExpiresAt().AsTime().Equal(e.clk.Now().Add(72*time.Hour)) {
		t.Errorf("status = %v, want PENDING expiring in 72h", st)
	}

	tua, err := e.users.Get(ctx, &iampb.GetTenantUserAccountRequest{Id: st.GetTenantUserAccountId()})
	if err != nil {
		t.Fatalf("the invitation's tenant user account: %v", err)
	}
	if ts := tua.GetStatus(); ts.GetInvitationId() != id || ts.GetState() != iampb.TenantUserAccountStatus_ACTIVE ||
		ts.GetUserAccountState() != iampb.UserAccountStatus_STATE_UNSPECIFIED || tua.GetMetadata().GetParentId() != e.inst.TenantID {
		t.Errorf("tenant user account = %v", tua)
	}

	// Listing: one per page, then everything; filters are refused.
	second := e.invite(t, invReq(e.inst.TenantID, "grace@example.com"))
	var seen []string
	token := ""
	for range 5 {
		page, err := e.invs.List(ctx, &iampb.ListInvitationsRequest{ParentId: e.inst.TenantID, PageSize: 1, PageToken: token})
		if err != nil {
			t.Fatal(err)
		}
		for _, i := range page.GetItems() {
			seen = append(seen, i.GetMetadata().GetId())
		}
		if token = page.GetNextPageToken(); token == "" {
			break
		}
	}
	if len(seen) != 2 || seen[0] != id || seen[1] != second.GetMetadata().GetId() {
		t.Errorf("paged List = %v, want [%s %s]", seen, id, second.GetMetadata().GetId())
	}
	_, err = e.invs.List(ctx, &iampb.ListInvitationsRequest{ParentId: e.inst.TenantID, Filter: "x"})
	authKeyWantInvalid(t, "List filter", err, "filter")

	// Deleting a pending invitation cancels it and removes the invitee's account.
	op, err := e.invs.Delete(ctx, &iampb.DeleteInvitationRequest{Id: id})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	_, err = e.invs.Get(ctx, &iampb.GetInvitationRequest{Id: id})
	assertCode(t, err, codes.NotFound)
	_, err = e.users.Get(ctx, &iampb.GetTenantUserAccountRequest{Id: st.GetTenantUserAccountId()})
	assertCode(t, err, codes.NotFound)
	_, err = e.invs.Delete(ctx, &iampb.DeleteInvitationRequest{Id: id})
	assertCode(t, err, codes.NotFound)
}

func TestInvitationCreateValidation(t *testing.T) {
	t.Parallel()
	e := startInvEnv(t)
	ctx := t.Context()
	for name, tc := range map[string]struct {
		req   *iampb.CreateInvitationRequest
		field string
	}{
		"no email":     {&iampb.CreateInvitationRequest{Metadata: &commonpb.ResourceMetadata{ParentId: e.inst.TenantID}, Spec: &iampb.InvitationSpec{}}, "spec.email"},
		"display name": {invReq(e.inst.TenantID, "Ada <ada@example.com>"), "spec.email"},
		"not an email": {invReq(e.inst.TenantID, "ada"), "spec.email"},
		"zero expiry": {func() *iampb.CreateInvitationRequest {
			r := invReq(e.inst.TenantID, "a@example.com")
			r.ExpiresIn = durationpb.New(0)
			return r
		}(), "expires_in"},
		"negative expiry": {func() *iampb.CreateInvitationRequest {
			r := invReq(e.inst.TenantID, "a@example.com")
			r.ExpiresIn = durationpb.New(-time.Hour)
			return r
		}(), "expires_in"},
	} {
		_, err := e.invs.Create(ctx, tc.req)
		authKeyWantInvalid(t, name, err, tc.field)
	}
	_, err := e.invs.Create(ctx, invReq("tenant-e00mock0000000nobody", "a@example.com"))
	assertCode(t, err, codes.NotFound)
	_, err = e.invs.Create(ctx, invReq(e.inst.ProjectID, "a@example.com"))
	assertCode(t, err, codes.NotFound) // a project is not a tenant
	_, err = e.invs.Get(ctx, &iampb.GetInvitationRequest{Id: "invitation-e00mock0000000nobody"})
	assertCode(t, err, codes.NotFound)
	_, err = e.invs.Resend(ctx, &iampb.ResendInvitationRequest{Id: "invitation-e00mock0000000nobody"})
	assertCode(t, err, codes.NotFound)
}

func TestInvitationExpiryAndResend(t *testing.T) {
	t.Parallel()
	e := startInvEnv(t)
	req := invReq(e.inst.TenantID, "ada@example.com")
	req.ExpiresIn = durationpb.New(2 * time.Hour)
	inv := e.invite(t, req)
	id := inv.GetMetadata().GetId()
	if got := inv.GetStatus().GetExpiresAt().AsTime(); !got.Equal(e.clk.Now().Add(2 * time.Hour)) {
		t.Fatalf("expires_at = %v, want now+2h", got)
	}

	e.clk.Advance(2 * time.Hour)
	if got := e.getInv(t, id).GetStatus().GetState(); got != iampb.InvitationStatus_EXPIRED {
		t.Fatalf("state after the window = %v, want EXPIRED", got)
	}
	page, err := e.invs.List(t.Context(), &iampb.ListInvitationsRequest{ParentId: e.inst.TenantID})
	if err != nil || len(page.GetItems()) != 1 || page.GetItems()[0].GetStatus().GetState() != iampb.InvitationStatus_EXPIRED {
		t.Fatalf("List = %v, %v; want one EXPIRED item", page.GetItems(), err)
	}
	if code, body := e.accept(t, id, ""); code != http.StatusConflict {
		t.Fatalf("accept of an expired invitation: %d %v, want 409", code, body)
	}

	// Resend restarts the original 2h window.
	e.resend(t, id)
	got := e.getInv(t, id)
	if got.GetStatus().GetState() != iampb.InvitationStatus_PENDING || !got.GetStatus().GetExpiresAt().AsTime().Equal(e.clk.Now().Add(2*time.Hour)) {
		t.Fatalf("status after Resend = %v", got.GetStatus())
	}

	// no_send: CREATED until resent.
	noSend := invReq(e.inst.TenantID, "grace@example.com")
	noSend.NoSend = true
	unsent := e.invite(t, noSend)
	uid := unsent.GetMetadata().GetId()
	if unsent.GetStatus().GetState() != iampb.InvitationStatus_CREATED {
		t.Fatalf("no_send state = %v, want CREATED", unsent.GetStatus().GetState())
	}
	if code, body := e.accept(t, uid, ""); code != http.StatusConflict || !strings.Contains(body["error"], "Resend") {
		t.Fatalf("accept of an unsent invitation: %d %v, want 409 pointing at Resend", code, body)
	}
	e.resend(t, uid)
	if got := e.getInv(t, uid).GetStatus().GetState(); got != iampb.InvitationStatus_PENDING {
		t.Fatalf("state after Resend = %v, want PENDING", got)
	}

	// An accepted invitation cannot be resent.
	if code, _ := e.accept(t, uid, ""); code != http.StatusOK {
		t.Fatalf("accept: %d", code)
	}
	_, err = e.invs.Resend(t.Context(), &iampb.ResendInvitationRequest{Id: uid})
	assertCode(t, err, codes.FailedPrecondition)
}

func TestInvitationUpdate(t *testing.T) {
	t.Parallel()
	e := startInvEnv(t)
	ctx := t.Context()
	req := invReq(e.inst.TenantID, "ada@example.com")
	req.Spec.Description = "first"
	inv := e.invite(t, req)
	id, tuaID := inv.GetMetadata().GetId(), inv.GetStatus().GetTenantUserAccountId()
	update := func(md *commonpb.ResourceMetadata, spec *iampb.InvitationSpec, reset string) error {
		c := ctx
		if reset != "" {
			var err error
			if c, err = grpcheader.AddResetMaskToOutgoingContext(ctx, mask.ParseMust(reset)); err != nil {
				t.Fatal(err)
			}
		}
		md.Id = id
		op, err := e.invs.Update(c, &iampb.UpdateInvitationRequest{Metadata: md, Spec: spec})
		if err != nil {
			return err
		}
		_, err = op.Wait(ctx)
		return err
	}

	if err := update(&commonpb.ResourceMetadata{Name: "renamed", Labels: map[string]string{"a": "b"}},
		&iampb.InvitationSpec{Description: "second", Contact: &iampb.InvitationSpec_Email{Email: "grace@example.com"}}, ""); err != nil {
		t.Fatal(err)
	}
	got := e.getInv(t, id)
	if got.GetMetadata().GetName() != "renamed" || got.GetMetadata().GetLabels()["a"] != "b" || got.GetSpec().GetDescription() != "second" ||
		got.GetSpec().GetEmail() != "grace@example.com" || got.GetMetadata().GetResourceVersion() != 2 {
		t.Fatalf("after update: %v", got)
	}
	if got.GetStatus().GetState() != iampb.InvitationStatus_PENDING || got.GetStatus().GetTenantUserAccountId() != tuaID {
		t.Fatalf("update changed the status: %v", got.GetStatus())
	}
	withAttrs, err := e.attrs.Get(ctx, &iampb.GetTenantUserAccountWithAttributesRequest{Id: tuaID})
	if err != nil || withAttrs.GetAttributes().GetEmail() != "grace@example.com" {
		t.Fatalf("attributes after the email change = %v, %v", withAttrs.GetAttributes(), err)
	}

	if err := update(&commonpb.ResourceMetadata{Name: "renamed"}, &iampb.InvitationSpec{Contact: &iampb.InvitationSpec_Email{Email: "grace@example.com"}}, "spec.description,metadata.labels"); err != nil {
		t.Fatal(err)
	}
	if got = e.getInv(t, id); got.GetSpec().GetDescription() != "" || len(got.GetMetadata().GetLabels()) != 0 {
		t.Fatalf("masked update left description/labels: %v", got)
	}
	authKeyWantInvalid(t, "garbled email", update(&commonpb.ResourceMetadata{}, emailOnly("nope"), ""), "spec.email")
	authKeyWantInvalid(t, "cleared email", update(&commonpb.ResourceMetadata{}, &iampb.InvitationSpec{}, "spec.email"), "spec.email")
	assertCode(t, update(&commonpb.ResourceMetadata{ResourceVersion: 99}, emailOnly("x@example.com"), ""), codes.Aborted)

	// After acceptance the email is fixed, the description is not.
	if code, _ := e.accept(t, id, ""); code != http.StatusOK {
		t.Fatalf("accept: %d", code)
	}
	assertCode(t, update(&commonpb.ResourceMetadata{Name: "renamed"}, emailOnly("late@example.com"), ""), codes.FailedPrecondition)
	if err := update(&commonpb.ResourceMetadata{Name: "renamed"}, &iampb.InvitationSpec{Description: "kept", Contact: &iampb.InvitationSpec_Email{Email: "grace@example.com"}}, ""); err != nil {
		t.Fatalf("description change after accept: %v", err)
	}
}

func emailOnly(email string) *iampb.InvitationSpec {
	return &iampb.InvitationSpec{Contact: &iampb.InvitationSpec_Email{Email: email}}
}

func TestInvitationAcceptEndpoint(t *testing.T) {
	t.Parallel()
	e := startInvEnv(t)
	ctx := t.Context()
	inv := e.invite(t, invReq(e.inst.TenantID, "ada@example.com"))
	id, tuaID := inv.GetMetadata().GetId(), inv.GetStatus().GetTenantUserAccountId()

	if code, body := e.accept(t, "invitation-e00mock0000000nobody", ""); code != http.StatusNotFound || body["error"] == "" {
		t.Fatalf("accept of unknown: %d %v, want 404 with an error", code, body)
	}
	if code, _ := e.accept(t, id, "federation-e00mock0000000nobody"); code != http.StatusNotFound {
		t.Fatalf("accept with an unknown federation: %d, want 404", code)
	}
	if got := e.getInv(t, id).GetStatus().GetState(); got != iampb.InvitationStatus_PENDING {
		t.Fatalf("failed accepts changed the state to %v", got)
	}

	fedOp, err := e.feds.Create(ctx, &iampb.CreateFederationRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: e.inst.TenantID, Name: "okta"},
		Spec: &iampb.FederationSpec{Settings: &iampb.FederationSpec_SamlSettings{SamlSettings: &iampb.SamlSettings{
			IdpIssuer: "https://idp.example.com", SsoUrl: "https://idp.example.com/sso",
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fedOp.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	fedID := fedOp.ResourceID()

	code, body := e.accept(t, id, fedID)
	if code != http.StatusOK || body["invitation_id"] != id || body["tenant_user_account_id"] != tuaID || body["state"] != "ACCEPTED" {
		t.Fatalf("accept = %d %v", code, body)
	}
	if got := e.getInv(t, id).GetStatus().GetState(); got != iampb.InvitationStatus_ACCEPTED {
		t.Fatalf("state = %v, want ACCEPTED", got)
	}
	tua, err := e.users.Get(ctx, &iampb.GetTenantUserAccountRequest{Id: tuaID})
	if err != nil {
		t.Fatal(err)
	}
	if ts := tua.GetStatus(); ts.GetInvitationId() != "" || ts.GetFederationId() != fedID || ts.GetUserAccountState() != iampb.UserAccountStatus_ACTIVE {
		t.Fatalf("account after accept = %v", ts)
	}
	fed, err := e.feds.Get(ctx, &iampb.GetFederationRequest{Id: fedID})
	if err != nil || fed.GetStatus().GetUsersCount() != 1 {
		t.Fatalf("federation users_count = %v, %v; want 1", fed.GetStatus().GetUsersCount(), err)
	}
	if code, _ := e.accept(t, id, ""); code != http.StatusConflict {
		t.Fatalf("second accept: %d, want 409", code)
	}

	// Deleting an accepted invitation leaves the (real) user behind.
	op, err := e.invs.Delete(ctx, &iampb.DeleteInvitationRequest{Id: id})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := e.users.Get(ctx, &iampb.GetTenantUserAccountRequest{Id: tuaID}); err != nil {
		t.Fatalf("accepted user after invitation delete: %v", err)
	}
}

func TestInvitationMembership(t *testing.T) {
	t.Parallel()
	e := startInvEnv(t)
	ctx := t.Context()
	editors := e.group(t, "editors")

	assertCode(t, e.addMember(t, editors, "tenantuseraccount-e00mock0000000nobody"), codes.NotFound)

	req := invReq(e.inst.TenantID, "ada@example.com")
	req.ExpiresIn = durationpb.New(time.Hour)
	inv := e.invite(t, req)
	tuaID := inv.GetStatus().GetTenantUserAccountId()
	if err := e.addMember(t, editors, tuaID); err != nil {
		t.Fatalf("adding a not-yet-accepted invited user: %v", err)
	}
	page, err := e.members.ListMembersWithAttributes(ctx, &iampb.ListGroupMembershipsRequest{ParentId: editors})
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]*iampb.GroupMembershipWithAttributes{}
	for _, m := range page.GetMemberships() {
		kinds[m.GetGroupMembership().GetSpec().GetMemberId()] = m
	}
	if m := kinds[tuaID]; m.GetGroupMemberKind().GetKind() != iampb.GroupMemberKind_INVITED_TENANT_USER_ACCOUNT || m.GetUserAttributes().GetEmail() != "ada@example.com" {
		t.Errorf("invited member = %v", m)
	}
	if m := kinds[config.DefaultEditorPrincipal]; m.GetGroupMemberKind().GetKind() != iampb.GroupMemberKind_ORDINARY_TENANT_USER_ACCOUNT || m.GetUserAttributes().GetSub() != config.DefaultEditorPrincipal {
		t.Errorf("seeded editor member = %v", m)
	}
	if st := kinds[tuaID].GetGroupMembership().GetStatus().GetTenantUserAccountStatus(); st.GetInvitationId() != inv.GetMetadata().GetId() {
		t.Errorf("invited member's membership status = %v, want invitation_id %s", st, inv.GetMetadata().GetId())
	}
	one, err := e.members.GetWithAttributes(ctx, &iampb.GetGroupMembershipRequest{Id: kinds[tuaID].GetGroupMembership().GetMetadata().GetId()})
	if err != nil || one.GetUserAttributes().GetEmail() != "ada@example.com" {
		t.Errorf("GetWithAttributes = %v, %v", one, err)
	}
	_, err = e.members.GetWithAttributes(ctx, &iampb.GetGroupMembershipRequest{Id: "groupmembership-e00mock0000000nobody"})
	assertCode(t, err, codes.NotFound)

	// Once the invitation expires its user cannot be added to groups until it is resent.
	admins := e.group(t, "admins")
	e.clk.Advance(2 * time.Hour)
	assertCode(t, e.addMember(t, admins, tuaID), codes.FailedPrecondition)
	e.resend(t, inv.GetMetadata().GetId())
	if err := e.addMember(t, admins, tuaID); err != nil {
		t.Fatalf("adding after Resend: %v", err)
	}

	// Deleting the invitation drops the user's memberships.
	op, err := e.invs.Delete(ctx, &iampb.DeleteInvitationRequest{Id: inv.GetMetadata().GetId()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	memberOf, err := e.members.ListMemberOf(ctx, &iampb.ListMemberOfRequest{SubjectId: tuaID})
	if err != nil || len(memberOf.GetItems()) != 0 {
		t.Fatalf("groups of a deleted invitee = %v, %v; want none", memberOf.GetItems(), err)
	}
}

func TestInvitationAndUserAuthz(t *testing.T) {
	t.Parallel()
	e := startInvEnv(t)
	ctx := t.Context()
	inv := e.invite(t, invReq(e.inst.TenantID, "ada@example.com"))
	id, tuaID := inv.GetMetadata().GetId(), inv.GetStatus().GetTenantUserAccountId()

	// Viewer: reads only.
	if _, err := e.viewerInvs.Get(ctx, &iampb.GetInvitationRequest{Id: id}); err != nil {
		t.Errorf("viewer Get invitation: %v", err)
	}
	if _, err := e.viewerInvs.List(ctx, &iampb.ListInvitationsRequest{ParentId: e.inst.TenantID}); err != nil {
		t.Errorf("viewer List invitations: %v", err)
	}
	if _, err := e.viewerUsers.Get(ctx, &iampb.GetTenantUserAccountRequest{Id: tuaID}); err != nil {
		t.Errorf("viewer Get tenant user account: %v", err)
	}
	if _, err := e.viewerUsers.List(ctx, &iampb.ListTenantUserAccountsRequest{ParentId: e.inst.TenantID}); err != nil {
		t.Errorf("viewer List tenant user accounts: %v", err)
	}
	if _, err := e.viewerAttrs.Get(ctx, &iampb.GetTenantUserAccountWithAttributesRequest{Id: tuaID}); err != nil {
		t.Errorf("viewer Get with attributes: %v", err)
	}
	if _, err := e.viewerAttrs.List(ctx, &iampb.ListTenantUserAccountsWithAttributesRequest{ParentId: e.inst.TenantID}); err != nil {
		t.Errorf("viewer List with attributes: %v", err)
	}
	_, err := e.viewerInvs.Create(ctx, invReq(e.inst.TenantID, "x@example.com"))
	assertCode(t, err, codes.PermissionDenied)
	_, err = e.viewerInvs.Update(ctx, &iampb.UpdateInvitationRequest{Metadata: &commonpb.ResourceMetadata{Id: id}, Spec: emailOnly("x@example.com")})
	assertCode(t, err, codes.PermissionDenied)
	_, err = e.viewerInvs.Resend(ctx, &iampb.ResendInvitationRequest{Id: id})
	assertCode(t, err, codes.PermissionDenied)
	_, err = e.viewerInvs.Delete(ctx, &iampb.DeleteInvitationRequest{Id: id})
	assertCode(t, err, codes.PermissionDenied)
	_, err = e.viewerUsers.Block(ctx, &iampb.BlockTenantUserAccountRequest{Id: tuaID})
	assertCode(t, err, codes.PermissionDenied)
	_, err = e.viewerUsers.Unblock(ctx, &iampb.UnblockTenantUserAccountRequest{Id: tuaID})
	assertCode(t, err, codes.PermissionDenied)

	// Editor: manages invitations and blocks users.
	op, err := e.editorInvs.Create(ctx, invReq(e.inst.TenantID, "by-editor@example.com"))
	if err != nil {
		t.Fatalf("editor Create invitation: %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if rop, err := e.editorInvs.Resend(ctx, &iampb.ResendInvitationRequest{Id: id}); err != nil {
		t.Errorf("editor Resend: %v", err)
	} else if _, err := rop.Wait(ctx); err != nil {
		t.Error(err)
	}
	if bop, err := e.editorUsers.Block(ctx, &iampb.BlockTenantUserAccountRequest{Id: tuaID}); err != nil {
		t.Errorf("editor Block: %v", err)
	} else if _, err := bop.Wait(ctx); err != nil {
		t.Error(err)
	}
	if dop, err := e.editorInvs.Delete(ctx, &iampb.DeleteInvitationRequest{Id: id}); err != nil {
		t.Errorf("editor Delete: %v", err)
	} else if _, err := dop.Wait(ctx); err != nil {
		t.Error(err)
	}
}
