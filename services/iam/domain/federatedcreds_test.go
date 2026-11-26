package domain_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/nebius/gosdk/proto/fieldmask/mask"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/auth"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/services/iam/domain"
)

// fakeFedCredRepo is a slice-backed domain.FederatedCredentialsRepository.
type fakeFedCredRepo struct{ items []*iampb.FederatedCredentials }

func (f *fakeFedCredRepo) find(pred func(*iampb.FederatedCredentials) bool, key string) (*iampb.FederatedCredentials, error) {
	for _, c := range f.items {
		if pred(c) {
			return proto.CloneOf(c), nil
		}
	}
	return nil, errs.NotFound(key)
}

func (f *fakeFedCredRepo) Get(_ context.Context, id string) (*iampb.FederatedCredentials, error) {
	return f.find(func(c *iampb.FederatedCredentials) bool { return c.GetMetadata().GetId() == id }, id)
}

func (f *fakeFedCredRepo) GetByName(_ context.Context, parentID, name string) (*iampb.FederatedCredentials, error) {
	return f.find(func(c *iampb.FederatedCredentials) bool {
		return c.GetMetadata().GetParentId() == parentID && c.GetMetadata().GetName() == name
	}, name)
}

func (f *fakeFedCredRepo) List(_ context.Context, parentID string, _ int, _ string) (domain.FederatedCredentialsPage, error) {
	var page domain.FederatedCredentialsPage
	for _, c := range f.items {
		if c.GetMetadata().GetParentId() == parentID {
			page.Items = append(page.Items, proto.CloneOf(c))
		}
	}
	return page, nil
}

func (f *fakeFedCredRepo) ListBySubject(_ context.Context, saID string) ([]*iampb.FederatedCredentials, error) {
	var out []*iampb.FederatedCredentials
	for _, c := range f.items {
		if c.GetSpec().GetSubjectId() == saID {
			out = append(out, proto.CloneOf(c))
		}
	}
	return out, nil
}

func (f *fakeFedCredRepo) Create(_ context.Context, c *iampb.FederatedCredentials) error {
	f.items = append(f.items, proto.CloneOf(c))
	return nil
}

func (f *fakeFedCredRepo) Update(_ context.Context, c *iampb.FederatedCredentials) error {
	for i, e := range f.items {
		if e.GetMetadata().GetId() == c.GetMetadata().GetId() {
			f.items[i] = proto.CloneOf(c)
			return nil
		}
	}
	return errs.NotFound(c.GetMetadata().GetId())
}

func (f *fakeFedCredRepo) Delete(_ context.Context, id string) error {
	n := len(f.items)
	f.items = slices.DeleteFunc(f.items, func(c *iampb.FederatedCredentials) bool { return c.GetMetadata().GetId() == id })
	if len(f.items) == n {
		return errs.NotFound(id)
	}
	return nil
}

type fedCredFixture struct {
	*credFixture
	svc  *domain.FederatedCredentialsService
	repo *fakeFedCredRepo
	sa   string
}

