package dal_test

import (
	"testing"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/iam/dal"
)

func fedCreds(id, parent, name, saID string) *iampb.FederatedCredentials {
	return &iampb.FederatedCredentials{
		Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: parent, Name: name},
		Spec: &iampb.FederatedCredentialsSpec{
			CredentialsProvider: &iampb.FederatedCredentialsSpec_OidcProvider{OidcProvider: &iampb.OidcCredentialsProvider{
				IssuerUrl: "https://issuer.example",
			}},
			FederatedSubjectId: "ci", SubjectId: saID,
		},
	}
}

func TestFederatedCredentialsRepo(t *testing.T) {
	t.Parallel()
	r := dal.NewFederatedCredentialsRepo(store.NewMemory())
	ctx := t.Context()
	// Unnamed credentials never collide; named ones are unique per parent.
	for _, c := range []*iampb.FederatedCredentials{
		fedCreds("f1", "project-1", "", "sa1"), fedCreds("f2", "project-1", "", "sa1"),
		fedCreds("f3", "project-1", "n", "sa2"), fedCreds("f4", "project-2", "n", "sa1"),
	} {
		if err := r.Create(ctx, c); err != nil {
			t.Fatalf("create %s: %v", c.GetMetadata().GetId(), err)
		}
	}
	if err := r.Create(ctx, fedCreds("f5", "project-1", "n", "sa2")); !errs.IsKind(err, errs.KindAlreadyExists) {
		t.Fatalf("duplicate name: %v, want AlreadyExists", err)
	}
	if c, err := r.GetByName(ctx, "project-1", "n"); err != nil || c.GetMetadata().GetId() != "f3" {
		t.Fatalf("GetByName = %v, %v", c, err)
	}
	// ListBySubject spans projects, in creation order.
	cs, err := r.ListBySubject(ctx, "sa1")
	if err != nil || len(cs) != 3 || cs[0].GetMetadata().GetId() != "f1" || cs[2].GetMetadata().GetId() != "f4" {
		t.Fatalf("ListBySubject(sa1) = %v, %v, want f1 f2 f4", cs, err)
	}
	if cs, err := r.ListBySubject(ctx, "sa-none"); err != nil || len(cs) != 0 {
		t.Fatalf("ListBySubject(sa-none) = %v, %v, want none", cs, err)
	}
	page, err := r.List(ctx, "project-1", 2, "")
	if err != nil || len(page.Items) != 2 || page.NextPageToken == "" {
		t.Fatalf("List page 1 = %v, %v", page, err)
	}
	if page, err = r.List(ctx, "project-1", 2, page.NextPageToken); err != nil || len(page.Items) != 1 || page.NextPageToken != "" {
		t.Fatalf("List page 2 = %v, %v", page, err)
	}
	upd := fedCreds("f1", "project-1", "", "sa1")
	upd.Spec.FederatedSubjectId = "changed"
	if err := r.Update(ctx, upd); err != nil {
		t.Fatal(err)
	}
	if c, _ := r.Get(ctx, "f1"); c.GetSpec().GetFederatedSubjectId() != "changed" {
		t.Fatalf("Update not stored: %v", c)
	}
	if err := r.Update(ctx, fedCreds("nope", "project-1", "", "sa1")); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("update missing: %v, want NotFound", err)
	}
	if err := r.Delete(ctx, "f1"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Get(ctx, "f1"); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("get after delete: %v, want NotFound", err)
	}
	if err := r.Delete(ctx, "f1"); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("delete twice: %v, want NotFound", err)
	}
}
