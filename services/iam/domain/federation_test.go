package domain_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"math/big"
	"slices"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/services/iam/domain"
)

// fakeFederations is a slice-backed domain.FederationRepository.
type fakeFederations struct{ items []*iampb.Federation }

func (f *fakeFederations) Get(_ context.Context, id string) (*iampb.Federation, error) {
	for _, e := range f.items {
		if e.GetMetadata().GetId() == id {
			return proto.CloneOf(e), nil
		}
	}
	return nil, errs.NotFound(id)
}

func (f *fakeFederations) GetByName(_ context.Context, parent, name string) (*iampb.Federation, error) {
	for _, e := range f.items {
		if e.GetMetadata().GetParentId() == parent && e.GetMetadata().GetName() == name {
			return proto.CloneOf(e), nil
		}
	}
	return nil, errs.NotFound(name)
}

func (f *fakeFederations) List(_ context.Context, parent string, _ int, _ string) (domain.FederationPage, error) {
	var out []*iampb.Federation
	for _, e := range f.items {
		if e.GetMetadata().GetParentId() == parent {
			out = append(out, proto.CloneOf(e))
		}
	}
	return domain.FederationPage{Items: out}, nil
}

func (f *fakeFederations) Create(_ context.Context, n *iampb.Federation) error {
	for _, e := range f.items {
		if e.GetMetadata().GetParentId() == n.GetMetadata().GetParentId() && e.GetMetadata().GetName() == n.GetMetadata().GetName() {
			return errs.AlreadyExists(e.GetMetadata().GetId(), "name taken")
		}
	}
	f.items = append(f.items, proto.CloneOf(n))
	return nil
}

func (f *fakeFederations) Update(_ context.Context, n *iampb.Federation) error {
	for i, e := range f.items {
		if e.GetMetadata().GetId() == n.GetMetadata().GetId() {
			f.items[i] = proto.CloneOf(n)
			return nil
		}
	}
	return errs.NotFound(n.GetMetadata().GetId())
}

func (f *fakeFederations) Delete(_ context.Context, id string) error {
	f.items = slices.DeleteFunc(f.items, func(e *iampb.Federation) bool { return e.GetMetadata().GetId() == id })
	return nil
}

// fakeFedCerts is a slice-backed domain.FederationCertificateRepository.
type fakeFedCerts struct {
	items []*iampb.FederationCertificate
}

func (f *fakeFedCerts) Get(_ context.Context, id string) (*iampb.FederationCertificate, error) {
	for _, e := range f.items {
		if e.GetMetadata().GetId() == id {
			return proto.CloneOf(e), nil
		}
	}
	return nil, errs.NotFound(id)
}

func (f *fakeFedCerts) ListAll(_ context.Context, fed string) ([]*iampb.FederationCertificate, error) {
	var out []*iampb.FederationCertificate
	for _, e := range f.items {
		if e.GetMetadata().GetParentId() == fed {
			out = append(out, proto.CloneOf(e))
		}
	}
	return out, nil
}

func (f *fakeFedCerts) List(ctx context.Context, fed string, _ int, _ string) (domain.FederationCertificatePage, error) {
	items, err := f.ListAll(ctx, fed)
	return domain.FederationCertificatePage{Items: items}, err
}

func (f *fakeFedCerts) Create(_ context.Context, c *iampb.FederationCertificate) error {
	f.items = append(f.items, proto.CloneOf(c))
	return nil
}

func (f *fakeFedCerts) Update(_ context.Context, c *iampb.FederationCertificate) error {
	for i, e := range f.items {
		if e.GetMetadata().GetId() == c.GetMetadata().GetId() {
			f.items[i] = proto.CloneOf(c)
			return nil
		}
	}
	return errs.NotFound(c.GetMetadata().GetId())
}

func (f *fakeFedCerts) Delete(_ context.Context, id string) error {
	f.items = slices.DeleteFunc(f.items, func(e *iampb.FederationCertificate) bool { return e.GetMetadata().GetId() == id })
	return nil
}

func (f *fakeFedCerts) DeleteByFederation(_ context.Context, fed string) error {
	f.items = slices.DeleteFunc(f.items, func(e *iampb.FederationCertificate) bool { return e.GetMetadata().GetParentId() == fed })
	return nil
}

