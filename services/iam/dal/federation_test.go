package dal_test

import (
	"testing"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/iam/dal"
)

func federation(id, name string) *iampb.Federation {
	return &iampb.Federation{Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: "tenant-1", Name: name}}
}

func fedCert(id, parent, name string) *iampb.FederationCertificate {
	return &iampb.FederationCertificate{
		Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: parent, Name: name},
		Spec:     &iampb.FederationCertificateSpec{Data: id},
	}
}

func TestFederationRepo(t *testing.T) {
	t.Parallel()
	r := dal.NewFederationRepo(store.NewMemory())
	ctx := t.Context()
	for _, n := range []string{"a", "b", "c"} {
		if err := r.Create(ctx, federation("fed-"+n, n)); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Create(ctx, federation("fed-x", "a")); !errs.IsKind(err, errs.KindAlreadyExists) {
		t.Fatalf("duplicate name = %v", err)
	}
	if err := r.Update(ctx, federation("fed-none", "z")); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("update missing = %v", err)
	}
	if f, err := r.GetByName(ctx, "tenant-1", "b"); err != nil || f.GetMetadata().GetId() != "fed-b" {
		t.Fatalf("GetByName = %v, %v", f, err)
	}
	page, err := r.List(ctx, "tenant-1", 2, "")
	if err != nil || len(page.Items) != 2 || page.NextPageToken == "" {
		t.Fatalf("page 1 = %v, %v", page, err)
	}
	page, err = r.List(ctx, "tenant-1", 2, page.NextPageToken)
	if err != nil || len(page.Items) != 1 || page.NextPageToken != "" {
		t.Fatalf("page 2 = %v, %v", page, err)
	}
	if err := r.Delete(ctx, "fed-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Get(ctx, "fed-a"); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("get deleted = %v", err)
	}
}

func TestFederationCertificateRepo(t *testing.T) {
	t.Parallel()
	r := dal.NewFederationCertificateRepo(store.NewMemory())
	ctx := t.Context()
	for _, c := range []*iampb.FederationCertificate{fedCert("c1", "f1", "one"), fedCert("c2", "f1", ""), fedCert("c3", "f1", ""), fedCert("c4", "f2", "one")} {
		if err := r.Create(ctx, c); err != nil {
			t.Fatalf("create %s: %v", c.GetMetadata().GetId(), err)
		}
	}
	if err := r.Create(ctx, fedCert("c5", "f1", "one")); !errs.IsKind(err, errs.KindAlreadyExists) {
		t.Fatalf("duplicate name = %v", err)
	}
	all, err := r.ListAll(ctx, "f1")
	if err != nil || len(all) != 3 {
		t.Fatalf("ListAll = %d, %v", len(all), err)
	}
	page, err := r.List(ctx, "f1", 2, "")
	if err != nil || len(page.Items) != 2 || page.NextPageToken == "" {
		t.Fatalf("page = %v, %v", page, err)
	}
}

func TestFederationCertificateReplaceAllIsAtomic(t *testing.T) {
	t.Parallel()
	r := dal.NewFederationCertificateRepo(store.NewMemory())
	ctx := t.Context()
	for _, c := range []*iampb.FederationCertificate{fedCert("c1", "f1", "one"), fedCert("c2", "f1", "two")} {
		if err := r.Create(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	// The create collides with the kept "one": the removal of c2 must roll back.
	err := r.ReplaceAll(ctx, "f1", []string{"c2"}, []*iampb.FederationCertificate{fedCert("c1", "f1", "one")},
		[]*iampb.FederationCertificate{fedCert("c9", "f1", "one")})
	if !errs.IsKind(err, errs.KindAlreadyExists) {
		t.Fatalf("ReplaceAll = %v, want AlreadyExists", err)
	}
	if all, _ := r.ListAll(ctx, "f1"); len(all) != 2 {
		t.Fatalf("failed ReplaceAll left %d certificates, want 2", len(all))
	}
	// Updating a missing certificate fails as a whole too.
	if err := r.ReplaceAll(ctx, "f1", []string{"c2"}, []*iampb.FederationCertificate{fedCert("c7", "f1", "")}, nil); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("update missing = %v", err)
	}
	if all, _ := r.ListAll(ctx, "f1"); len(all) != 2 {
		t.Fatalf("failed ReplaceAll left %d certificates, want 2", len(all))
	}

	// Success: remove c2, rename c1, add c9 under c2's old name.
	err = r.ReplaceAll(ctx, "f1", []string{"c2"}, []*iampb.FederationCertificate{fedCert("c1", "f1", "renamed")},
		[]*iampb.FederationCertificate{fedCert("c9", "f1", "two")})
	if err != nil {
		t.Fatal(err)
	}
	all, _ := r.ListAll(ctx, "f1")
	if len(all) != 2 || all[0].GetMetadata().GetName() != "renamed" || all[1].GetMetadata().GetId() != "c9" {
		t.Fatalf("after ReplaceAll = %v", all)
	}
	if err := r.DeleteByFederation(ctx, "f1"); err != nil {
		t.Fatal(err)
	}
	if all, _ := r.ListAll(ctx, "f1"); len(all) != 0 {
		t.Fatalf("DeleteByFederation left %d", len(all))
	}
}
