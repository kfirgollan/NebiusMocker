package dal_test

import (
	"testing"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/iam/dal"
)

func authKey(id, name, saID string) *iampb.AuthPublicKey {
	return &iampb.AuthPublicKey{
		Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: "project-1", Name: name},
		Spec: &iampb.AuthPublicKeySpec{Account: &iampb.Account{
			Type: &iampb.Account_ServiceAccount_{ServiceAccount: &iampb.Account_ServiceAccount{Id: saID}},
		}},
	}
}

func TestAuthPublicKeyRepo(t *testing.T) {
	t.Parallel()
	r := dal.NewAuthPublicKeyRepo(store.NewMemory())
	ctx := t.Context()
	// Unnamed keys never collide; named keys are unique per parent.
	for _, k := range []*iampb.AuthPublicKey{authKey("k1", "", "sa1"), authKey("k2", "", "sa1"), authKey("k3", "n", "sa2")} {
		if err := r.Create(ctx, k); err != nil {
			t.Fatalf("create %s: %v", k.GetMetadata().GetId(), err)
		}
	}
	if err := r.Create(ctx, authKey("k4", "n", "sa2")); !errs.IsKind(err, errs.KindAlreadyExists) {
		t.Fatalf("duplicate name: %v, want AlreadyExists", err)
	}
	if k, err := r.GetByName(ctx, "project-1", "n"); err != nil || k.GetMetadata().GetId() != "k3" {
		t.Fatalf("GetByName = %v, %v", k, err)
	}
	if ks, err := r.ListByAccount(ctx, "sa1"); err != nil || len(ks) != 2 {
		t.Fatalf("ListByAccount = %v, %v", ks, err)
	}
	page, err := r.List(ctx, "project-1", 2, "")
	if err != nil || len(page.Items) != 2 || page.NextPageToken == "" {
		t.Fatalf("List page 1 = %v, %v", page, err)
	}
	if page, err = r.List(ctx, "project-1", 2, page.NextPageToken); err != nil || len(page.Items) != 1 || page.NextPageToken != "" {
		t.Fatalf("List page 2 = %v, %v", page, err)
	}
	upd := authKey("k1", "", "sa1")
	upd.Spec.Description = "changed"
	if err := r.Update(ctx, upd); err != nil {
		t.Fatal(err)
	}
	if k, _ := r.Get(ctx, "k1"); k.GetSpec().GetDescription() != "changed" {
		t.Fatalf("Update not stored: %v", k)
	}
	if err := r.Update(ctx, authKey("nope", "", "sa1")); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("update missing: %v, want NotFound", err)
	}
	if err := r.Delete(ctx, "k1"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Get(ctx, "k1"); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("get after delete: %v, want NotFound", err)
	}
	if err := r.Delete(ctx, "k1"); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("delete twice: %v, want NotFound", err)
	}
}