func (f *fakeFedCerts) ReplaceAll(_ context.Context, _ string, remove []string, update, create []*iampb.FederationCertificate) error {
	f.items = slices.DeleteFunc(f.items, func(e *iampb.FederationCertificate) bool { return slices.Contains(remove, e.GetMetadata().GetId()) })
	for _, u := range update {
		for i, e := range f.items {
			if e.GetMetadata().GetId() == u.GetMetadata().GetId() {
				f.items[i] = proto.CloneOf(u)
			}
		}
	}
	for _, c := range create {
		f.items = append(f.items, proto.CloneOf(c))
	}
	return nil
}

type fedTenants struct{}

func (fedTenants) Get(_ context.Context, id string) (*iampb.Container, error) {
	if id != tenantID {
		return nil, errs.NotFound(id)
	}
	return &iampb.Container{Metadata: &commonpb.ResourceMetadata{Id: id}}, nil
}
func (fedTenants) Ensure(context.Context, *iampb.Container) error { return nil }
func (fedTenants) GetByName(_ context.Context, name string) (*iampb.Container, error) {
	return nil, errs.NotFound(name)
}

func (fedTenants) List(context.Context, int, string) (domain.ContainerPage, error) {
	return domain.ContainerPage{}, nil
}

type fixedUsers int

func (u fixedUsers) CountByFederation(context.Context, string) (int, error) { return int(u), nil }

type fedFixture struct {
	feds  *domain.FederationService
	certs *domain.FederationCertificateService
	fr    *fakeFederations
	cr    *fakeFedCerts
	clk   *clock.Manual
}

