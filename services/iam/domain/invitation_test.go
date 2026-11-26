package domain_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/nebius/gosdk/proto/fieldmask/mask"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/services/iam/domain"
)

// fakeFedGetter serves federations by ID.
type fakeFedGetter map[string]*iampb.Federation

func (f fakeFedGetter) Get(_ context.Context, id string) (*iampb.Federation, error) {
	if fed, ok := f[id]; ok {
		return fed, nil
	}
	return nil, errs.NotFound(id)
}

// nidIDs generates well-formed Nebius IDs (membership validation parses them).
type nidIDs struct{ n int }

func (g *nidIDs) New(typ string) (string, error) {
	g.n++
	return fmt.Sprintf("%s-e00mock%013d", typ, g.n), nil
}

type inviteFixture struct {
	*accessFixture
	svc      *domain.InvitationService
	accounts *domain.TenantUserAccountService
	feds     fakeFedGetter
}

func newInviteFixture(t *testing.T) *inviteFixture {
	t.Helper()
	a := newAccess(t)
	feds := fakeFedGetter{
		"federation-active":   {Metadata: &commonpb.ResourceMetadata{Id: "federation-active", ParentId: tenantID}, Status: &iampb.FederationStatus{State: iampb.FederationStatus_ACTIVE}},
		"federation-inactive": {Metadata: &commonpb.ResourceMetadata{Id: "federation-inactive", ParentId: tenantID}, Status: &iampb.FederationStatus{State: iampb.FederationStatus_INACTIVE}},
		"federation-other":    {Metadata: &commonpb.ResourceMetadata{Id: "federation-other", ParentId: "tenant-other"}, Status: &iampb.FederationStatus{State: iampb.FederationStatus_ACTIVE}},
	}
	svc, err := domain.NewInvitationService(domain.InvitationDeps{
		Invitations: a.invites, Users: a.users, Attributes: a.users, Memberships: a.memberships, Tenants: fakeTenants{},
		Federations: feds, Ops: inlineOps{}, IDs: &nidIDs{}, Clock: a.clk,
	})
	if err != nil {
		t.Fatal(err)
	}
	accounts, err := domain.NewTenantUserAccountService(a.users, a.dir, inlineOps{}, a.clk)
	if err != nil {
		t.Fatal(err)
	}
	return &inviteFixture{accessFixture: a, svc: svc, accounts: accounts, feds: feds}
}

func (f *inviteFixture) create(t *testing.T, cmd domain.CreateInvitationCmd) *iampb.Invitation {
	t.Helper()
	if cmd.Metadata == nil {
		cmd.Metadata = &commonpb.ResourceMetadata{ParentId: tenantID}
	}
	if cmd.Spec == nil {
		cmd.Spec = emailSpec("ada@example.com")
	}
	op, err := f.svc.Create(t.Context(), cmd)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	inv, err := f.svc.Get(t.Context(), op.ResourceID)
	if err != nil {
		t.Fatal(err)
	}
	return inv
}

func (f *inviteFixture) state(t *testing.T, id string) iampb.InvitationStatus_State {
	t.Helper()
	inv, err := f.svc.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return inv.GetStatus().GetState()
}

func emailSpec(email string) *iampb.InvitationSpec {
	return &iampb.InvitationSpec{Contact: &iampb.InvitationSpec_Email{Email: email}}
}

func TestInvitationCreateDefaults(t *testing.T) {
	t.Parallel()
	f := newInviteFixture(t)
	inv := f.create(t, domain.CreateInvitationCmd{
		Metadata: &commonpb.ResourceMetadata{ParentId: tenantID, Name: "ada", Labels: map[string]string{"a": "b"}},
		Spec:     &iampb.InvitationSpec{Description: "hi", Contact: &iampb.InvitationSpec_Email{Email: "ada@example.com"}},
	})
	st := inv.GetStatus()
	if st.GetState() != iampb.InvitationStatus_PENDING || !st.GetExpiresAt().AsTime().Equal(f.clk.Now().Add(72*time.Hour)) {
		t.Fatalf("status = %v, want PENDING expiring in 72h", st)
	}
	md := inv.GetMetadata()
	if md.GetName() != "ada" || md.GetLabels()["a"] != "b" || md.GetParentId() != tenantID || inv.GetSpec().GetDescription() != "hi" {
		t.Fatalf("metadata/spec = %v / %v", md, inv.GetSpec())
	}
	tua, err := f.users.Get(t.Context(), st.GetTenantUserAccountId())
	if err != nil {
		t.Fatalf("tenant user account of the invitation: %v", err)
	}
	ts := tua.GetStatus()
	if tua.GetMetadata().GetParentId() != tenantID || ts.GetInvitationId() != md.GetId() ||
		ts.GetState() != iampb.TenantUserAccountStatus_ACTIVE || ts.GetUserAccountState() != iampb.UserAccountStatus_STATE_UNSPECIFIED {
		t.Fatalf("tenant user account = %v", tua)
	}
}