func newFedCredFixture(t *testing.T) *fedCredFixture {
	t.Helper()
	f := newCredFixture(t)
	repo := &fakeFedCredRepo{}
	svc, err := domain.NewFederatedCredentialsService(domain.FederatedCredentialsDeps{
		Credentials: repo, ServiceAccounts: f.sar, Projects: fakeProjects{}, Ops: inlineOps{}, IDs: &seqIDs{n: 60},
		Clock: f.clk, Revoker: f.rev,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &fedCredFixture{credFixture: f, svc: svc, repo: repo, sa: f.newSA(t, "ci")}
}

func (f *fedCredFixture) spec(issuer, jwks, sub string) *iampb.FederatedCredentialsSpec {
	return &iampb.FederatedCredentialsSpec{
		CredentialsProvider: &iampb.FederatedCredentialsSpec_OidcProvider{OidcProvider: &iampb.OidcCredentialsProvider{
			IssuerUrl: issuer, JwkSetJson: jwks,
		}},
		FederatedSubjectId: sub, SubjectId: f.sa,
	}
}

func (f *fedCredFixture) create(t *testing.T, name string, spec *iampb.FederatedCredentialsSpec) *iampb.FederatedCredentials {
	t.Helper()
	op, err := f.svc.Create(t.Context(), domain.CreateFederatedCredentialsCmd{
		Metadata: &commonpb.ResourceMetadata{ParentId: projectID, Name: name, Labels: map[string]string{"team": "ci"}}, Spec: spec,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	c, err := f.svc.Get(t.Context(), op.ResourceID)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func testJWKS(t *testing.T) string {
	t.Helper()
	priv, err := txRSA()
	if err != nil {
		t.Fatal(err)
	}
	return string(fxJWKS(t, auth.JWK{KeyID: "k1", Key: &priv.PublicKey}))
}

func TestFederatedCredentialsCreateGet(t *testing.T) {
	t.Parallel()
	f := newFedCredFixture(t)
	jwks := testJWKS(t)
	c := f.create(t, "github", f.spec("https://token.actions.githubusercontent.com", jwks, "repo:org/app:ref:main"))
	if !strings.HasPrefix(c.GetMetadata().GetId(), domain.KindFederatedCredentials+"-") || c.GetMetadata().GetParentId() != projectID ||
		c.GetMetadata().GetResourceVersion() != 1 || c.GetMetadata().GetLabels()["team"] != "ci" || c.GetStatus() == nil {
		t.Fatalf("created = %v", c)
	}
	// The spec round-trips byte for byte (Terraform echoes it).
	if got := c.GetSpec().GetOidcProvider().GetJwkSetJson(); got != jwks {
		t.Errorf("jwk_set_json = %q, want it unchanged", got)
	}
	if byName, err := f.svc.GetByName(t.Context(), projectID, "github"); err != nil || byName.GetMetadata().GetId() != c.GetMetadata().GetId() {
		t.Errorf("GetByName = %v, %v", byName, err)
	}
	page, err := f.svc.List(t.Context(), projectID, 0, "", "")
	if err != nil || len(page.Items) != 1 {
		t.Errorf("List = %v, %v", page, err)
	}
	if _, err := f.svc.List(t.Context(), projectID, 0, "", "name = 'github'"); !hasViolation(err, "filter") {
		t.Errorf("List with a filter = %v, want InvalidArgument naming filter", err)
	}
	if _, err := f.svc.GetByName(t.Context(), projectID, ""); !hasViolation(err, "name") {
		t.Errorf("GetByName without a name = %v, want a name violation", err)
	}
}

func TestFederatedCredentialsCreateValidation(t *testing.T) {
	t.Parallel()
	f := newFedCredFixture(t)
	f.sar.items = append(f.sar.items, &iampb.ServiceAccount{Metadata: &commonpb.ResourceMetadata{Id: "serviceaccount-e00other", ParentId: "project-e00other"}})
	good := f.spec("https://issuer.example", "", "ci")
	with := func(edit func(s *iampb.FederatedCredentialsSpec)) *iampb.FederatedCredentialsSpec {
		s := proto.CloneOf(good)
		edit(s)
		return s
	}
	for name, tt := range map[string]struct {
		md    *commonpb.ResourceMetadata
		spec  *iampb.FederatedCredentialsSpec
		want  errs.Kind
		field string
	}{
		"no parent":      {&commonpb.ResourceMetadata{}, good, errs.KindBadRequest, "metadata.parent_id"},
		"tenant parent":  {&commonpb.ResourceMetadata{ParentId: tenantID}, good, errs.KindBadRequest, "metadata.parent_id"},
		"unknown parent": {&commonpb.ResourceMetadata{ParentId: "project-e00nope"}, good, errs.KindNotFound, ""},
		"client id":      {&commonpb.ResourceMetadata{ParentId: projectID, Id: "federatedcredentials-e00x"}, good, errs.KindBadRequest, "metadata.id"},
		"no provider": {
			nil, with(func(s *iampb.FederatedCredentialsSpec) { s.CredentialsProvider = nil }),
			errs.KindBadRequest, "spec.oidc_provider",
		},
		"no issuer": {
			nil, with(func(s *iampb.FederatedCredentialsSpec) { s.GetOidcProvider().IssuerUrl = "" }),
			errs.KindBadRequest, "spec.oidc_provider.issuer_url",
		},
		"relative issuer": {
			nil, with(func(s *iampb.FederatedCredentialsSpec) { s.GetOidcProvider().IssuerUrl = "issuer.example" }),
			errs.KindBadRequest, "spec.oidc_provider.issuer_url",
		},
		"ftp issuer": {
			nil, with(func(s *iampb.FederatedCredentialsSpec) { s.GetOidcProvider().IssuerUrl = "ftp://issuer.example" }),
			errs.KindBadRequest, "spec.oidc_provider.issuer_url",
		},
		"issuer with query": {
			nil, with(func(s *iampb.FederatedCredentialsSpec) { s.GetOidcProvider().IssuerUrl = "https://i.example/?a=b" }),
			errs.KindBadRequest, "spec.oidc_provider.issuer_url",
		},
		"jwks not JSON": {
			nil, with(func(s *iampb.FederatedCredentialsSpec) { s.GetOidcProvider().JwkSetJson = "nope" }),
			errs.KindBadRequest, "spec.oidc_provider.jwk_set_json",
		},
		"jwks without supported keys": {nil, with(func(s *iampb.FederatedCredentialsSpec) {
			s.GetOidcProvider().JwkSetJson = `{"keys":[{"kty":"OKP","crv":"Ed25519","x":"AA"}]}`
		}), errs.KindBadRequest, "spec.oidc_provider.jwk_set_json"},
		"no federated subject": {
			nil, with(func(s *iampb.FederatedCredentialsSpec) { s.FederatedSubjectId = "" }),
			errs.KindBadRequest, "spec.federated_subject_id",
		},
		"no subject": {nil, with(func(s *iampb.FederatedCredentialsSpec) { s.SubjectId = "" }), errs.KindBadRequest, "spec.subject_id"},
		"subject not an SA": {
			nil, with(func(s *iampb.FederatedCredentialsSpec) { s.SubjectId = "useraccount-e00x" }),
			errs.KindBadRequest, "spec.subject_id",
		},
		"unknown SA": {
			nil, with(func(s *iampb.FederatedCredentialsSpec) { s.SubjectId = "serviceaccount-e00nope" }),
			errs.KindBadRequest, "spec.subject_id",
		},
		"SA of another project": {
			nil, with(func(s *iampb.FederatedCredentialsSpec) { s.SubjectId = "serviceaccount-e00other" }),
			errs.KindBadRequest, "spec.subject_id",
		},
	} {
		md := tt.md
		if md == nil {
			md = &commonpb.ResourceMetadata{ParentId: projectID}
		}
		_, err := f.svc.Create(t.Context(), domain.CreateFederatedCredentialsCmd{Metadata: md, Spec: tt.spec})
		if got := errs.KindOf(err); got != tt.want {
			t.Errorf("%s: kind %v, want %v (%v)", name, got, tt.want, err)
			continue
		}
		if tt.field != "" && !hasViolation(err, tt.field) {
			t.Errorf("%s: error %v does not name field %s", name, err, tt.field)
		}
	}
	if len(f.repo.items) != 0 {
		t.Fatalf("rejected creates stored %d credentials", len(f.repo.items))
	}
	// The federated subject refusal explains itself.
	_, err := f.svc.Create(t.Context(), domain.CreateFederatedCredentialsCmd{
		Metadata: &commonpb.ResourceMetadata{ParentId: projectID}, Spec: with(func(s *iampb.FederatedCredentialsSpec) { s.FederatedSubjectId = "" }),
	})
	if err == nil || !strings.Contains(err.Error(), "does not trust every subject") {
		t.Errorf("missing federated_subject_id error = %v", err)
	}
}

func (f *fedCredFixture) update(t *testing.T, md *commonpb.ResourceMetadata, spec *iampb.FederatedCredentialsSpec, reset *mask.Mask) error {
	t.Helper()
	_, err := f.svc.Update(t.Context(), domain.UpdateFederatedCredentialsCmd{Metadata: md, Spec: spec, ResetMask: reset})
	return err
}

func TestFederatedCredentialsUpdate(t *testing.T) {
	t.Parallel()
	f := newFedCredFixture(t)
	c := f.create(t, "gh", f.spec("https://issuer.example", "", "ci"))
	id := c.GetMetadata().GetId()

	// Labels only: a new version, but the trust is unchanged, so no revocation.
	if err := f.update(t, &commonpb.ResourceMetadata{Id: id, Name: "gh", Labels: map[string]string{"team": "infra"}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	got, _ := f.svc.Get(t.Context(), id)
	if got.GetMetadata().GetLabels()["team"] != "infra" || got.GetMetadata().GetResourceVersion() != 2 || len(f.rev.keys) != 0 {
		t.Fatalf("after label update: %v, revoked %v", got.GetMetadata(), f.rev.keys)
	}
	// A spec change revokes the tokens exchanged through the credentials.
	jwks := testJWKS(t)
	if err := f.update(t, &commonpb.ResourceMetadata{Id: id}, f.spec("https://issuer.example", jwks, "deploy"), nil); err != nil {
		t.Fatal(err)
	}
	got, _ = f.svc.Get(t.Context(), id)
	if got.GetSpec().GetFederatedSubjectId() != "deploy" || got.GetSpec().GetOidcProvider().GetJwkSetJson() != jwks ||
		got.GetMetadata().GetResourceVersion() != 3 || !slices.Equal(f.rev.keys, []string{id}) {
		t.Fatalf("after spec update: %v, revoked %v", got, f.rev.keys)
	}
	// Resetting jwk_set_json falls back to discovery.
	if err := f.update(t, &commonpb.ResourceMetadata{Id: id}, f.spec("https://issuer.example", "", "deploy"),
		mask.ParseMust("spec.oidc_provider.jwk_set_json")); err != nil {
		t.Fatal(err)
	}
	if got, _ = f.svc.Get(t.Context(), id); got.GetSpec().GetOidcProvider().GetJwkSetJson() != "" {
		t.Fatalf("jwk_set_json after reset = %q", got.GetSpec().GetOidcProvider().GetJwkSetJson())
	}
	// A no-op update keeps the version.
	if err := f.update(t, &commonpb.ResourceMetadata{Id: id}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if v := mustGetFedCred(t, f, id).GetMetadata().GetResourceVersion(); v != 4 {
		t.Errorf("version after no-op = %d, want 4", v)
	}

	for name, tt := range map[string]struct {
		md    *commonpb.ResourceMetadata
		spec  *iampb.FederatedCredentialsSpec
		want  errs.Kind
		field string
	}{
		"no id":         {&commonpb.ResourceMetadata{}, nil, errs.KindBadRequest, "metadata.id"},
		"unknown":       {&commonpb.ResourceMetadata{Id: "federatedcredentials-e00nope"}, nil, errs.KindNotFound, ""},
		"stale version": {&commonpb.ResourceMetadata{Id: id, ResourceVersion: 1}, nil, errs.KindConflict, ""},
		"move parent":   {&commonpb.ResourceMetadata{Id: id, ParentId: "project-e00other"}, nil, errs.KindBadRequest, "metadata.parent_id"},
		"bad issuer":    {&commonpb.ResourceMetadata{Id: id}, f.spec("nope", "", "ci"), errs.KindBadRequest, "spec.oidc_provider.issuer_url"},
		"bad jwks":      {&commonpb.ResourceMetadata{Id: id}, f.spec("https://issuer.example", "{}", "ci"), errs.KindBadRequest, "spec.oidc_provider.jwk_set_json"},
		"unknown subject": {
			&commonpb.ResourceMetadata{Id: id}, &iampb.FederatedCredentialsSpec{SubjectId: "serviceaccount-e00nope"},
			errs.KindBadRequest, "spec.subject_id",
		},
	} {
		err := f.update(t, tt.md, tt.spec, nil)
		if got := errs.KindOf(err); got != tt.want {
			t.Errorf("%s: kind %v, want %v (%v)", name, got, tt.want, err)
			continue
		}
		if tt.field != "" && !hasViolation(err, tt.field) {
			t.Errorf("%s: error %v does not name field %s", name, err, tt.field)
		}
	}
}

func mustGetFedCred(t *testing.T, f *fedCredFixture, id string) *iampb.FederatedCredentials {
	t.Helper()
	c, err := f.svc.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestFederatedCredentialsDelete(t *testing.T) {
	t.Parallel()
	f := newFedCredFixture(t)
	id := f.create(t, "", f.spec("https://issuer.example", "", "ci")).GetMetadata().GetId()
	if _, err := f.svc.Delete(t.Context(), id, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Get(t.Context(), id); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("Get after delete = %v, want NotFound", err)
	}
	if !slices.Equal(f.rev.keys, []string{id}) {
		t.Errorf("revoked keys = %v, want [%s]", f.rev.keys, id)
	}
	if _, err := f.svc.Delete(t.Context(), id, nil); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("second delete = %v, want NotFound", err)
	}
}

func TestNewFederatedCredentialsServiceRequiresDeps(t *testing.T) {
	t.Parallel()
	if _, err := domain.NewFederatedCredentialsService(domain.FederatedCredentialsDeps{}); err == nil {
		t.Error("NewFederatedCredentialsService with no deps succeeded")
	}
}
