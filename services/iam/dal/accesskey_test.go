package dal_test

import (
	"testing"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"
	iamv2pb "github.com/nebius/gosdk/proto/nebius/iam/v2"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/iam/dal"
)

func key(id, awsID, saID string) *iamv2pb.AccessKey {
	return &iamv2pb.AccessKey{
		Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: "project-1"},
		Spec: &iamv2pb.AccessKeySpec{Account: &iampb.Account{
			Type: &iampb.Account_ServiceAccount_{ServiceAccount: &iampb.Account_ServiceAccount{Id: saID}},
		}},
		Status: &iamv2pb.AccessKeyStatus{AwsAccessKeyId: awsID, Secret: "s"},
	}
}

func TestAccessKeyRepo(t *testing.T) {
	t.Parallel()
	r := dal.NewAccessKeyRepo(store.NewMemory())
	ctx := t.Context()
	if err := r.Create(ctx, key("k1", "AWS1", "sa1")); err != nil {
		t.Fatal(err)
	}
	if err := r.Create(ctx, key("k2", "AWS1", "sa1")); !errs.IsKind(err, errs.KindAlreadyExists) {
		t.Fatalf("duplicate AWS id: %v, want AlreadyExists", err)
	}
	if err := r.Create(ctx, key("k2", "AWS2", "sa2")); err != nil {
		t.Fatal(err)
	}
	if k, err := r.GetByAwsID(ctx, "AWS2"); err != nil || k.GetMetadata().GetId() != "k2" {
		t.Fatalf("GetByAwsID = %v, %v", k, err)
	}
	if ks, err := r.ListByAccount(ctx, "sa1"); err != nil || len(ks) != 1 {
		t.Fatalf("ListByAccount = %v, %v", ks, err)
	}
	if page, err := r.List(ctx, "project-1", 10, ""); err != nil || len(page.Items) != 2 {
		t.Fatalf("List = %v, %v", page, err)
	}
	if err := r.Delete(ctx, "k1"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.GetByAwsID(ctx, "AWS1"); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("GetByAwsID after delete: %v, want NotFound (index removed)", err)
	}
	if err := r.Create(ctx, key("k3", "AWS1", "sa1")); err != nil {
		t.Fatalf("reuse AWS id after delete: %v", err)
	}
}

func TestServiceAccountRepo(t *testing.T) {
	t.Parallel()
	r := dal.NewServiceAccountRepo(store.NewMemory())
	ctx := t.Context()
	sa := func(id, name string) *iampb.ServiceAccount {
		return &iampb.ServiceAccount{Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: "project-1", Name: name}}
	}
	if err := r.Create(ctx, sa("s1", "a")); err != nil {
		t.Fatal(err)
	}
	if err := r.Create(ctx, sa("s2", "a")); !errs.IsKind(err, errs.KindAlreadyExists) {
		t.Fatalf("duplicate name: %v, want AlreadyExists", err)
	}
	if got, err := r.GetByName(ctx, "project-1", "a"); err != nil || got.GetMetadata().GetId() != "s1" {
		t.Fatalf("GetByName = %v, %v", got, err)
	}
	if err := r.Update(ctx, sa("s9", "z")); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("Update missing: %v, want NotFound", err)
	}
	if err := r.Delete(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Get(ctx, "s1"); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("Get after delete: %v, want NotFound", err)
	}
}
