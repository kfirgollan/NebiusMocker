package domain_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"slices"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/services/iam/domain"
)

// fakePubKeys is a slice-backed domain.AuthPublicKeyRepository.
type fakePubKeys struct{ items []*iampb.AuthPublicKey }

func (f *fakePubKeys) find(pred func(*iampb.AuthPublicKey) bool, key string) (*iampb.AuthPublicKey, error) {
	for _, k := range f.items {
		if pred(k) {
			return proto.CloneOf(k), nil
		}
	}
	return nil, errs.NotFound(key)
}

func (f *fakePubKeys) Get(_ context.Context, id string) (*iampb.AuthPublicKey, error) {
	return f.find(func(k *iampb.AuthPublicKey) bool { return k.GetMetadata().GetId() == id }, id)
}

func (f *fakePubKeys) GetByName(_ context.Context, _, name string) (*iampb.AuthPublicKey, error) {
	return f.find(func(k *iampb.AuthPublicKey) bool { return k.GetMetadata().GetName() == name }, name)
}

func (f *fakePubKeys) List(context.Context, string, int, string) (domain.AuthPublicKeyPage, error) {
	items := make([]*iampb.AuthPublicKey, 0, len(f.items))
	for _, k := range f.items {
		items = append(items, proto.CloneOf(k))
	}
	return domain.AuthPublicKeyPage{Items: items}, nil
}

func (f *fakePubKeys) ListByAccount(_ context.Context, id string) ([]*iampb.AuthPublicKey, error) {
	var out []*iampb.AuthPublicKey
	for _, k := range f.items {
		if k.GetSpec().GetAccount().GetServiceAccount().GetId() == id {
			out = append(out, proto.CloneOf(k))
		}
	}
	return out, nil
}

func (f *fakePubKeys) Create(_ context.Context, k *iampb.AuthPublicKey) error {
	f.items = append(f.items, proto.CloneOf(k))
	return nil
}

func (f *fakePubKeys) Update(_ context.Context, k *iampb.AuthPublicKey) error {
	for i, e := range f.items {
		if e.GetMetadata().GetId() == k.GetMetadata().GetId() {
			f.items[i] = proto.CloneOf(k)
			return nil
		}
	}
	return errs.NotFound(k.GetMetadata().GetId())
}

func (f *fakePubKeys) Delete(_ context.Context, id string) error {
	f.items = slices.DeleteFunc(f.items, func(k *iampb.AuthPublicKey) bool { return k.GetMetadata().GetId() == id })
	return nil
}

// fakeRevoker records domain.TokenRevoker calls.
type fakeRevoker struct{ keys, principals []string }

func (f *fakeRevoker) RevokeKey(id string)       { f.keys = append(f.keys, id) }
func (f *fakeRevoker) RevokePrincipal(id string) { f.principals = append(f.principals, id) }