func newFedFixture(t *testing.T) *fedFixture {
	t.Helper()
	f := &fedFixture{fr: &fakeFederations{}, cr: &fakeFedCerts{}, clk: clock.NewManual(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))}
	ids := &seqIDs{}
	var err error
	f.feds, err = domain.NewFederationService(domain.FederationDeps{
		Federations: f.fr, Certificates: f.cr, Tenants: fedTenants{}, Users: fixedUsers(3), Ops: inlineOps{}, IDs: ids, Clock: f.clk,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.certs, err = domain.NewFederationCertificateService(domain.FederationCertificateDeps{
		Certificates: f.cr, Federations: f.fr, Ops: inlineOps{}, IDs: ids, Clock: f.clk,
	})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func samlSpec(url string) *iampb.FederationSpec {
	return &iampb.FederationSpec{Settings: &iampb.FederationSpec_SamlSettings{
		SamlSettings: &iampb.SamlSettings{IdpIssuer: "https://idp.example.com", SsoUrl: url},
	}}
}

func (f *fedFixture) create(t *testing.T, name string) *iampb.Federation {
	t.Helper()
	op, err := f.feds.Create(t.Context(), domain.CreateFederationCmd{
		Metadata: &commonpb.ResourceMetadata{ParentId: tenantID, Name: name}, Spec: samlSpec("https://idp.example.com/sso"),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := f.feds.Get(t.Context(), op.ResourceID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func (f *fedFixture) addCert(t *testing.T, fedID, name, data string) *iampb.FederationCertificate {
	t.Helper()
	op, err := f.certs.Create(t.Context(), domain.CreateFederationCertificateCmd{
		Metadata: &commonpb.ResourceMetadata{ParentId: fedID, Name: name}, Spec: &iampb.FederationCertificateSpec{Data: data},
	})
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	c, err := f.certs.Get(t.Context(), op.ResourceID)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// testCert returns a PEM self-signed certificate valid in [notBefore, notAfter].
func testCert(t *testing.T, key, pub any, notBefore, notAfter time.Time) (pemData string, der []byte) {
	t.Helper()
	serial, err := rand.Int(rand.Reader, big.NewInt(1<<60))
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "idp"}, NotBefore: notBefore, NotAfter: notAfter}
	der, err = x509.CreateCertificate(rand.Reader, tpl, tpl, pub, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), der
}

func rsaCert(t *testing.T, notAfter time.Time) (pemData string, der []byte) {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return testCert(t, k, &k.PublicKey, time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC), notAfter)
}

var pastTime = time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

var farFuture = time.Date(2040, 1, 1, 0, 0, 0, 0, time.UTC)

func TestFederationCreateAndStatus(t *testing.T) {
	t.Parallel()
	f := newFedFixture(t)
	fed := f.create(t, "okta")
	if fed.GetStatus().GetState() != iampb.FederationStatus_ACTIVE || fed.GetStatus().GetUsersCount() != 3 || fed.GetStatus().GetCertificatesCount() != 0 {
		t.Fatalf("status = %v", fed.GetStatus())
	}
	data, _ := rsaCert(t, farFuture)
	f.addCert(t, fed.GetMetadata().GetId(), "", data)
	got, err := f.feds.GetByName(t.Context(), tenantID, "okta")
	if err != nil || got.GetStatus().GetCertificatesCount() != 1 {
		t.Fatalf("GetByName = %v, %v", got.GetStatus(), err)
	}
	page, err := f.feds.List(t.Context(), tenantID, 0, "", "")
	if err != nil || len(page.Items) != 1 || page.Items[0].GetStatus().GetCertificatesCount() != 1 {
		t.Fatalf("List = %v, %v", page, err)
	}
	if _, err := f.feds.List(t.Context(), tenantID, 0, "", "x"); !hasViolation(err, "filter") {
		t.Fatalf("filter err = %v", err)
	}
}

func TestFederationCreateValidation(t *testing.T) {
	t.Parallel()
	f := newFedFixture(t)
	f.create(t, "taken")
	noURL := samlSpec("")
	noIssuer := samlSpec("https://x.example.com")
	noIssuer.GetSamlSettings().IdpIssuer = ""
	for name, tt := range map[string]struct {
		parent, fedName string
		spec            *iampb.FederationSpec
		want            errs.Kind
		field           string
	}{
		"no saml":        {tenantID, "a", &iampb.FederationSpec{}, errs.KindBadRequest, "spec.saml_settings"},
		"nil spec":       {tenantID, "a", nil, errs.KindBadRequest, "spec.saml_settings"},
		"no sso url":     {tenantID, "a", noURL, errs.KindBadRequest, "spec.saml_settings.sso_url"},
		"relative url":   {tenantID, "a", samlSpec("/sso"), errs.KindBadRequest, "spec.saml_settings.sso_url"},
		"ftp url":        {tenantID, "a", samlSpec("ftp://idp.example.com/sso"), errs.KindBadRequest, "spec.saml_settings.sso_url"},
		"no issuer":      {tenantID, "a", noIssuer, errs.KindBadRequest, "spec.saml_settings.idp_issuer"},
		"no name":        {tenantID, "", samlSpec("https://x.example.com"), errs.KindBadRequest, "metadata.name"},
		"no parent":      {"", "a", samlSpec("https://x.example.com"), errs.KindBadRequest, "metadata.parent_id"},
		"unknown parent": {"tenant-e00nope", "a", samlSpec("https://x.example.com"), errs.KindNotFound, ""},
		"duplicate name": {tenantID, "taken", samlSpec("https://x.example.com"), errs.KindAlreadyExists, ""},
	} {
		_, err := f.feds.Create(t.Context(), domain.CreateFederationCmd{
			Metadata: &commonpb.ResourceMetadata{ParentId: tt.parent, Name: tt.fedName}, Spec: tt.spec,
		})
		if got := errs.KindOf(err); got != tt.want {
			t.Errorf("%s: kind %v, want %v (%v)", name, got, tt.want, err)
			continue
		}
		if tt.field != "" && !hasViolation(err, tt.field) {
			t.Errorf("%s: error %v does not name %s", name, err, tt.field)
		}
	}
	if len(f.fr.items) != 1 {
		t.Fatalf("rejected creates stored %d federations", len(f.fr.items)-1)
	}
}

func TestFederationUpdate(t *testing.T) {
	t.Parallel()
	f := newFedFixture(t)
	fed := f.create(t, "okta")
	id := fed.GetMetadata().GetId()
	spec := samlSpec("https://new.example.com/sso")
	spec.UserAccountAutoCreation = true
	spec.Active = true //nolint:staticcheck // deprecated spec.active must still be stored and echoed
	spec.GetSamlSettings().ForceAuthn = true
	_, err := f.feds.Update(t.Context(), domain.UpdateFederationCmd{
		Metadata: &commonpb.ResourceMetadata{Id: id, Name: "okta", Labels: map[string]string{"a": "b"}}, Spec: spec,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := f.feds.Get(t.Context(), id)
	if got.GetSpec().GetSamlSettings().GetSsoUrl() != "https://new.example.com/sso" || !got.GetSpec().GetUserAccountAutoCreation() ||
		!got.GetSpec().GetActive() || //nolint:staticcheck // deprecated spec.active must be echoed
		!got.GetSpec().GetSamlSettings().GetForceAuthn() || got.GetMetadata().GetLabels()["a"] != "b" {
		t.Fatalf("after update: %v", got)
	}
	if got.GetMetadata().GetResourceVersion() != 2 || got.GetStatus().GetState() != iampb.FederationStatus_ACTIVE {
		t.Fatalf("version/state = %d/%v", got.GetMetadata().GetResourceVersion(), got.GetStatus().GetState())
	}

	for name, cmd := range map[string]domain.UpdateFederationCmd{
		"stale version": {Metadata: &commonpb.ResourceMetadata{Id: id, Name: "okta", ResourceVersion: 1}, Spec: spec},
		"parent change": {Metadata: &commonpb.ResourceMetadata{Id: id, Name: "okta", ParentId: "tenant-e00other"}, Spec: spec},
		"bad url":       {Metadata: &commonpb.ResourceMetadata{Id: id, Name: "okta"}, Spec: samlSpec("nope")},
		"no id":         {Metadata: &commonpb.ResourceMetadata{Name: "okta"}, Spec: spec},
		"unknown":       {Metadata: &commonpb.ResourceMetadata{Id: "federation-e00nope", Name: "x"}, Spec: spec},
	} {
		if _, err := f.feds.Update(t.Context(), cmd); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestFederationStateMachineAndDelete(t *testing.T) {
	t.Parallel()
	f := newFedFixture(t)
	fed := f.create(t, "okta")
	id := fed.GetMetadata().GetId()
	data, _ := rsaCert(t, farFuture)
	f.addCert(t, id, "", data)

	if _, err := f.feds.Delete(t.Context(), id, nil); !errs.IsKind(err, errs.KindBadState) {
		t.Fatalf("delete active: %v, want BadState", err)
	}
	if _, err := f.feds.Deactivate(t.Context(), id, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.feds.Get(t.Context(), id); got.GetStatus().GetState() != iampb.FederationStatus_INACTIVE {
		t.Fatalf("state = %v", got.GetStatus().GetState())
	}
	if _, err := f.feds.Activate(t.Context(), id, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.feds.Get(t.Context(), id); got.GetStatus().GetState() != iampb.FederationStatus_ACTIVE {
		t.Fatalf("state = %v", got.GetStatus().GetState())
	}
	if _, err := f.feds.Deactivate(t.Context(), id, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := f.feds.Delete(t.Context(), id, nil); err != nil {
		t.Fatalf("delete inactive: %v", err)
	}
	if len(f.fr.items) != 0 || len(f.cr.items) != 0 {
		t.Fatalf("left behind %d federations, %d certificates", len(f.fr.items), len(f.cr.items))
	}
	if _, err := f.feds.Delete(t.Context(), id, nil); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("delete missing: %v", err)
	}
	if _, err := f.feds.Activate(t.Context(), id, nil); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("activate missing: %v", err)
	}
}

func TestFederationCertificateStatus(t *testing.T) {
	t.Parallel()
	f := newFedFixture(t)
	fedID := f.create(t, "okta").GetMetadata().GetId()

	data, der := rsaCert(t, time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC))
	c := f.addCert(t, fedID, "main", data)
	sum := sha256.Sum256(der)
	st := c.GetStatus()
	if st.GetFingerprint() != hex.EncodeToString(sum[:]) || st.GetAlgorithm() != "RSA" || st.GetKeySize() != 2048 ||
		st.GetState() != iampb.FederationCertificateStatus_ACTIVE || st.GetNotAfter().AsTime().Year() != 2026 {
		t.Fatalf("status = %v", st)
	}
	// Expiry is computed on read against the mocker clock.
	f.clk.Advance(48 * time.Hour)
	got, err := f.certs.Get(t.Context(), c.GetMetadata().GetId())
	if err != nil || got.GetStatus().GetState() != iampb.FederationCertificateStatus_EXPIRED {
		t.Fatalf("after expiry = %v, %v", got.GetStatus().GetState(), err)
	}
	page, _ := f.certs.ListByFederation(t.Context(), fedID, 0, "")
	if page.Items[0].GetStatus().GetState() != iampb.FederationCertificateStatus_EXPIRED {
		t.Fatal("List does not apply expiry")
	}

	ek, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	ecData, _ := testCert(t, ek, &ek.PublicKey, pastTime, farFuture)
	if got := f.addCert(t, fedID, "ec", ecData).GetStatus(); got.GetAlgorithm() != "ECDSA" || got.GetKeySize() != 384 {
		t.Fatalf("ec status = %v", got)
	}
	epub, epriv, _ := ed25519.GenerateKey(rand.Reader)
	edData, _ := testCert(t, epriv, epub, pastTime, farFuture)
	if got := f.addCert(t, fedID, "ed", edData).GetStatus(); got.GetAlgorithm() != "ED25519" || got.GetKeySize() != 256 {
		t.Fatalf("ed status = %v", got)
	}
}

func TestFederationCertificateCreateValidation(t *testing.T) {
	t.Parallel()
	f := newFedFixture(t)
	fedID := f.create(t, "okta").GetMetadata().GetId()
	good, _ := rsaCert(t, farFuture)
	other, _ := rsaCert(t, farFuture)
	badBlock := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("junk")}))
	pubKey := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: []byte("x")}))
	for name, tt := range map[string]struct {
		parent, data string
		want         errs.Kind
		field        string
	}{
		"empty":          {fedID, "", errs.KindBadRequest, "spec.data"},
		"not pem":        {fedID, "hello", errs.KindBadRequest, "spec.data"},
		"wrong type":     {fedID, pubKey, errs.KindBadRequest, "spec.data"},
		"junk der":       {fedID, badBlock, errs.KindBadRequest, "spec.data"},
		"two certs":      {fedID, good + other, errs.KindBadRequest, "spec.data"},
		"no parent":      {"", good, errs.KindBadRequest, "metadata.parent_id"},
		"unknown parent": {"federation-e00nope", good, errs.KindNotFound, ""},
	} {
		_, err := f.certs.Create(t.Context(), domain.CreateFederationCertificateCmd{
			Metadata: &commonpb.ResourceMetadata{ParentId: tt.parent}, Spec: &iampb.FederationCertificateSpec{Data: tt.data},
		})
		if got := errs.KindOf(err); got != tt.want {
			t.Errorf("%s: kind %v, want %v (%v)", name, got, tt.want, err)
			continue
		}
		if tt.field != "" && !hasViolation(err, tt.field) {
			t.Errorf("%s: error %v does not name %s", name, err, tt.field)
		}
	}
	if len(f.cr.items) != 0 {
		t.Fatalf("rejected creates stored %d certificates", len(f.cr.items))
	}
	if _, err := f.certs.ListByFederation(t.Context(), "federation-e00nope", 0, ""); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("list unknown federation: %v", err)
	}
}

func TestFederationCertificateUpdateAndDelete(t *testing.T) {
	t.Parallel()
	f := newFedFixture(t)
	fedID := f.create(t, "okta").GetMetadata().GetId()
	data, _ := rsaCert(t, farFuture)
	other, _ := rsaCert(t, farFuture)
	c := f.addCert(t, fedID, "", data)
	id := c.GetMetadata().GetId()

	_, err := f.certs.Update(t.Context(), domain.UpdateFederationCertificateCmd{
		Metadata: &commonpb.ResourceMetadata{Id: id, Name: "renamed", Labels: map[string]string{"k": "v"}},
		Spec:     &iampb.FederationCertificateSpec{Description: "primary", Data: data},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := f.certs.Get(t.Context(), id)
	if got.GetMetadata().GetName() != "renamed" || got.GetSpec().GetDescription() != "primary" || got.GetSpec().GetData() != data ||
		got.GetMetadata().GetResourceVersion() != 2 || got.GetStatus().GetFingerprint() != c.GetStatus().GetFingerprint() {
		t.Fatalf("after update = %v", got)
	}
	_, err = f.certs.Update(t.Context(), domain.UpdateFederationCertificateCmd{
		Metadata: &commonpb.ResourceMetadata{Id: id}, Spec: &iampb.FederationCertificateSpec{Data: other},
	}, nil)
	if !hasViolation(err, "spec.data") || errs.KindOf(err) != errs.KindBadRequest {
		t.Fatalf("data change err = %v", err)
	}
	if _, err := f.certs.Update(t.Context(), domain.UpdateFederationCertificateCmd{Metadata: &commonpb.ResourceMetadata{Id: "x-e00nope"}, Spec: &iampb.FederationCertificateSpec{}}, nil); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("update missing: %v", err)
	}
	if _, err := f.certs.Delete(t.Context(), id, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := f.certs.Delete(t.Context(), id, nil); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("delete missing: %v", err)
	}
}

func TestFederationCertificateUpdateBulk(t *testing.T) {
	t.Parallel()
	f := newFedFixture(t)
	fedID := f.create(t, "okta").GetMetadata().GetId()
	otherFed := f.create(t, "other").GetMetadata().GetId()
	d1, _ := rsaCert(t, farFuture)
	d2, _ := rsaCert(t, farFuture)
	d3, _ := rsaCert(t, farFuture)
	c1 := f.addCert(t, fedID, "one", d1)
	c2 := f.addCert(t, fedID, "two", d2)
	foreign := f.addCert(t, otherFed, "", d3)

	ids := func() []string {
		var out []string
		for _, c := range f.cr.items {
			if c.GetMetadata().GetParentId() == fedID {
				out = append(out, c.GetMetadata().GetId())
			}
		}
		return out
	}
	before := ids()

	// Invalid element: nothing may change.
	bad := []domain.UpdateFederationCertificateCmd{
		{Metadata: &commonpb.ResourceMetadata{Id: c1.GetMetadata().GetId(), Name: "changed"}, Spec: &iampb.FederationCertificateSpec{}},
		{Metadata: &commonpb.ResourceMetadata{}, Spec: &iampb.FederationCertificateSpec{Data: "garbage"}},
	}
	if _, err := f.certs.UpdateBulk(t.Context(), fedID, bad, nil); !hasViolation(err, "updates[1].spec.data") {
		t.Fatalf("bad bulk err = %v", err)
	}
	if !slices.Equal(ids(), before) {
		t.Fatal("rejected bulk changed certificates")
	}
	if got, _ := f.certs.Get(t.Context(), c1.GetMetadata().GetId()); got.GetMetadata().GetName() != "one" {
		t.Fatal("rejected bulk renamed a certificate")
	}
	for name, ups := range map[string][]domain.UpdateFederationCertificateCmd{
		"foreign id":   {{Metadata: &commonpb.ResourceMetadata{Id: foreign.GetMetadata().GetId()}, Spec: &iampb.FederationCertificateSpec{}}},
		"id twice":     {{Metadata: &commonpb.ResourceMetadata{Id: c1.GetMetadata().GetId()}, Spec: &iampb.FederationCertificateSpec{}}, {Metadata: &commonpb.ResourceMetadata{Id: c1.GetMetadata().GetId()}, Spec: &iampb.FederationCertificateSpec{}}},
		"name twice":   {{Metadata: &commonpb.ResourceMetadata{Id: c1.GetMetadata().GetId(), Name: "n"}, Spec: &iampb.FederationCertificateSpec{}}, {Metadata: &commonpb.ResourceMetadata{Id: c2.GetMetadata().GetId(), Name: "n"}, Spec: &iampb.FederationCertificateSpec{}}},
		"data change":  {{Metadata: &commonpb.ResourceMetadata{Id: c1.GetMetadata().GetId()}, Spec: &iampb.FederationCertificateSpec{Data: d3}}},
		"wrong parent": {{Metadata: &commonpb.ResourceMetadata{ParentId: otherFed}, Spec: &iampb.FederationCertificateSpec{Data: d3}}},
	} {
		if _, err := f.certs.UpdateBulk(t.Context(), fedID, ups, nil); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
	if _, err := f.certs.UpdateBulk(t.Context(), "federation-e00nope", nil, nil); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("unknown federation: %v", err)
	}
	if !slices.Equal(ids(), before) {
		t.Fatal("rejected bulk changed certificates")
	}

	// Valid: update c1's description, create a new one, c2 is removed.
	_, err := f.certs.UpdateBulk(t.Context(), fedID, []domain.UpdateFederationCertificateCmd{
		{Metadata: &commonpb.ResourceMetadata{Id: c1.GetMetadata().GetId(), Name: "one"}, Spec: &iampb.FederationCertificateSpec{Description: "kept", Data: d1}},
		{Metadata: &commonpb.ResourceMetadata{Name: "fresh"}, Spec: &iampb.FederationCertificateSpec{Data: d3}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	page, _ := f.certs.ListByFederation(t.Context(), fedID, 0, "")
	if len(page.Items) != 2 {
		t.Fatalf("after bulk: %d certificates", len(page.Items))
	}
	if _, err := f.certs.Get(t.Context(), c2.GetMetadata().GetId()); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("c2 survived: %v", err)
	}
	if got, _ := f.certs.Get(t.Context(), c1.GetMetadata().GetId()); got.GetSpec().GetDescription() != "kept" {
		t.Fatalf("c1 = %v", got)
	}
	if got, _ := f.certs.Get(t.Context(), foreign.GetMetadata().GetId()); got == nil {
		t.Fatal("foreign certificate was touched")
	}
	// An empty update removes everything.
	if _, err := f.certs.UpdateBulk(t.Context(), fedID, nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(ids()) != 0 {
		t.Fatalf("empty bulk left %v", ids())
	}
}
