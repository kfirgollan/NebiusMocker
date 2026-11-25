package domain_test

import (
	"context"
	"regexp"
	"slices"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"
	iamv2pb "github.com/nebius/gosdk/proto/nebius/iam/v2"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/services/iam/domain"
)

const projectID = "project-e00test0000000000001"

type fakeProjects struct{}

func (fakeProjects) Get(_ context.Context, id string) (*iampb.Container, error) {
	if id != projectID {
		return nil, errs.NotFound(id)
	}
	return &iampb.Container{Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: tenantID}}, nil
}

func (fakeProjects) Ensure(context.Context, *iampb.Container) error { return nil }

// fakeSAs is a slice-backed domain.ServiceAccountRepository.
type fakeSAs struct{ items []*iampb.ServiceAccount }

func (f *fakeSAs) Get(_ context.Context, id string) (*iampb.ServiceAccount, error) {
	for _, s := range f.items {
		if s.GetMetadata().GetId() == id {
			return proto.CloneOf(s), nil
		}
	}
	return nil, errs.NotFound(id)
}

func (f *fakeSAs) GetByName(_ context.Context, parentID, name string) (*iampb.ServiceAccount, error) {
	for _, s := range f.items {
		if s.GetMetadata().GetParentId() == parentID && s.GetMetadata().GetName() == name {
			return proto.CloneOf(s), nil
		}
	}
	return nil, errs.NotFound(name)
}

func (f *fakeSAs) List(context.Context, string, int, string) (domain.ServiceAccountPage, error) {
	return domain.ServiceAccountPage{Items: f.items}, nil
}

func (f *fakeSAs) Create(ctx context.Context, s *iampb.ServiceAccount) error {
	if _, err := f.GetByName(ctx, s.GetMetadata().GetParentId(), s.GetMetadata().GetName()); err == nil {
		return errs.AlreadyExists("", "taken")
	}
	f.items = append(f.items, proto.CloneOf(s))
	return nil
}

func (f *fakeSAs) Update(_ context.Context, s *iampb.ServiceAccount) error {
	for i, e := range f.items {
		if e.GetMetadata().GetId() == s.GetMetadata().GetId() {
			f.items[i] = proto.CloneOf(s)
			return nil
		}
	}
	return errs.NotFound(s.GetMetadata().GetId())
}

func (f *fakeSAs) Delete(_ context.Context, id string) error {
	f.items = slices.DeleteFunc(f.items, func(s *iampb.ServiceAccount) bool { return s.GetMetadata().GetId() == id })
	return nil
}

// fakeKeys is a slice-backed domain.AccessKeyRepository.
type fakeKeys struct {
	items      []*iamv2pb.AccessKey
	failCreate error // returned by Create when set
}

func (f *fakeKeys) find(pred func(*iamv2pb.AccessKey) bool, key string) (*iamv2pb.AccessKey, error) {
	for _, k := range f.items {
		if pred(k) {
			return proto.CloneOf(k), nil
		}
	}
	return nil, errs.NotFound(key)
}

func (f *fakeKeys) Get(_ context.Context, id string) (*iamv2pb.AccessKey, error) {
	return f.find(func(k *iamv2pb.AccessKey) bool { return k.GetMetadata().GetId() == id }, id)
}

func (f *fakeKeys) GetByName(_ context.Context, _, name string) (*iamv2pb.AccessKey, error) {
	return f.find(func(k *iamv2pb.AccessKey) bool { return k.GetMetadata().GetName() == name }, name)
}

func (f *fakeKeys) GetByAwsID(_ context.Context, aws string) (*iamv2pb.AccessKey, error) {
	return f.find(func(k *iamv2pb.AccessKey) bool { return k.GetStatus().GetAwsAccessKeyId() == aws }, aws)
}

func (f *fakeKeys) List(context.Context, string, int, string) (domain.AccessKeyPage, error) {
	items := make([]*iamv2pb.AccessKey, 0, len(f.items)) // copies, like the store: callers may modify the page
	for _, k := range f.items {
		items = append(items, proto.CloneOf(k))
	}
	return domain.AccessKeyPage{Items: items}, nil
}

func (f *fakeKeys) ListByAccount(_ context.Context, id string) ([]*iamv2pb.AccessKey, error) {
	var out []*iamv2pb.AccessKey
	for _, k := range f.items {
		if k.GetSpec().GetAccount().GetServiceAccount().GetId() == id {
			out = append(out, proto.CloneOf(k))
		}
	}
	return out, nil
}