func TestInvitationCreateOptions(t *testing.T) {
	t.Parallel()
	f := newInviteFixture(t)
	noSend := f.create(t, domain.CreateInvitationCmd{NoSend: true})
	if noSend.GetStatus().GetState() != iampb.InvitationStatus_CREATED {
		t.Fatalf("no_send state = %v, want CREATED", noSend.GetStatus().GetState())
	}
	short := f.create(t, domain.CreateInvitationCmd{ExpiresIn: durationpb.New(time.Hour)})
	if !short.GetStatus().GetExpiresAt().AsTime().Equal(f.clk.Now().Add(time.Hour)) {
		t.Fatalf("expires_at = %v, want now+1h", short.GetStatus().GetExpiresAt().AsTime())
	}
}

func TestInvitationCreateValidation(t *testing.T) {
	t.Parallel()
	f := newInviteFixture(t)
	for name, tt := range map[string]struct {
		cmd   domain.CreateInvitationCmd
		want  errs.Kind
		field string
	}{
		"no email":      {domain.CreateInvitationCmd{Metadata: &commonpb.ResourceMetadata{ParentId: tenantID}, Spec: &iampb.InvitationSpec{}}, errs.KindBadRequest, "spec.email"},
		"display name":  {domain.CreateInvitationCmd{Metadata: &commonpb.ResourceMetadata{ParentId: tenantID}, Spec: emailSpec("Ada <ada@example.com>")}, errs.KindBadRequest, "spec.email"},
		"not an email":  {domain.CreateInvitationCmd{Metadata: &commonpb.ResourceMetadata{ParentId: tenantID}, Spec: emailSpec("ada")}, errs.KindBadRequest, "spec.email"},
		"zero expiry":   {domain.CreateInvitationCmd{Metadata: &commonpb.ResourceMetadata{ParentId: tenantID}, Spec: emailSpec("a@b.io"), ExpiresIn: durationpb.New(0)}, errs.KindBadRequest, "expires_in"},
		"negative":      {domain.CreateInvitationCmd{Metadata: &commonpb.ResourceMetadata{ParentId: tenantID}, Spec: emailSpec("a@b.io"), ExpiresIn: durationpb.New(-time.Hour)}, errs.KindBadRequest, "expires_in"},
		"no parent":     {domain.CreateInvitationCmd{Metadata: &commonpb.ResourceMetadata{}, Spec: emailSpec("a@b.io")}, errs.KindBadRequest, "metadata.parent_id"},
		"id set":        {domain.CreateInvitationCmd{Metadata: &commonpb.ResourceMetadata{ParentId: tenantID, Id: "x"}, Spec: emailSpec("a@b.io")}, errs.KindBadRequest, "metadata.id"},
		"unknown":       {domain.CreateInvitationCmd{Metadata: &commonpb.ResourceMetadata{ParentId: "tenant-nope"}, Spec: emailSpec("a@b.io")}, errs.KindNotFound, ""},
		"project as pa": {domain.CreateInvitationCmd{Metadata: &commonpb.ResourceMetadata{ParentId: "project-e00x"}, Spec: emailSpec("a@b.io")}, errs.KindNotFound, ""},
	} {
		_, err := f.svc.Create(t.Context(), tt.cmd)
		if got := errs.KindOf(err); got != tt.want {
			t.Errorf("%s: kind %v, want %v (%v)", name, got, tt.want, err)
			continue
		}
		if tt.field != "" && !hasViolation(err, tt.field) {
			t.Errorf("%s: error %v does not name %s", name, err, tt.field)
		}
	}
	if len(f.invites.items) != 0 || len(f.users.items) != 2 { // only the two seeded users
		t.Fatalf("rejected creates left %d invitations and %d accounts", len(f.invites.items), len(f.users.items))
	}
}

