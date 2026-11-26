package dal_test

import (
	"testing"
	"time"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/iam/dal"
)

func invitation(id, tenantID, name string) *iampb.Invitation {
	return &iampb.Invitation{
		Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: tenantID, Name: name},
		Spec:     &iampb.InvitationSpec{Contact: &iampb.InvitationSpec_Email{Email: id + "@example.com"}},
		Status:   &iampb.InvitationStatus{State: iampb.InvitationStatus_PENDING},
	}
}

func TestInvitationRepo(t *testing.T) {
	t.Parallel()
	r := dal.NewInvitationRepo(store.NewMemory())
	ctx := t.Context()
	for _, inv := range []*iampb.Invitation{invitation("i1", "t1", ""), invitation("i2", "t1", ""), invitation("i3", "t2", "n"), invitation("i4", "t1", "")} {
		if err := r.Create(ctx, inv, 24*time.Hour); err != nil {
			t.Fatalf("create %s: %v", inv.GetMetadata().GetId(), err)
		}
	}
	if err := r.Create(ctx, invitation("i1", "t1", ""), time.Hour); !errs.IsKind(err, errs.KindAlreadyExists) {
		t.Fatalf("duplicate id: %v, want AlreadyExists", err)
	}
	if err := r.Create(ctx, invitation("i5", "t2", "n"), time.Hour); !errs.IsKind(err, errs.KindAlreadyExists) {
		t.Fatalf("duplicate name: %v, want AlreadyExists", err)
	}
	if _, err := r.Validity(ctx, "i5"); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("a rejected create must leave no validity record: %v", err)
	}
	if got, err := r.Validity(ctx, "i1"); err != nil || got != 24*time.Hour {
		t.Fatalf("Validity = %v, %v; want 24h", got, err)
	}

	page, err := r.List(ctx, "t1", 2, "")
	if err != nil || len(page.Items) != 2 || page.NextPageToken == "" {
		t.Fatalf("List page 1 = %d items, token %q, %v", len(page.Items), page.NextPageToken, err)
	}
	page2, err := r.List(ctx, "t1", 2, page.NextPageToken)
	if err != nil || len(page2.Items) != 1 || page2.Items[0].GetMetadata().GetId() != "i4" || page2.NextPageToken != "" {
		t.Fatalf("List page 2 = %v, %v", page2.Items, err)
	}

	inv, _ := r.Get(ctx, "i2")
	inv.Status.State = iampb.InvitationStatus_ACCEPTED
	if err := r.Update(ctx, inv); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.Get(ctx, "i2"); got.GetStatus().GetState() != iampb.InvitationStatus_ACCEPTED {
		t.Fatalf("Update did not persist: %v", got.GetStatus())
	}
	if err := r.Update(ctx, invitation("nope", "t1", "")); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("Update unknown: %v, want NotFound", err)
	}
	if got, err := r.Validity(ctx, "i2"); err != nil || got != 24*time.Hour {
		t.Fatalf("Update must keep the validity: %v, %v", got, err)
	}

	if err := r.Delete(ctx, "i1"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Get(ctx, "i1"); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("Get after delete: %v", err)
	}
	if _, err := r.Validity(ctx, "i1"); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("Validity after delete: %v", err)
	}
	if err := r.Delete(ctx, "i1"); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("second delete: %v, want NotFound", err)
	}
}

func tenantUser(id, tenantID, invitationID string) *iampb.TenantUserAccount {
	return &iampb.TenantUserAccount{
		Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: tenantID},
		Spec:     &iampb.TenantUserAccountSpec{},
		Status:   &iampb.TenantUserAccountStatus{State: iampb.TenantUserAccountStatus_ACTIVE, InvitationId: invitationID},
	}
}

func TestTenantUserAccountRepo(t *testing.T) {
	t.Parallel()
	r := dal.NewTenantUserAccountRepo(store.NewMemory())
	ctx := t.Context()
	if err := r.Ensure(ctx, tenantUser("u1", "t1", "")); err != nil {
		t.Fatal(err)
	}
	changed := tenantUser("u1", "t1", "inv")
	if err := r.Ensure(ctx, changed); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.Get(ctx, "u1"); got.GetStatus().GetInvitationId() != "" {
		t.Fatalf("Ensure replaced an existing account: %v", got.GetStatus())
	}
	for _, a := range []*iampb.TenantUserAccount{tenantUser("u2", "t1", "i2"), tenantUser("u3", "t2", "")} {
		if err := r.Create(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Create(ctx, tenantUser("u2", "t1", "")); !errs.IsKind(err, errs.KindAlreadyExists) {
		t.Fatalf("duplicate: %v", err)
	}
	page, err := r.List(ctx, "t1", 10, "")
	if err != nil || len(page.Items) != 2 {
		t.Fatalf("List t1 = %d, %v; want 2", len(page.Items), err)
	}
	a, _ := r.Get(ctx, "u2")
	a.Status.State = iampb.TenantUserAccountStatus_BLOCKED
	if err := r.Update(ctx, a); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.Get(ctx, "u2"); got.GetStatus().GetState() != iampb.TenantUserAccountStatus_BLOCKED {
		t.Fatalf("Update did not persist: %v", got.GetStatus())
	}
	if err := r.Update(ctx, tenantUser("nope", "t1", "")); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("Update unknown: %v", err)
	}

	// Attributes: empty until set, replaced on set, removed with the account.
	if got, err := r.Attributes(ctx, "u2"); err != nil || got == nil || got.Email != nil {
		t.Fatalf("Attributes before set = %v, %v; want empty", got, err)
	}
	email := "u2@example.com"
	if err := r.SetAttributes(ctx, "u2", &iampb.UserAttributes{Email: &email}); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.Attributes(ctx, "u2"); got.GetEmail() != email {
		t.Fatalf("Attributes = %v, want email %s", got, email)
	}
	if err := r.Delete(ctx, "u2"); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.Attributes(ctx, "u2"); got.Email != nil {
		t.Fatalf("attributes outlived the account: %v", got)
	}
	if err := r.Delete(ctx, "u2"); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("second delete: %v, want NotFound", err)
	}
	if err := r.Delete(ctx, "u1"); err != nil {
		t.Fatalf("delete of an account without attributes: %v", err)
	}
}
