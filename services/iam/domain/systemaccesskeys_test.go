package domain_test

import (
	"testing"

	iamv2pb "github.com/nebius/gosdk/proto/nebius/iam/v2"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
)

func TestSystemAccessKeyCreateIsAnOrdinaryKey(t *testing.T) {
	t.Parallel()
	f := newCredFixture(t)
	sa := f.newSA(t, "mlflow-sa")
	got, err := f.keys.CreateSystemAccessKey(t.Context(), peer.SystemAccessKeySpec{
		ParentID: projectID, Name: "mlflow-x", Description: "artifacts", AccountID: sa,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.KeyID == "" || got.AccessKeyID == "" || got.Secret == "" {
		t.Fatalf("incomplete key %+v", got)
	}
	// Visible through the public service API.
	k, err := f.keys.Get(t.Context(), got.KeyID)
	if err != nil {
		t.Fatal(err)
	}
	if k.GetMetadata().GetName() != "mlflow-x" || k.GetSpec().GetDescription() != "artifacts" ||
		k.GetSpec().GetAccount().GetServiceAccount().GetId() != sa || k.GetStatus().GetState() != iamv2pb.AccessKeyStatus_ACTIVE {
		t.Fatalf("stored key %v", k)
	}
	byName, err := f.keys.GetByName(t.Context(), projectID, "mlflow-x")
	if err != nil || byName.GetMetadata().GetId() != got.KeyID {
		t.Fatalf("GetByName = %v, %v", byName, err)
	}
	// Usable by the S3 front.
	r, err := f.keys.ResolveAccessKey(t.Context(), got.AccessKeyID)
	if err != nil || r.PrincipalID != sa || r.Secret != got.Secret || r.KeyID != got.KeyID {
		t.Fatalf("ResolveAccessKey = %+v, %v", r, err)
	}
}

func TestSystemAccessKeyErrors(t *testing.T) {
	t.Parallel()
	f := newCredFixture(t)
	sa := f.newSA(t, "sa")
	for name, tt := range map[string]struct {
		spec peer.SystemAccessKeySpec
		want errs.Kind
	}{
		"unknown account": {peer.SystemAccessKeySpec{ParentID: projectID, Name: "k", AccountID: "serviceaccount-e00nope"}, errs.KindNotFound},
		"unknown project": {peer.SystemAccessKeySpec{ParentID: "project-e00nope", Name: "k", AccountID: sa}, errs.KindNotFound},
		"no account":      {peer.SystemAccessKeySpec{ParentID: projectID, Name: "k"}, errs.KindBadRequest},
	} {
		if _, err := f.keys.CreateSystemAccessKey(t.Context(), tt.spec); errs.KindOf(err) != tt.want {
			t.Errorf("%s: kind %v, want %v (%v)", name, errs.KindOf(err), tt.want, err)
		}
	}
	f.kr.failCreate = errs.AlreadyExists("", "taken")
	_, err := f.keys.CreateSystemAccessKey(t.Context(), peer.SystemAccessKeySpec{ParentID: projectID, Name: "dup", AccountID: sa})
	if !errs.IsKind(err, errs.KindAlreadyExists) {
		t.Errorf("duplicate name: %v", err)
	}
}

func TestSystemAccessKeyDelete(t *testing.T) {
	t.Parallel()
	f := newCredFixture(t)
	sa := f.newSA(t, "sa")
	got, err := f.keys.CreateSystemAccessKey(t.Context(), peer.SystemAccessKeySpec{ParentID: projectID, Name: "k", AccountID: sa})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.keys.DeleteSystemAccessKey(t.Context(), got.KeyID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.keys.Get(t.Context(), got.KeyID); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("key still there: %v", err)
	}
	if err := f.keys.DeleteSystemAccessKey(t.Context(), got.KeyID); err != nil {
		t.Fatalf("deleting a missing key: %v", err)
	}
}