func TestInvitationExpiresOnRead(t *testing.T) {
	t.Parallel()
	f := newInviteFixture(t)
	sent := f.create(t, domain.CreateInvitationCmd{})
	unsent := f.create(t, domain.CreateInvitationCmd{NoSend: true, Spec: emailSpec("grace@example.com")})
	f.clk.Advance(72*time.Hour - time.Second)
	if got := f.state(t, sent.GetMetadata().GetId()); got != iampb.InvitationStatus_PENDING {
		t.Fatalf("state just before expiry = %v", got)
	}
	f.clk.Advance(time.Second)
	for _, id := range []string{sent.GetMetadata().GetId(), unsent.GetMetadata().GetId()} {
		if got := f.state(t, id); got != iampb.InvitationStatus_EXPIRED {
			t.Fatalf("%s state at expiry = %v, want EXPIRED", id, got)
		}
	}
	page, err := f.svc.List(t.Context(), tenantID, 0, "", "")
	if err != nil || len(page.Items) != 2 || page.Items[0].GetStatus().GetState() != iampb.InvitationStatus_EXPIRED {
		t.Fatalf("List = %v, %v; want 2 expired items", page.Items, err)
	}
	if _, err := f.svc.List(t.Context(), tenantID, 0, "", `x = "y"`); !hasViolation(err, "filter") {
		t.Fatalf("List filter error = %v, want a violation on filter", err)
	}
}

func TestInvitationResend(t *testing.T) {
	t.Parallel()
	f := newInviteFixture(t)
	inv := f.create(t, domain.CreateInvitationCmd{ExpiresIn: durationpb.New(2 * time.Hour), NoSend: true})
	id := inv.GetMetadata().GetId()
	resend := func() error {
		_, err := f.svc.Resend(t.Context(), id, nil)
		return err
	}
	if err := resend(); err != nil || f.state(t, id) != iampb.InvitationStatus_PENDING {
		t.Fatalf("resend of CREATED: %v, state %v", err, f.state(t, id))
	}
	f.clk.Advance(5 * time.Hour)
	if f.state(t, id) != iampb.InvitationStatus_EXPIRED {
		t.Fatalf("state after 5h = %v, want EXPIRED", f.state(t, id))
	}
	if err := resend(); err != nil {
		t.Fatalf("resend of EXPIRED: %v", err)
	}
	got, _ := f.svc.Get(t.Context(), id)
	if got.GetStatus().GetState() != iampb.InvitationStatus_PENDING || !got.GetStatus().GetExpiresAt().AsTime().Equal(f.clk.Now().Add(2*time.Hour)) {
		t.Fatalf("status after resend = %v, want PENDING expiring in the original 2h", got.GetStatus())
	}
	if got.GetMetadata().GetResourceVersion() <= inv.GetMetadata().GetResourceVersion() {
		t.Fatalf("resource_version did not advance: %d -> %d", inv.GetMetadata().GetResourceVersion(), got.GetMetadata().GetResourceVersion())
	}
	if _, err := f.svc.Accept(t.Context(), id, ""); err != nil {
		t.Fatal(err)
	}
	if err := resend(); !errs.IsKind(err, errs.KindBadState) {
		t.Fatalf("resend of ACCEPTED: %v, want BadState", err)
	}
	if _, err := f.svc.Resend(t.Context(), "invitation-nope", nil); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("resend of unknown: %v, want NotFound", err)
	}
}