func pemKey(t *testing.T, bits int) string {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&k.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

func ecPEM(t *testing.T) string {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&k.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

type pubKeyFixture struct {
	*credFixture
	svc *domain.AuthPublicKeyService
	sa  string
}

func newPubKeyFixture(t *testing.T) *pubKeyFixture {
	t.Helper()
	f := newCredFixture(t)
	svc, err := domain.NewAuthPublicKeyService(domain.AuthPublicKeyDeps{
		Keys: f.pkr, ServiceAccounts: f.sar, Projects: fakeProjects{}, Ops: inlineOps{}, IDs: &seqIDs{n: 50}, Clock: f.clk, Revoker: f.rev,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &pubKeyFixture{credFixture: f, svc: svc, sa: f.newSA(t, "app")}
}

func (f *pubKeyFixture) create(t *testing.T, spec *iampb.AuthPublicKeySpec) *iampb.AuthPublicKey {
	t.Helper()
	op, err := f.svc.Create(t.Context(), domain.CreateAuthPublicKeyCmd{Metadata: &commonpb.ResourceMetadata{ParentId: projectID}, Spec: spec})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	k, err := f.svc.Get(t.Context(), op.ResourceID)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestAuthPublicKeyCreate(t *testing.T) {
	t.Parallel()
	f := newPubKeyFixture(t)
	k := f.create(t, &iampb.AuthPublicKeySpec{Account: saAccount(f.sa), Data: pemKey(t, 2048)})
	st := k.GetStatus()
	if st.GetState() != iampb.AuthPublicKeyStatus_ACTIVE || st.GetKeySize() != 2048 || st.GetAlgorithm() == "" || st.GetFingerprint() == "" {
		t.Fatalf("status = %v", st)
	}
	if k.GetMetadata().GetName() != "" || k.GetMetadata().GetParentId() != projectID {
		t.Fatalf("metadata = %v", k.GetMetadata())
	}
}

func TestAuthPublicKeyCreateValidation(t *testing.T) {
	t.Parallel()
	f := newPubKeyFixture(t)
	good := pemKey(t, 2048)
	past := timestamppb.New(f.clk.Now().Add(-time.Hour))
	user := &iampb.Account{Type: &iampb.Account_UserAccount_{UserAccount: &iampb.Account_UserAccount{Id: "useraccount-e00x"}}}
	// An SA that lives in another project.
	f.sar.items = append(f.sar.items, &iampb.ServiceAccount{Metadata: &commonpb.ResourceMetadata{Id: "serviceaccount-e00other", ParentId: "project-e00other", Name: "o"}})
	for name, tt := range map[string]struct {
		parent string
		spec   *iampb.AuthPublicKeySpec
		want   errs.Kind
		field  string
	}{
		"ec key":         {projectID, &iampb.AuthPublicKeySpec{Account: saAccount(f.sa), Data: ecPEM(t)}, errs.KindBadRequest, "spec.data"},
		"garbage":        {projectID, &iampb.AuthPublicKeySpec{Account: saAccount(f.sa), Data: "nope"}, errs.KindBadRequest, "spec.data"},
		"rsa 1024":       {projectID, &iampb.AuthPublicKeySpec{Account: saAccount(f.sa), Data: pemKey(t, 1024)}, errs.KindBadRequest, "spec.data"},
		"no data":        {projectID, &iampb.AuthPublicKeySpec{Account: saAccount(f.sa)}, errs.KindBadRequest, "spec.data"},
		"user account":   {projectID, &iampb.AuthPublicKeySpec{Account: user, Data: good}, errs.KindBadRequest, "spec.account"},
		"no account":     {projectID, &iampb.AuthPublicKeySpec{Data: good}, errs.KindBadRequest, "spec.account"},
		"unknown sa":     {projectID, &iampb.AuthPublicKeySpec{Account: saAccount("serviceaccount-e00nope"), Data: good}, errs.KindNotFound, ""},
		"unknown parent": {"project-e00nope", &iampb.AuthPublicKeySpec{Account: saAccount(f.sa), Data: good}, errs.KindNotFound, ""},
		"no parent":      {"", &iampb.AuthPublicKeySpec{Account: saAccount(f.sa), Data: good}, errs.KindBadRequest, "metadata.parent_id"},
		"wrong project":  {projectID, &iampb.AuthPublicKeySpec{Account: saAccount("serviceaccount-e00other"), Data: good}, errs.KindBadRequest, "metadata.parent_id"},
		"expired":        {projectID, &iampb.AuthPublicKeySpec{Account: saAccount(f.sa), Data: good, ExpiresAt: past}, errs.KindBadRequest, "spec.expires_at"},
	} {
		_, err := f.svc.Create(t.Context(), domain.CreateAuthPublicKeyCmd{Metadata: &commonpb.ResourceMetadata{ParentId: tt.parent}, Spec: tt.spec})
		if got := errs.KindOf(err); got != tt.want {
			t.Errorf("%s: kind %v, want %v (%v)", name, got, tt.want, err)
			continue
		}
		if tt.field != "" && !hasViolation(err, tt.field) {
			t.Errorf("%s: error %v does not name field %s", name, err, tt.field)
		}
	}
	if len(f.pkr.items) != 0 {
		t.Fatalf("rejected creates stored %d keys", len(f.pkr.items))
	}
}

func hasViolation(err error, field string) bool {
	e, ok := errors.AsType[*errs.Error](err)
	if !ok {
		return false
	}
	return slices.ContainsFunc(e.Violations, func(v errs.Violation) bool { return v.Field == field })
}

func TestAuthPublicKeyExpiryAndStates(t *testing.T) {
	t.Parallel()
	f := newPubKeyFixture(t)
	k := f.create(t, &iampb.AuthPublicKeySpec{
		Account: saAccount(f.sa), Data: pemKey(t, 2048), ExpiresAt: timestamppb.New(f.clk.Now().Add(time.Hour)),
	})
	id := k.GetMetadata().GetId()
	f.clk.Advance(time.Hour)
	for name, get := range map[string]func() (*iampb.AuthPublicKey, error){
		"Get":     func() (*iampb.AuthPublicKey, error) { return f.svc.Get(t.Context(), id) },
		"Resolve": func() (*iampb.AuthPublicKey, error) { return f.svc.Resolve(t.Context(), id) },
	} {
		if got, err := get(); err != nil || got.GetStatus().GetState() != iampb.AuthPublicKeyStatus_EXPIRED {
			t.Fatalf("%s after expiry = %v, %v; want EXPIRED", name, got.GetStatus().GetState(), err)
		}
	}
	if page, _ := f.svc.List(t.Context(), projectID, 0, "", ""); page.Items[0].GetStatus().GetState() != iampb.AuthPublicKeyStatus_EXPIRED {
		t.Fatal("List does not apply expiry")
	}

	// Deactivate revokes; Activate does not.
	k2 := f.create(t, &iampb.AuthPublicKeySpec{Account: saAccount(f.sa), Data: pemKey(t, 2048)})
	id2 := k2.GetMetadata().GetId()
	if _, err := f.svc.Deactivate(t.Context(), id2, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.svc.Get(t.Context(), id2); got.GetStatus().GetState() != iampb.AuthPublicKeyStatus_INACTIVE {
		t.Fatalf("state = %v, want INACTIVE", got.GetStatus().GetState())
	}
	if !slices.Equal(f.rev.keys, []string{id2}) {
		t.Fatalf("revoked keys = %v, want [%s]", f.rev.keys, id2)
	}
	if _, err := f.svc.Activate(t.Context(), id2, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.svc.Get(t.Context(), id2); got.GetStatus().GetState() != iampb.AuthPublicKeyStatus_ACTIVE || len(f.rev.keys) != 1 {
		t.Fatalf("after activate: state %v, revoked %v", got.GetStatus().GetState(), f.rev.keys)
	}

	// Delete revokes and removes.
	if _, err := f.svc.Delete(t.Context(), id2, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Get(t.Context(), id2); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("get deleted: %v", err)
	}
	if !slices.Equal(f.rev.keys, []string{id2, id2}) {
		t.Fatalf("revoked keys = %v", f.rev.keys)
	}
	if _, err := f.svc.Delete(t.Context(), id2, nil); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("delete missing: %v, want NotFound", err)
	}
}

func TestAuthPublicKeyUpdate(t *testing.T) {
	t.Parallel()
	f := newPubKeyFixture(t)
	data := pemKey(t, 2048)
	k := f.create(t, &iampb.AuthPublicKeySpec{Account: saAccount(f.sa), Data: data})
	id := k.GetMetadata().GetId()
	update := func(md *commonpb.ResourceMetadata, spec *iampb.AuthPublicKeySpec) error {
		md.Id = id
		_, err := f.svc.Update(t.Context(), domain.UpdateAuthPublicKeyCmd{Metadata: md, Spec: spec})
		return err
	}
	if err := update(&commonpb.ResourceMetadata{Name: "renamed", Labels: map[string]string{"a": "b"}}, &iampb.AuthPublicKeySpec{Description: "d"}); err != nil {
		t.Fatal(err)
	}
	got, _ := f.svc.Get(t.Context(), id)
	if got.GetMetadata().GetName() != "renamed" || got.GetSpec().GetDescription() != "d" || got.GetMetadata().GetResourceVersion() != 2 ||
		got.GetSpec().GetData() != data || got.GetStatus().GetFingerprint() == "" {
		t.Fatalf("after update: %v", got)
	}
	// A no-op update keeps the version.
	if err := update(&commonpb.ResourceMetadata{Name: "renamed", Labels: map[string]string{"a": "b"}}, &iampb.AuthPublicKeySpec{Description: "d"}); err != nil {
		t.Fatal(err)
	}
	if got, _ = f.svc.Get(t.Context(), id); got.GetMetadata().GetResourceVersion() != 2 {
		t.Fatalf("no-op update bumped the version to %d", got.GetMetadata().GetResourceVersion())
	}
	for field, spec := range map[string]*iampb.AuthPublicKeySpec{
		"spec.data":       {Data: pemKey(t, 2048)},
		"spec.account":    {Account: saAccount("serviceaccount-e00other")},
		"spec.expires_at": {ExpiresAt: timestamppb.New(f.clk.Now().Add(time.Hour))},
	} {
		err := update(&commonpb.ResourceMetadata{}, spec)
		if !errs.IsKind(err, errs.KindBadRequest) || !hasViolation(err, field) {
			t.Errorf("update %s: %v, want BadRequest naming the field", field, err)
		}
	}
	if err := update(&commonpb.ResourceMetadata{ResourceVersion: 99}, &iampb.AuthPublicKeySpec{}); !errs.IsKind(err, errs.KindConflict) {
		t.Fatalf("stale version: %v, want Conflict", err)
	}
}

func TestAuthPublicKeyListAndFilter(t *testing.T) {
	t.Parallel()
	f := newPubKeyFixture(t)
	var ids []string
	for range 3 {
		ids = append(ids, f.create(t, &iampb.AuthPublicKeySpec{Account: saAccount(f.sa), Data: pemKey(t, 2048)}).GetMetadata().GetId())
	}
	if _, err := f.svc.List(t.Context(), projectID, 0, "", "name = x"); !errs.IsKind(err, errs.KindBadRequest) || !hasViolation(err, "filter") {
		t.Fatalf("List filter: %v, want BadRequest naming filter", err)
	}
	if _, err := f.svc.ListByAccount(t.Context(), saAccount(f.sa), 0, "", "x"); !errs.IsKind(err, errs.KindBadRequest) || !hasViolation(err, "filter") {
		t.Fatalf("ListByAccount filter: %v, want BadRequest naming filter", err)
	}
	var got []string
	token := ""
	for range 5 {
		page, err := f.svc.ListByAccount(t.Context(), saAccount(f.sa), 2, token, "")
		if err != nil {
			t.Fatal(err)
		}
		for _, k := range page.Items {
			got = append(got, k.GetMetadata().GetId())
		}
		if token = page.NextPageToken; token == "" {
			break
		}
	}
	if !slices.Equal(got, ids) {
		t.Fatalf("ListByAccount pages = %v, want %v", got, ids)
	}
	if _, err := f.svc.ListByAccount(t.Context(), nil, 0, "", ""); !errs.IsKind(err, errs.KindBadRequest) {
		t.Fatalf("ListByAccount without account: %v", err)
	}
	if _, err := f.svc.GetByName(t.Context(), projectID, ""); !errs.IsKind(err, errs.KindBadRequest) {
		t.Fatalf("GetByName empty: %v", err)
	}
}

func TestServiceAccountDeleteCascadesPublicKeys(t *testing.T) {
	t.Parallel()
	f := newPubKeyFixture(t)
	f.create(t, &iampb.AuthPublicKeySpec{Account: saAccount(f.sa), Data: pemKey(t, 2048)})
	if _, err := f.sas.Delete(t.Context(), f.sa, nil); err != nil {
		t.Fatal(err)
	}
	if len(f.pkr.items) != 0 {
		t.Fatalf("public keys after SA delete = %d, want 0", len(f.pkr.items))
	}
	if !slices.Equal(f.rev.principals, []string{f.sa}) {
		t.Fatalf("revoked principals = %v, want [%s]", f.rev.principals, f.sa)
	}
}