func (f *fakeKeys) Create(_ context.Context, k *iamv2pb.AccessKey) error {
	if f.failCreate != nil {
		return f.failCreate
	}
	f.items = append(f.items, proto.CloneOf(k))
	return nil
}

func (f *fakeKeys) Update(_ context.Context, k *iamv2pb.AccessKey) error {
	for i, e := range f.items {
		if e.GetMetadata().GetId() == k.GetMetadata().GetId() {
			f.items[i] = proto.CloneOf(k)
			return nil
		}
	}
	return errs.NotFound(k.GetMetadata().GetId())
}

func (f *fakeKeys) Delete(_ context.Context, id string) error {
	f.items = slices.DeleteFunc(f.items, func(k *iamv2pb.AccessKey) bool { return k.GetMetadata().GetId() == id })
	return nil
}

type credFixture struct {
	sas  *domain.ServiceAccountService
	keys *domain.AccessKeyService
	sar  *fakeSAs
	kr   *fakeKeys
	clk  *clock.Manual
}

func newCredFixture(t *testing.T) *credFixture {
	t.Helper()
	return newCredFixtureWithPeers(t, nil)
}

// newCredFixtureWithPeers is newCredFixture with peer capabilities (e.g.
// peer.SystemSecrets for MYSTERY_BOX delivery).
func newCredFixtureWithPeers(t *testing.T, peers *peer.Directory) *credFixture {
	t.Helper()
	f := &credFixture{sar: &fakeSAs{}, kr: &fakeKeys{}, clk: clock.NewManual(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))}
	ids := &seqIDs{}
	var err error
	f.sas, err = domain.NewServiceAccountService(domain.ServiceAccountDeps{
		ServiceAccounts: f.sar, Projects: fakeProjects{}, AccessKeys: f.kr, Ops: inlineOps{}, IDs: ids, Clock: f.clk,
	})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	f.keys, err = domain.NewAccessKeyService(domain.AccessKeyDeps{
		AccessKeys: f.kr, ServiceAccounts: f.sar, Projects: fakeProjects{}, Ops: inlineOps{}, IDs: ids, Clock: f.clk,
		Peers: peers,
		Material: func() (string, string, error) {
			n++
			return "NMKTEST" + string(rune('A'+n)), "secret-" + string(rune('a'+n)), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *credFixture) newSA(t *testing.T, name string) string {
	t.Helper()
	op, err := f.sas.Create(t.Context(), domain.CreateServiceAccountCmd{Metadata: &commonpb.ResourceMetadata{ParentId: projectID, Name: name}})
	if err != nil {
		t.Fatalf("create SA: %v", err)
	}
	return op.ResourceID
}

func saAccount(id string) *iampb.Account {
	return &iampb.Account{Type: &iampb.Account_ServiceAccount_{ServiceAccount: &iampb.Account_ServiceAccount{Id: id}}}
}

func (f *credFixture) newKey(t *testing.T, spec *iamv2pb.AccessKeySpec) *iamv2pb.AccessKey {
	t.Helper()
	op, err := f.keys.Create(t.Context(), domain.CreateAccessKeyCmd{Metadata: &commonpb.ResourceMetadata{ParentId: projectID}, Spec: spec})
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	k, err := f.keys.Get(t.Context(), op.ResourceID)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestGenerateKeyMaterial(t *testing.T) {
	t.Parallel()
	id, secret, err := domain.GenerateKeyMaterial()
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^NMK[A-Z2-7]{17}$`).MatchString(id) || !regexp.MustCompile(`^[A-Za-z0-9+/]{40}$`).MatchString(secret) {
		t.Fatalf("GenerateKeyMaterial = %q, %q (len %d)", id, secret, len(secret))
	}
	id2, secret2, _ := domain.GenerateKeyMaterial()
	if id == id2 || secret == secret2 {
		t.Fatal("GenerateKeyMaterial is not random")
	}
}

func TestAccessKeyCreateValidation(t *testing.T) {
	t.Parallel()
	f := newCredFixture(t)
	sa := f.newSA(t, "app")
	past := timestamppb.New(f.clk.Now().Add(-time.Hour))
	for name, tt := range map[string]struct {
		parent string
		spec   *iamv2pb.AccessKeySpec
		want   errs.Kind
	}{
		"no account":      {projectID, &iamv2pb.AccessKeySpec{}, errs.KindBadRequest},
		"unknown account": {projectID, &iamv2pb.AccessKeySpec{Account: saAccount("serviceaccount-e00nope")}, errs.KindNotFound},
		"unknown parent":  {"project-e00nope", &iamv2pb.AccessKeySpec{Account: saAccount(sa)}, errs.KindNotFound},
		"mysterybox off":  {projectID, &iamv2pb.AccessKeySpec{Account: saAccount(sa), SecretDeliveryMode: iamv2pb.SecretDeliveryMode_MYSTERY_BOX}, errs.KindBadState},
		"expired":         {projectID, &iamv2pb.AccessKeySpec{Account: saAccount(sa), ExpiresAt: past}, errs.KindBadRequest},
	} {
		_, err := f.keys.Create(t.Context(), domain.CreateAccessKeyCmd{Metadata: &commonpb.ResourceMetadata{ParentId: tt.parent}, Spec: tt.spec})
		if got := errs.KindOf(err); got != tt.want {
			t.Errorf("%s: kind %v, want %v (%v)", name, got, tt.want, err)
		}
	}
}

func TestAccessKeySecretDeliveryAndResolve(t *testing.T) {
	t.Parallel()
	f := newCredFixture(t)
	sa := f.newSA(t, "app")
	inline := f.newKey(t, &iamv2pb.AccessKeySpec{Account: saAccount(sa)}) // unspecified delivery means inline
	explicit := f.newKey(t, &iamv2pb.AccessKeySpec{
		Account: saAccount(sa), SecretDeliveryMode: iamv2pb.SecretDeliveryMode_EXPLICIT,
		ExpiresAt: timestamppb.New(f.clk.Now().Add(time.Hour)),
	})
	if inline.GetStatus().GetSecret() == "" || explicit.GetStatus().GetSecret() != "" {
		t.Fatalf("secrets in Get: inline %q, explicit %q; want inline only", inline.GetStatus().GetSecret(), explicit.GetStatus().GetSecret())
	}
	sec, err := f.keys.GetSecret(t.Context(), explicit.GetMetadata().GetId())
	if err != nil || sec.Secret == "" || sec.AwsAccessKeyID != explicit.GetStatus().GetAwsAccessKeyId() {
		t.Fatalf("GetSecret = %+v, %v", sec, err)
	}
	aws := explicit.GetStatus().GetAwsAccessKeyId()
	k, err := f.keys.ResolveAccessKey(t.Context(), aws)
	if err != nil || k.PrincipalID != sa || k.Secret != sec.Secret {
		t.Fatalf("ResolveAccessKey = %+v, %v", k, err)
	}

	// Expiry: the key reports EXPIRED and stops resolving.
	f.clk.Advance(time.Hour)
	if got, _ := f.keys.Get(t.Context(), explicit.GetMetadata().GetId()); got.GetStatus().GetState() != iamv2pb.AccessKeyStatus_EXPIRED {
		t.Fatalf("state after expiry = %v, want EXPIRED", got.GetStatus().GetState())
	}
	if _, err := f.keys.ResolveAccessKey(t.Context(), aws); !errs.IsKind(err, errs.KindUnauthenticated) {
		t.Fatalf("resolve expired: %v, want Unauthenticated", err)
	}

	// Deactivation and reactivation.
	inlineAws := inline.GetStatus().GetAwsAccessKeyId()
	if _, err := f.keys.Deactivate(t.Context(), inline.GetMetadata().GetId(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := f.keys.ResolveAccessKey(t.Context(), inlineAws); !errs.IsKind(err, errs.KindUnauthenticated) {
		t.Fatalf("resolve inactive: %v, want Unauthenticated", err)
	}
	if _, err := f.keys.Activate(t.Context(), inline.GetMetadata().GetId(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := f.keys.ResolveAccessKey(t.Context(), inlineAws); err != nil {
		t.Fatalf("resolve reactivated: %v", err)
	}
	if _, err := f.keys.ResolveAccessKey(t.Context(), "NMKUNKNOWN"); !errs.IsKind(err, errs.KindUnauthenticated) {
		t.Fatalf("resolve unknown: %v, want Unauthenticated", err)
	}

	// Deleting the service account cascades to its keys.
	if _, err := f.sas.Delete(t.Context(), sa, nil); err != nil {
		t.Fatal(err)
	}
	if len(f.kr.items) != 0 {
		t.Fatalf("keys after SA delete = %d, want 0", len(f.kr.items))
	}
}

// fakeSystemSecrets records peer.SystemSecrets calls.
type fakeSystemSecrets struct {
	created   []peer.SystemSecretSpec
	discarded []string
	failWith  error
}

func (f *fakeSystemSecrets) CreateSystemSecret(_ context.Context, spec peer.SystemSecretSpec) (peer.SystemSecret, error) {
	if f.failWith != nil {
		return peer.SystemSecret{}, f.failWith
	}
	f.created = append(f.created, spec)
	return peer.SystemSecret{SecretID: "mbsec-e00test" + string(rune('0'+len(f.created))), VersionID: "mbsecver-e00test"}, nil
}

func (f *fakeSystemSecrets) DiscardSystemSecret(_ context.Context, id string) error {
	f.discarded = append(f.discarded, id)
	return nil
}

func mysteryBoxFixture(t *testing.T) (*credFixture, *fakeSystemSecrets) {
	t.Helper()
	d := peer.NewDirectory()
	mb := &fakeSystemSecrets{}
	if err := peer.Provide[peer.SystemSecrets](d, peer.SystemSecretsKey, mb); err != nil {
		t.Fatal(err)
	}
	return newCredFixtureWithPeers(t, d), mb
}

// TestAccessKeyMysteryBoxDelivery: the secret goes to a MysteryBox secret in
// the key's project, status.secret_reference_id names it, and the secret is
// never returned inline; the key itself works as usual.
func TestAccessKeyMysteryBoxDelivery(t *testing.T) {
	t.Parallel()
	f, mb := mysteryBoxFixture(t)
	sa := f.newSA(t, "app")
	k := f.newKey(t, &iamv2pb.AccessKeySpec{Account: saAccount(sa), SecretDeliveryMode: iamv2pb.SecretDeliveryMode_MYSTERY_BOX})
	if len(mb.created) != 1 {
		t.Fatalf("system secrets created = %d, want 1", len(mb.created))
	}
	spec := mb.created[0]
	stored, _ := f.kr.Get(t.Context(), k.GetMetadata().GetId())
	secret := stored.GetStatus().GetSecret()
	if spec.ParentID != projectID || spec.Name != k.GetMetadata().GetId() || len(spec.Entries) != 1 || secret == "" ||
		spec.Entries[0].Key != domain.MysteryBoxSecretKey || spec.Entries[0].StringValue != secret || spec.Entries[0].Binary {
		t.Fatalf("system secret spec = %+v, want one %q entry with the key secret in %s", spec, domain.MysteryBoxSecretKey, projectID)
	}
	if got := k.GetStatus().GetSecretReferenceId(); got != "mbsec-e00test1" {
		t.Fatalf("status.secret_reference_id = %q, want mbsec-e00test1", got)
	}
	if k.GetStatus().GetSecret() != "" {
		t.Fatal("MYSTERY_BOX key returned its secret inline")
	}
	listed, err := f.keys.List(t.Context(), projectID, 0, "")
	if err != nil || len(listed.Items) != 1 || listed.Items[0].GetStatus().GetSecret() != "" {
		t.Fatalf("List = %v, %v; want the key without its secret", listed.Items, err)
	}
	if r, err := f.keys.ResolveAccessKey(t.Context(), k.GetStatus().GetAwsAccessKeyId()); err != nil || r.Secret != secret {
		t.Fatalf("ResolveAccessKey = %+v, %v", r, err)
	}

	// Deleting the key leaves the MysteryBox secret alone (Nebius documents
	// no effect on it).
	if _, err := f.keys.Delete(t.Context(), k.GetMetadata().GetId(), nil); err != nil {
		t.Fatal(err)
	}
	if len(mb.discarded) != 0 {
		t.Fatalf("delete discarded %v, want no MysteryBox change", mb.discarded)
	}
}

// TestAccessKeyMysteryBoxRollback: a key that cannot be stored leaves no
// MysteryBox secret behind, and MysteryBox errors fail the create.
func TestAccessKeyMysteryBoxRollback(t *testing.T) {
	t.Parallel()
	f, mb := mysteryBoxFixture(t)
	sa := f.newSA(t, "app")
	spec := &iamv2pb.AccessKeySpec{Account: saAccount(sa), SecretDeliveryMode: iamv2pb.SecretDeliveryMode_MYSTERY_BOX}
	create := func() error {
		_, err := f.keys.Create(t.Context(), domain.CreateAccessKeyCmd{Metadata: &commonpb.ResourceMetadata{ParentId: projectID}, Spec: spec})
		return err
	}

	f.kr.failCreate = errs.AlreadyExists("accesskey-e00other", "name taken")
	if err := create(); !errs.IsKind(err, errs.KindAlreadyExists) {
		t.Fatalf("create with failing store: %v, want AlreadyExists", err)
	}
	if len(mb.created) != 1 || len(mb.discarded) != 1 || mb.discarded[0] != "mbsec-e00test1" {
		t.Fatalf("created %d, discarded %v; want the one created secret discarded", len(mb.created), mb.discarded)
	}

	f.kr.failCreate = nil
	mb.failWith = errs.NotFound(projectID)
	if err := create(); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("create with failing MysteryBox: %v, want NotFound", err)
	}
	if len(f.kr.items) != 0 {
		t.Fatalf("stored keys = %d, want 0", len(f.kr.items))
	}
}

func TestAccessKeyUpdate(t *testing.T) {
	t.Parallel()
	f := newCredFixture(t)
	sa := f.newSA(t, "app")
	other := f.newSA(t, "other")
	k := f.newKey(t, &iamv2pb.AccessKeySpec{Account: saAccount(sa), Description: "a", SecretDeliveryMode: iamv2pb.SecretDeliveryMode_EXPLICIT})
	id := k.GetMetadata().GetId()
	for name, spec := range map[string]*iamv2pb.AccessKeySpec{
		"account": {Account: saAccount(other)},
		"expiry":  {ExpiresAt: timestamppb.New(f.clk.Now().Add(time.Hour))},
		"mode":    {SecretDeliveryMode: iamv2pb.SecretDeliveryMode_INLINE},
	} {
		if _, err := f.keys.Update(t.Context(), domain.UpdateAccessKeyCmd{Metadata: &commonpb.ResourceMetadata{Id: id}, Spec: spec}); !errs.IsKind(err, errs.KindBadRequest) {
			t.Errorf("change %s: %v, want BadRequest", name, err)
		}
	}
	if _, err := f.keys.Update(t.Context(), domain.UpdateAccessKeyCmd{
		Metadata: &commonpb.ResourceMetadata{Id: id, Name: "named"},
		Spec:     &iamv2pb.AccessKeySpec{Account: saAccount(sa), Description: "b"},
	}); err != nil {
		t.Fatal(err)
	}
	got, _ := f.keys.Get(t.Context(), id)
	if got.GetSpec().GetDescription() != "b" || got.GetMetadata().GetName() != "named" || got.GetMetadata().GetResourceVersion() != 2 ||
		got.GetStatus().GetAwsAccessKeyId() != k.GetStatus().GetAwsAccessKeyId() || got.GetSpec().GetSecretDeliveryMode() != iamv2pb.SecretDeliveryMode_EXPLICIT {
		t.Fatalf("after update = %v", got)
	}
}

func TestServiceAccountRules(t *testing.T) {
	t.Parallel()
	f := newCredFixture(t)
	id := f.newSA(t, "app")
	for name, md := range map[string]*commonpb.ResourceMetadata{
		"no name":   {ParentId: projectID},
		"client id": {Id: "serviceaccount-x", ParentId: projectID, Name: "x"},
		"tenant":    {ParentId: tenantID, Name: "x"},
	} {
		if _, err := f.sas.Create(t.Context(), domain.CreateServiceAccountCmd{Metadata: md}); err == nil {
			t.Errorf("%s: Create succeeded, want error", name)
		}
	}
	if _, err := f.sas.Update(t.Context(), domain.UpdateServiceAccountCmd{
		Metadata: &commonpb.ResourceMetadata{Id: id, Name: "app", ResourceVersion: 9},
	}); !errs.IsKind(err, errs.KindConflict) {
		t.Errorf("stale resource_version: %v, want Conflict", err)
	}
	if _, err := f.sas.Update(t.Context(), domain.UpdateServiceAccountCmd{
		Metadata: &commonpb.ResourceMetadata{Id: id, Name: "renamed"}, Spec: &iampb.ServiceAccountSpec{Description: "d"},
	}); err != nil {
		t.Fatal(err)
	}
	got, _ := f.sas.Get(t.Context(), id)
	if got.GetMetadata().GetName() != "renamed" || got.GetSpec().GetDescription() != "d" || !got.GetStatus().GetActive() || got.GetMetadata().GetResourceVersion() != 2 {
		t.Fatalf("after update = %v", got)
	}
}