func TestInvitationUpdate(t *testing.T) {
	t.Parallel()
	f := newInviteFixture(t)
	inv := f.create(t, domain.CreateInvitationCmd{Spec: &iampb.InvitationSpec{Description: "d", Contact: &iampb.InvitationSpec_Email{Email: "ada@example.com"}}})
	id, tuaID := inv.GetMetadata().GetId(), inv.GetStatus().GetTenantUserAccountId()
	update := func(spec *iampb.InvitationSpec, reset string) error {
		cmd := domain.UpdateInvitationCmd{Metadata: &commonpb.ResourceMetadata{Id: id}, Spec: spec}
		if reset != "" {
			cmd.ResetMask = mask.ParseMust(reset)
		}
		_, err := f.svc.Update(t.Context(), cmd)
		return err
	}

	if err := update(&iampb.InvitationSpec{Description: "new", Contact: &iampb.InvitationSpec_Email{Email: "grace@example.com"}}, ""); err != nil {
		t.Fatal(err)
	}
	got, _ := f.svc.Get(t.Context(), id)
	if got.GetSpec().GetDescription() != "new" || got.GetSpec().GetEmail() != "grace@example.com" {
		t.Fatalf("spec = %v", got.GetSpec())
	}
	if a, _ := f.users.Attributes(t.Context(), tuaID); a.GetEmail() != "grace@example.com" {
		t.Fatalf("attributes email = %q, want the new email", a.GetEmail())
	}
	// Unset description without a reset mask keeps it; with the mask it clears.
	if err := update(&iampb.InvitationSpec{}, ""); err != nil {
		t.Fatal(err)
	}
	if got, _ = f.svc.Get(t.Context(), id); got.GetSpec().GetDescription() != "new" || got.GetSpec().GetEmail() != "grace@example.com" {
		t.Fatalf("unmasked empty update changed spec: %v", got.GetSpec())
	}
	if err := update(&iampb.InvitationSpec{}, "spec.description"); err != nil {
		t.Fatal(err)
	}
	if got, _ = f.svc.Get(t.Context(), id); got.GetSpec().GetDescription() != "" || got.GetSpec().GetEmail() != "grace@example.com" {
		t.Fatalf("masked update spec = %v, want description cleared and email kept", got.GetSpec())
	}
	// Clearing or garbling the email is rejected.
	for _, tt := range []struct {
		spec  *iampb.InvitationSpec
		reset string
	}{{&iampb.InvitationSpec{}, "spec.email"}, {emailSpec("nope"), ""}} {
		if err := update(tt.spec, tt.reset); !hasViolation(err, "spec.email") {
			t.Errorf("update %v reset %q: %v, want a violation on spec.email", tt.spec, tt.reset, err)
		}
	}
	// A stale resource_version conflicts.
	_, err := f.svc.Update(t.Context(), domain.UpdateInvitationCmd{Metadata: &commonpb.ResourceMetadata{Id: id, ResourceVersion: 99}, Spec: emailSpec("x@y.io")})
	if !errs.IsKind(err, errs.KindConflict) {
		t.Fatalf("stale version: %v, want Conflict", err)
	}
	if _, err := f.svc.Update(t.Context(), domain.UpdateInvitationCmd{Metadata: &commonpb.ResourceMetadata{Id: "invitation-nope"}}); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("unknown: %v, want NotFound", err)
	}

	// After acceptance the email is fixed, the description is not.
	if _, err := f.svc.Accept(t.Context(), id, ""); err != nil {
		t.Fatal(err)
	}
	if err := update(emailSpec("late@example.com"), ""); !errs.IsKind(err, errs.KindBadState) {
		t.Fatalf("email change after accept: %v, want BadState", err)
	}
	if err := update(&iampb.InvitationSpec{Description: "after", Contact: &iampb.InvitationSpec_Email{Email: "grace@example.com"}}, ""); err != nil {
		t.Fatalf("description change after accept: %v", err)
	}
}

func TestInvitationDeleteCascade(t *testing.T) {
	t.Parallel()
	f := newInviteFixture(t)
	pending := f.create(t, domain.CreateInvitationCmd{})
	accepted := f.create(t, domain.CreateInvitationCmd{Spec: emailSpec("grace@example.com")})
	if _, err := f.svc.Accept(t.Context(), accepted.GetMetadata().GetId(), ""); err != nil {
		t.Fatal(err)
	}
	pendingTUA, acceptedTUA := pending.GetStatus().GetTenantUserAccountId(), accepted.GetStatus().GetTenantUserAccountId()
	for _, id := range []string{pendingTUA, acceptedTUA} {
		if err := f.addMember(t, id, 0); err != nil {
			t.Fatalf("add member %s: %v", id, err)
		}
	}

	if _, err := f.svc.Delete(t.Context(), pending.GetMetadata().GetId(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Get(t.Context(), pending.GetMetadata().GetId()); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("deleted invitation Get: %v", err)
	}
	if _, err := f.users.Get(t.Context(), pendingTUA); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("pending invitation's account survived: %v", err)
	}
	if ms, _ := f.memberships.ListByMember(t.Context(), pendingTUA); len(ms) != 0 {
		t.Fatalf("pending invitation's memberships survived: %v", ms)
	}
	if _, ok := f.invites.validity[pending.GetMetadata().GetId()]; ok {
		t.Fatal("validity record survived")
	}

	if _, err := f.svc.Delete(t.Context(), accepted.GetMetadata().GetId(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := f.users.Get(t.Context(), acceptedTUA); err != nil {
		t.Fatalf("accepted invitation's account must outlive the invitation: %v", err)
	}
	if ms, _ := f.memberships.ListByMember(t.Context(), acceptedTUA); len(ms) != 1 {
		t.Fatalf("accepted user's memberships = %v, want 1", ms)
	}
	if _, err := f.svc.Delete(t.Context(), "invitation-nope", nil); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("delete unknown: %v, want NotFound", err)
	}
}

func TestInvitationAccept(t *testing.T) {
	t.Parallel()
	f := newInviteFixture(t)
	inv := f.create(t, domain.CreateInvitationCmd{})
	id, tuaID := inv.GetMetadata().GetId(), inv.GetStatus().GetTenantUserAccountId()

	for name, fed := range map[string]string{"unknown": "federation-nope", "other tenant": "federation-other"} {
		if _, err := f.svc.Accept(t.Context(), id, fed); !errs.IsKind(err, errs.KindNotFound) {
			t.Errorf("federation %s: %v, want NotFound", name, err)
		}
	}
	if _, err := f.svc.Accept(t.Context(), id, "federation-inactive"); !errs.IsKind(err, errs.KindBadState) {
		t.Errorf("inactive federation: %v, want BadState", err)
	}
	if got := f.state(t, id); got != iampb.InvitationStatus_PENDING {
		t.Fatalf("failed accepts changed the state to %v", got)
	}

	res, err := f.svc.Accept(t.Context(), id, "federation-active")
	if err != nil || res.InvitationID != id || res.TenantUserAccountID != tuaID || res.State != iampb.InvitationStatus_ACCEPTED {
		t.Fatalf("Accept = %+v, %v", res, err)
	}
	tua, _ := f.users.Get(t.Context(), tuaID)
	if ts := tua.GetStatus(); ts.GetInvitationId() != "" || ts.GetFederationId() != "federation-active" ||
		ts.GetUserAccountState() != iampb.UserAccountStatus_ACTIVE || ts.GetState() != iampb.TenantUserAccountStatus_ACTIVE {
		t.Fatalf("account after accept = %v", ts)
	}
	if got := f.state(t, id); got != iampb.InvitationStatus_ACCEPTED {
		t.Fatalf("state = %v, want ACCEPTED", got)
	}
	if _, err := f.svc.Accept(t.Context(), id, ""); !errs.IsKind(err, errs.KindBadState) {
		t.Fatalf("second accept: %v, want BadState", err)
	}
	// Accepted invitations never expire.
	f.clk.Advance(30 * 24 * time.Hour)
	if got := f.state(t, id); got != iampb.InvitationStatus_ACCEPTED {
		t.Fatalf("accepted invitation state after a month = %v", got)
	}

	unsent := f.create(t, domain.CreateInvitationCmd{NoSend: true, Spec: emailSpec("g@example.com")})
	if _, err := f.svc.Accept(t.Context(), unsent.GetMetadata().GetId(), ""); !errs.IsKind(err, errs.KindBadState) {
		t.Fatalf("accept of CREATED: %v, want BadState", err)
	}
	expiring := f.create(t, domain.CreateInvitationCmd{ExpiresIn: durationpb.New(time.Minute), Spec: emailSpec("h@example.com")})
	f.clk.Advance(time.Minute)
	if _, err := f.svc.Accept(t.Context(), expiring.GetMetadata().GetId(), ""); !errs.IsKind(err, errs.KindBadState) {
		t.Fatalf("accept of EXPIRED: %v, want BadState", err)
	}
	if _, err := f.svc.Accept(t.Context(), "invitation-nope", ""); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("accept of unknown: %v, want NotFound", err)
	}
}

func TestMembershipOfInvitedUsers(t *testing.T) {
	t.Parallel()
	f := newInviteFixture(t)
	if err := f.addMember(t, "tenantuseraccount-e00nobody", 0); !errs.IsKind(err, errs.KindNotFound) || !hasMessage(err, "tenantuseraccount-e00nobody") {
		t.Fatalf("unknown user: %v, want NotFound naming it", err)
	}
	foreign := &iampb.TenantUserAccount{Metadata: &commonpb.ResourceMetadata{Id: "tenantuseraccount-e00foreign", ParentId: "tenant-other"}}
	if err := f.users.Create(t.Context(), foreign); err != nil {
		t.Fatal(err)
	}
	if err := f.addMember(t, "tenantuseraccount-e00foreign", 0); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("user of another tenant: %v, want NotFound", err)
	}

	inv := f.create(t, domain.CreateInvitationCmd{})
	tuaID := inv.GetStatus().GetTenantUserAccountId()
	if err := f.addMember(t, tuaID, 0); err != nil {
		t.Fatalf("invited user (pending) must be addable: %v", err)
	}
	f.clk.Advance(73 * time.Hour)
	expired := f.create(t, domain.CreateInvitationCmd{Spec: emailSpec("late@example.com"), ExpiresIn: durationpb.New(time.Hour)})
	f.clk.Advance(2 * time.Hour)
	expiredTUA := expired.GetStatus().GetTenantUserAccountId()
	if err := f.addMember(t, expiredTUA, 0); !errs.IsKind(err, errs.KindBadState) {
		t.Fatalf("user with an expired invitation: %v, want BadState", err)
	}
	if _, err := f.svc.Resend(t.Context(), expired.GetMetadata().GetId(), nil); err != nil {
		t.Fatal(err)
	}
	if err := f.addMember(t, expiredTUA, 0); err != nil {
		t.Fatalf("after Resend: %v", err)
	}
	// Service accounts are not checked.
	if err := f.addMember(t, saID, 0); err != nil {
		t.Fatalf("service account member: %v", err)
	}
}

func hasMessage(err error, sub string) bool {
	return err != nil && strings.Contains(err.Error(), sub)
}

func TestTenantUserAccountBlockUnblock(t *testing.T) {
	t.Parallel()
	f := newInviteFixture(t)
	ctx := t.Context()
	if _, err := f.accounts.Block(ctx, userID, nil); err != nil {
		t.Fatal(err)
	}
	a, err := f.accounts.Get(ctx, userID)
	if err != nil || a.GetStatus().GetState() != iampb.TenantUserAccountStatus_BLOCKED {
		t.Fatalf("after Block: %v, %v", a.GetStatus(), err)
	}
	v1 := a.GetMetadata().GetResourceVersion()
	if _, err := f.accounts.Block(ctx, userID, nil); err != nil {
		t.Fatalf("Block is idempotent: %v", err)
	}
	if a, _ = f.accounts.Get(ctx, userID); a.GetMetadata().GetResourceVersion() != v1 {
		t.Fatalf("idempotent Block bumped the version")
	}
	if _, err := f.accounts.Unblock(ctx, userID, nil); err != nil {
		t.Fatal(err)
	}
	if a, _ = f.accounts.Get(ctx, userID); a.GetStatus().GetState() != iampb.TenantUserAccountStatus_ACTIVE {
		t.Fatalf("after Unblock: %v", a.GetStatus())
	}
	if _, err := f.accounts.Block(ctx, "tenantuseraccount-nope", nil); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("Block unknown: %v, want NotFound", err)
	}
	if _, err := f.accounts.Unblock(ctx, "tenantuseraccount-nope", nil); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("Unblock unknown: %v, want NotFound", err)
	}
}

func TestTenantUserAccountListAndAttributes(t *testing.T) {
	t.Parallel()
	f := newInviteFixture(t)
	ctx := t.Context()
	inv := f.create(t, domain.CreateInvitationCmd{})
	if _, err := f.accounts.List(ctx, tenantID, 0, "", `a = "b"`); !hasViolation(err, "filter") {
		t.Fatalf("List filter: %v, want a violation on filter", err)
	}
	if _, err := f.accounts.List(ctx, tenantID, -1, "", ""); !hasViolation(err, "page_size") {
		t.Fatalf("List page size: %v", err)
	}
	page, err := f.accounts.List(ctx, tenantID, 0, "", "")
	if err != nil || len(page.Items) != 3 {
		t.Fatalf("List = %d items, %v; want 2 seeded + 1 invited", len(page.Items), err)
	}

	seeded, err := f.accounts.GetWithAttributes(ctx, userID)
	if err != nil || seeded.GetAttributes().GetSub() != userID || seeded.GetAttributes().Email != nil {
		t.Fatalf("seeded attributes = %v, %v; want sub = the account ID", seeded.GetAttributes(), err)
	}
	invited, err := f.accounts.GetWithAttributes(ctx, inv.GetStatus().GetTenantUserAccountId())
	if err != nil || invited.GetAttributes().GetEmail() != "ada@example.com" || invited.GetAttributes().Sub != nil {
		t.Fatalf("invited attributes = %v, %v; want email only", invited.GetAttributes(), err)
	}
	if _, err := f.svc.Accept(ctx, inv.GetMetadata().GetId(), ""); err != nil {
		t.Fatal(err)
	}
	accepted, _ := f.accounts.GetWithAttributes(ctx, inv.GetStatus().GetTenantUserAccountId())
	if accepted.GetAttributes().GetEmail() != "ada@example.com" || accepted.GetAttributes().GetSub() != inv.GetStatus().GetTenantUserAccountId() {
		t.Fatalf("accepted attributes = %v", accepted.GetAttributes())
	}
	all, err := f.accounts.ListWithAttributes(ctx, tenantID, 0, "", "")
	if err != nil || len(all.Items) != 3 {
		t.Fatalf("ListWithAttributes = %d, %v", len(all.Items), err)
	}
	if _, err := f.accounts.GetWithAttributes(ctx, "tenantuseraccount-nope"); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("unknown: %v", err)
	}
}

func TestMembershipWithAttributes(t *testing.T) {
	t.Parallel()
	f := newInviteFixture(t)
	ctx := t.Context()
	inv := f.create(t, domain.CreateInvitationCmd{})
	invitedID := inv.GetStatus().GetTenantUserAccountId()
	f.sas.items = append(f.sas.items, &iampb.ServiceAccount{
		Metadata: &commonpb.ResourceMetadata{Id: saID, ParentId: "project-e00x", Name: "robot"},
		Spec:     &iampb.ServiceAccountSpec{Description: "does chores"},
	})
	const goneSA = "serviceaccount-e00gone"
	for _, m := range []string{userID, invitedID, saID, goneSA} {
		if err := f.addMember(t, m, 0); err != nil {
			t.Fatal(err)
		}
	}
	page, err := f.members.ListMembersWithAttributes(ctx, f.groupID, 0, "")
	if err != nil || len(page.Items) != 4 {
		t.Fatalf("ListMembersWithAttributes = %d items, %v", len(page.Items), err)
	}
	byMember := map[string]*iampb.GroupMembershipWithAttributes{}
	for _, it := range page.Items {
		byMember[it.GetGroupMembership().GetSpec().GetMemberId()] = it
	}
	if it := byMember[userID]; it.GetGroupMemberKind().GetKind() != iampb.GroupMemberKind_ORDINARY_TENANT_USER_ACCOUNT || it.GetUserAttributes().GetSub() != userID {
		t.Errorf("ordinary member = %v", it)
	}
	if it := byMember[invitedID]; it.GetGroupMemberKind().GetKind() != iampb.GroupMemberKind_INVITED_TENANT_USER_ACCOUNT || it.GetUserAttributes().GetEmail() != "ada@example.com" {
		t.Errorf("invited member = %v", it)
	}
	if it := byMember[saID]; it.GetGroupMemberKind().GetKind() != iampb.GroupMemberKind_SERVICE_ACCOUNT ||
		it.GetServiceAccountAttributes().GetName() != "robot" || it.GetServiceAccountAttributes().GetDescription() != "does chores" {
		t.Errorf("service account member = %v, want its name and description", it)
	}
	// A member that no longer exists is reported per item, not as a failure.
	if it := byMember[goneSA]; it.GetGroupMemberKind().GetKind() != iampb.GroupMemberKind_SERVICE_ACCOUNT || it.GetError().GetMessage() == "" {
		t.Errorf("deleted service account member = %v, want kind SERVICE_ACCOUNT with an error item", it)
	}
	one, err := f.members.GetWithAttributes(ctx, byMember[userID].GetGroupMembership().GetMetadata().GetId())
	if err != nil || one.GetUserAttributes().GetSub() != userID {
		t.Fatalf("GetWithAttributes = %v, %v", one, err)
	}
	if _, err := f.members.GetWithAttributes(ctx, "groupmembership-nope"); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("unknown membership: %v", err)
	}
}

func TestBindingsBlockedUser(t *testing.T) {
	t.Parallel()
	f := newInviteFixture(t)
	ctx := t.Context()
	if err := f.addMember(t, userID, 0); err != nil {
		t.Fatal(err)
	}
	if err := f.grant(t, tenantID, authz.RoleEditor); err != nil {
		t.Fatal(err)
	}
	b := domain.NewBindings(f.memberships, f.permits, f.users, f.clk)
	if roles, err := b.RolesOn(ctx, userID, tenantID); err != nil || len(roles) != 1 {
		t.Fatalf("roles before block = %v, %v", roles, err)
	}
	if _, err := f.accounts.Block(ctx, userID, nil); err != nil {
		t.Fatal(err)
	}
	if roles, err := b.RolesOn(ctx, userID, tenantID); err != nil || len(roles) != 0 {
		t.Fatalf("roles of a blocked user = %v, %v; want none", roles, err)
	}
	if _, err := f.accounts.Unblock(ctx, userID, nil); err != nil {
		t.Fatal(err)
	}
	if roles, err := b.RolesOn(ctx, userID, tenantID); err != nil || len(roles) != 1 {
		t.Fatalf("roles after unblock = %v, %v", roles, err)
	}
}

func TestMembershipStatusOnRead(t *testing.T) {
	t.Parallel()
	f := newInviteFixture(t)
	ctx := t.Context()
	inv := f.create(t, domain.CreateInvitationCmd{})
	invitedID := inv.GetStatus().GetTenantUserAccountId()
	f.sas.items = append(f.sas.items, &iampb.ServiceAccount{Metadata: &commonpb.ResourceMetadata{Id: saID, Name: "robot"}})
	const goneSA = "serviceaccount-e00gone"
	for _, m := range []string{userID, invitedID, saID, goneSA} {
		if err := f.addMember(t, m, 0); err != nil {
			t.Fatal(err)
		}
	}
	page, err := f.members.ListMembers(ctx, f.groupID, 0, "")
	if err != nil || len(page.Items) != 4 {
		t.Fatalf("ListMembers = %d, %v", len(page.Items), err)
	}
	byMember := map[string]*iampb.GroupMembership{}
	for _, m := range page.Items {
		byMember[m.GetSpec().GetMemberId()] = m
	}
	if st := byMember[userID].GetStatus().GetTenantUserAccountStatus(); st.GetState() != iampb.TenantUserAccountStatus_ACTIVE ||
		st.GetUserAccountState() != iampb.UserAccountStatus_ACTIVE {
		t.Errorf("seeded member status = %v", st)
	}
	if st := byMember[invitedID].GetStatus().GetTenantUserAccountStatus(); st.GetInvitationId() != inv.GetMetadata().GetId() {
		t.Errorf("invited member status = %v, want invitation_id %s", st, inv.GetMetadata().GetId())
	}
	if byMember[saID].GetStatus().GetServiceAccountStatus() == nil {
		t.Errorf("service account member has no service_account_status")
	}
	if st := byMember[goneSA].GetStatus(); st.GetGroupMemberStatus() != nil || st.GetGroupMemberMetadata().GetId() != goneSA {
		t.Errorf("unknown service account member status = %v, want only group_member_metadata", st)
	}

	if _, err := f.accounts.Block(ctx, userID, nil); err != nil {
		t.Fatal(err)
	}
	one, err := f.members.Get(ctx, byMember[userID].GetMetadata().GetId())
	if err != nil || one.GetStatus().GetTenantUserAccountStatus().GetState() != iampb.TenantUserAccountStatus_BLOCKED {
		t.Fatalf("Get after Block = %v, %v; want BLOCKED", one.GetStatus(), err)
	}
	withAttrs, err := f.members.GetWithAttributes(ctx, byMember[userID].GetMetadata().GetId())
	if err != nil || withAttrs.GetGroupMembership().GetStatus().GetTenantUserAccountStatus().GetState() != iampb.TenantUserAccountStatus_BLOCKED {
		t.Fatalf("GetWithAttributes after Block = %v, %v; want BLOCKED", withAttrs.GetGroupMembership().GetStatus(), err)
	}
	if stored, _ := f.memberships.Get(ctx, one.GetMetadata().GetId()); stored.GetStatus().GetTenantUserAccountStatus() != nil {
		t.Errorf("status was stored; it must be computed on read")
	}
}
