package contract

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/nebius/gosdk/proto/fieldmask/grpcheader"
	"github.com/nebius/gosdk/proto/fieldmask/mask"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"
	iamsvc "github.com/nebius/gosdk/services/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/iam"
)

type fedEnv struct {
	inst  *testkit.Instance
	feds  iamsvc.FederationService
	certs iamsvc.FederationCertificateService
}

func fedStart(t *testing.T, opts ...testkit.Option) *fedEnv {
	t.Helper()
	inst := testkit.StartT(t, []mocker.Module{iam.New()}, opts...)
	sdk := inst.SDK(t)
	return &fedEnv{inst: inst, feds: iamsvc.NewFederationService(sdk), certs: iamsvc.NewFederationCertificateService(sdk)}
}

// fedCertPEM returns a self-signed P-256 certificate valid until notAfter, and its DER.
func fedCertPEM(t *testing.T, notAfter time.Time) (pemData string, der []byte) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, big.NewInt(1<<60))
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: "idp"},
		NotBefore: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC), NotAfter: notAfter,
	}
	der, err = x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), der
}

func fedSpec(ssoURL string) *iampb.FederationSpec {
	return &iampb.FederationSpec{Settings: &iampb.FederationSpec_SamlSettings{
		SamlSettings: &iampb.SamlSettings{IdpIssuer: "https://idp.example.com", SsoUrl: ssoURL},
	}}
}

func (e *fedEnv) createFed(t *testing.T, svc iamsvc.FederationService, name string) string {
	t.Helper()
	op, err := svc.Create(t.Context(), &iampb.CreateFederationRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: e.inst.TenantID, Name: name}, Spec: fedSpec("https://idp.example.com/sso"),
	})
	if err != nil {
		t.Fatalf("Create federation %s: %v", name, err)
	}
	if _, err := op.Wait(t.Context()); err != nil {
		t.Fatalf("Create federation wait: %v", err)
	}
	return op.ResourceID()
}

func (e *fedEnv) createCert(t *testing.T, svc iamsvc.FederationCertificateService, fedID, name, data string) string {
	t.Helper()
	op, err := svc.Create(t.Context(), &iampb.CreateFederationCertificateRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: fedID, Name: name}, Spec: &iampb.FederationCertificateSpec{Data: data},
	})
	if err != nil {
		t.Fatalf("Create certificate: %v", err)
	}
	if _, err := op.Wait(t.Context()); err != nil {
		t.Fatalf("Create certificate wait: %v", err)
	}
	return op.ResourceID()
}

func (e *fedEnv) fedState(t *testing.T, id string) *iampb.FederationStatus {
	t.Helper()
	f, err := e.feds.Get(t.Context(), &iampb.GetFederationRequest{Id: id})
	if err != nil {
		t.Fatalf("Get federation: %v", err)
	}
	return f.GetStatus()
}

func TestFederationLifecycle(t *testing.T) {
	t.Parallel()
	env := fedStart(t)
	ctx := t.Context()
	tenant := env.inst.TenantID

	op, err := env.feds.Create(ctx, &iampb.CreateFederationRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: tenant, Name: "okta", Labels: map[string]string{"a": "b"}},
		Spec: &iampb.FederationSpec{
			UserAccountAutoCreation: true,
			Settings: &iampb.FederationSpec_SamlSettings{SamlSettings: &iampb.SamlSettings{
				IdpIssuer: "https://idp.example.com", SsoUrl: "https://idp.example.com/sso", ForceAuthn: true,
			}},
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	id := op.ResourceID()
	if len(id) < len("federation-") || id[:len("federation-")] != "federation-" {
		t.Fatalf("id %q lacks the federation- prefix", id)
	}
	got, err := env.feds.Get(ctx, &iampb.GetFederationRequest{Id: id})
	if err != nil {
		t.Fatal(err)
	}
	saml := got.GetSpec().GetSamlSettings()
	if saml.GetIdpIssuer() != "https://idp.example.com" || saml.GetSsoUrl() != "https://idp.example.com/sso" || !saml.GetForceAuthn() ||
		!got.GetSpec().GetUserAccountAutoCreation() || got.GetMetadata().GetLabels()["a"] != "b" || got.GetMetadata().GetParentId() != tenant {
		t.Errorf("round trip = %v", got)
	}
	if got.GetStatus().GetState() != iampb.FederationStatus_ACTIVE {
		t.Errorf("state = %v, want ACTIVE (spec.active is ignored)", got.GetStatus().GetState())
	}

	byName, err := env.feds.GetByName(ctx, &commonpb.GetByNameRequest{ParentId: tenant, Name: "okta"})
	if err != nil || byName.GetMetadata().GetId() != id {
		t.Fatalf("GetByName = %v, %v", byName, err)
	}
	second := env.createFed(t, env.feds, "azure")
	page, err := env.feds.List(ctx, &iampb.ListFederationsRequest{ParentId: tenant, PageSize: 1})
	if err != nil || len(page.GetItems()) != 1 || page.GetNextPageToken() == "" || page.GetItems()[0].GetMetadata().GetId() != id {
		t.Fatalf("List page 1 = %v, %v", page, err)
	}
	page, err = env.feds.List(ctx, &iampb.ListFederationsRequest{ParentId: tenant, PageSize: 1, PageToken: page.GetNextPageToken()})
	if err != nil || len(page.GetItems()) != 1 || page.GetItems()[0].GetMetadata().GetId() != second || page.GetNextPageToken() != "" {
		t.Fatalf("List page 2 = %v, %v", page, err)
	}

	// Update in place; the status counts are computed.
	upd, err := env.feds.Update(ctx, &iampb.UpdateFederationRequest{
		Metadata: &commonpb.ResourceMetadata{Id: id, Name: "okta"},
		Spec:     fedSpec("https://idp.example.com/sso2"),
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if _, err := upd.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ = env.feds.Get(ctx, &iampb.GetFederationRequest{Id: id})
	if got.GetSpec().GetSamlSettings().GetSsoUrl() != "https://idp.example.com/sso2" || got.GetMetadata().GetResourceVersion() != 2 {
		t.Errorf("after Update = %v", got)
	}
	// X-ResetMask clears the labels and the flags the request leaves out.
	rctx, err := grpcheader.AddResetMaskToOutgoingContext(ctx, mask.ParseMust("metadata.labels,spec.user_account_auto_creation"))
	if err != nil {
		t.Fatal(err)
	}
	upd, err = env.feds.Update(rctx, &iampb.UpdateFederationRequest{Metadata: &commonpb.ResourceMetadata{Id: id, Name: "okta"}, Spec: fedSpec("https://idp.example.com/sso2")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := upd.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ = env.feds.Get(ctx, &iampb.GetFederationRequest{Id: id})
	if len(got.GetMetadata().GetLabels()) != 0 || got.GetSpec().GetUserAccountAutoCreation() {
		t.Errorf("reset mask did not clear: %v", got)
	}

	// Certificates are counted.
	data, _ := fedCertPEM(t, time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC))
	env.createCert(t, env.certs, id, "", data)
	if n := env.fedState(t, id).GetCertificatesCount(); n != 1 {
		t.Errorf("certificates_count = %d, want 1", n)
	}

	// Deactivate / Activate.
	for _, step := range []struct {
		name string
		call func() error
		want iampb.FederationStatus_State
	}{
		{"Deactivate", func() error {
			op, err := env.feds.Deactivate(ctx, &iampb.DeactivateFederationRequest{FederationId: id})
			if err == nil {
				_, err = op.Wait(ctx)
			}
			return err
		}, iampb.FederationStatus_INACTIVE},
		{"Activate", func() error {
			op, err := env.feds.Activate(ctx, &iampb.ActivateFederationRequest{FederationId: id})
			if err == nil {
				_, err = op.Wait(ctx)
			}
			return err
		}, iampb.FederationStatus_ACTIVE},
	} {
		if err := step.call(); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		if s := env.fedState(t, id).GetState(); s != step.want {
			t.Errorf("after %s state = %v, want %v", step.name, s, step.want)
		}
	}

	// An active federation cannot be deleted.
	_, err = env.feds.Delete(ctx, &iampb.DeleteFederationRequest{Id: id})
	assertCode(t, err, codes.FailedPrecondition)
	dop, err := env.feds.Deactivate(ctx, &iampb.DeactivateFederationRequest{FederationId: id})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dop.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	certs, err := env.certs.ListByFederation(ctx, &iampb.ListFederationCertificateByFederationRequest{FederationId: id})
	if err != nil || len(certs.GetItems()) != 1 {
		t.Fatalf("ListByFederation = %v, %v", certs, err)
	}
	del, err := env.feds.Delete(ctx, &iampb.DeleteFederationRequest{Id: id})
	if err != nil {
		t.Fatalf("Delete inactive: %v", err)
	}
	if _, err := del.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	_, err = env.feds.Get(ctx, &iampb.GetFederationRequest{Id: id})
	assertCode(t, err, codes.NotFound)
	_, err = env.certs.Get(ctx, &iampb.GetFederationCertificateRequest{Id: certs.GetItems()[0].GetMetadata().GetId()})
	assertCode(t, err, codes.NotFound)
}

func TestFederationErrors(t *testing.T) {
	t.Parallel()
	env := fedStart(t)
	ctx := t.Context()
	tenant := env.inst.TenantID
	env.createFed(t, env.feds, "taken")

	create := func(parent, name string, spec *iampb.FederationSpec) error {
		_, err := env.feds.Create(ctx, &iampb.CreateFederationRequest{Metadata: &commonpb.ResourceMetadata{ParentId: parent, Name: name}, Spec: spec})
		return err
	}
	assertCode(t, create(tenant, "taken", fedSpec("https://x.example.com")), codes.AlreadyExists)
	assertCode(t, create("tenant-e00mock0000000nobody", "n", fedSpec("https://x.example.com")), codes.NotFound)
	for name, tc := range map[string]struct {
		spec  *iampb.FederationSpec
		field string
	}{
		// The proto's required oneof is enforced before the service sees the call.
		"no saml":    {&iampb.FederationSpec{}, "spec.settings"},
		"relative":   {fedSpec("/sso"), "spec.saml_settings.sso_url"},
		"no sso url": {fedSpec(""), "spec.saml_settings.sso_url"},
		"ftp":        {fedSpec("ftp://x.example.com/sso"), "spec.saml_settings.sso_url"},
	} {
		authKeyWantInvalid(t, name, create(tenant, "bad", tc.spec), tc.field)
	}
	authKeyWantInvalid(t, "no name", create(tenant, "", fedSpec("https://x.example.com")), "metadata.name")

	_, err := env.feds.Get(ctx, &iampb.GetFederationRequest{Id: "federation-e00mock0000000nobody"})
	assertCode(t, err, codes.NotFound)
	_, err = env.feds.GetByName(ctx, &commonpb.GetByNameRequest{ParentId: tenant, Name: "nope"})
	assertCode(t, err, codes.NotFound)
	_, err = env.feds.List(ctx, &iampb.ListFederationsRequest{ParentId: tenant, Filter: "name = 'x'"})
	authKeyWantInvalid(t, "filter", err, "filter")
	_, err = env.feds.Activate(ctx, &iampb.ActivateFederationRequest{FederationId: "federation-e00mock0000000nobody"})
	assertCode(t, err, codes.NotFound)
	_, err = env.feds.Delete(ctx, &iampb.DeleteFederationRequest{Id: "federation-e00mock0000000nobody"})
	assertCode(t, err, codes.NotFound)

	// Immutable parent and stale resource version on Update.
	id := env.createFed(t, env.feds, "upd")
	_, err = env.feds.Update(ctx, &iampb.UpdateFederationRequest{
		Metadata: &commonpb.ResourceMetadata{Id: id, Name: "upd", ParentId: env.inst.ProjectID}, Spec: fedSpec("https://x.example.com"),
	})
	authKeyWantInvalid(t, "parent change", err, "metadata.parent_id")
	_, err = env.feds.Update(ctx, &iampb.UpdateFederationRequest{
		Metadata: &commonpb.ResourceMetadata{Id: id, Name: "upd", ResourceVersion: 99}, Spec: fedSpec("https://x.example.com"),
	})
	if status.Code(err) != codes.Aborted && status.Code(err) != codes.FailedPrecondition {
		t.Errorf("stale resource_version: code %v (%v)", status.Code(err), err)
	}
}

func TestFederationCertificates(t *testing.T) {
	t.Parallel()
	clk := clock.NewManual(time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC))
	env := fedStart(t, testkit.WithClock(clk))
	ctx := t.Context()
	fedID := env.createFed(t, env.feds, "okta")

	notAfter := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	data, der := fedCertPEM(t, notAfter)
	id := env.createCert(t, env.certs, fedID, "primary", data)
	if len(id) < len("federationcertificate-") || id[:len("federationcertificate-")] != "federationcertificate-" {
		t.Fatalf("id %q lacks the federationcertificate- prefix", id)
	}
	got, err := env.certs.Get(ctx, &iampb.GetFederationCertificateRequest{Id: id})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(der)
	st := got.GetStatus()
	if st.GetFingerprint() != hex.EncodeToString(sum[:]) || st.GetAlgorithm() != "ECDSA" || st.GetKeySize() != 256 ||
		st.GetState() != iampb.FederationCertificateStatus_ACTIVE || !st.GetNotAfter().AsTime().Equal(notAfter) || st.GetNotBefore() == nil {
		t.Errorf("status = %v", st)
	}
	if got.GetSpec().GetData() != data || got.GetMetadata().GetName() != "primary" || got.GetMetadata().GetParentId() != fedID {
		t.Errorf("round trip = %v", got)
	}
	if n := env.fedState(t, fedID).GetCertificatesCount(); n != 1 {
		t.Errorf("certificates_count = %d", n)
	}

	// Update: name, labels and description change in place; data is immutable.
	upd, err := env.certs.Update(ctx, &iampb.UpdateFederationCertificateRequest{
		Metadata: &commonpb.ResourceMetadata{Id: id, Name: "renamed", Labels: map[string]string{"k": "v"}},
		Spec:     &iampb.FederationCertificateSpec{Description: "the cert", Data: data},
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if _, err := upd.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ = env.certs.Get(ctx, &iampb.GetFederationCertificateRequest{Id: id})
	if got.GetMetadata().GetName() != "renamed" || got.GetSpec().GetDescription() != "the cert" || got.GetMetadata().GetLabels()["k"] != "v" {
		t.Errorf("after Update = %v", got)
	}
	other, _ := fedCertPEM(t, notAfter)
	_, err = env.certs.Update(ctx, &iampb.UpdateFederationCertificateRequest{
		Metadata: &commonpb.ResourceMetadata{Id: id}, Spec: &iampb.FederationCertificateSpec{Data: other},
	})
	authKeyWantInvalid(t, "data change", err, "spec.data")

	// Expiry follows the mocker clock.
	clk.Advance(36 * time.Hour)
	got, _ = env.certs.Get(ctx, &iampb.GetFederationCertificateRequest{Id: id})
	if got.GetStatus().GetState() != iampb.FederationCertificateStatus_EXPIRED {
		t.Errorf("after not_after state = %v, want EXPIRED", got.GetStatus().GetState())
	}

	// Validation.
	pubPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: []byte("x")}))
	for name, d := range map[string]string{"empty": "", "garbage": "nope", "wrong block": pubPEM, "two": data + other} {
		_, err := env.certs.Create(ctx, &iampb.CreateFederationCertificateRequest{
			Metadata: &commonpb.ResourceMetadata{ParentId: fedID}, Spec: &iampb.FederationCertificateSpec{Data: d},
		})
		authKeyWantInvalid(t, name, err, "spec.data")
	}
	_, err = env.certs.Create(ctx, &iampb.CreateFederationCertificateRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: "federation-e00mock0000000nobody"}, Spec: &iampb.FederationCertificateSpec{Data: data},
	})
	assertCode(t, err, codes.NotFound)
	_, err = env.certs.Get(ctx, &iampb.GetFederationCertificateRequest{Id: "federationcertificate-e00mock0000000nobody"})
	assertCode(t, err, codes.NotFound)
	_, err = env.certs.ListByFederation(ctx, &iampb.ListFederationCertificateByFederationRequest{FederationId: "federation-e00mock0000000nobody"})
	assertCode(t, err, codes.NotFound)

	// Names are unique per federation when set; unnamed certificates may repeat.
	_, err = env.certs.Create(ctx, &iampb.CreateFederationCertificateRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: fedID, Name: "renamed"}, Spec: &iampb.FederationCertificateSpec{Data: other},
	})
	assertCode(t, err, codes.AlreadyExists)
	env.createCert(t, env.certs, fedID, "", other)
	env.createCert(t, env.certs, fedID, "", other)

	// Paging.
	page, err := env.certs.ListByFederation(ctx, &iampb.ListFederationCertificateByFederationRequest{FederationId: fedID, PageSize: 2})
	if err != nil || len(page.GetItems()) != 2 || page.GetNextPageToken() == "" {
		t.Fatalf("ListByFederation page 1 = %v, %v", page, err)
	}

	del, err := env.certs.Delete(ctx, &iampb.DeleteFederationCertificateRequest{Id: id})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := del.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	_, err = env.certs.Delete(ctx, &iampb.DeleteFederationCertificateRequest{Id: id})
	assertCode(t, err, codes.NotFound)
}

func TestFederationCertificateUpdateBulk(t *testing.T) {
	t.Parallel()
	env := fedStart(t)
	ctx := t.Context()
	fedID := env.createFed(t, env.feds, "okta")
	otherFed := env.createFed(t, env.feds, "other")
	far := time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
	d1, _ := fedCertPEM(t, far)
	d2, _ := fedCertPEM(t, far)
	d3, _ := fedCertPEM(t, far)
	c1 := env.createCert(t, env.certs, fedID, "one", d1)
	c2 := env.createCert(t, env.certs, fedID, "two", d2)
	foreign := env.createCert(t, env.certs, otherFed, "", d3)

	bulk := func(updates ...*iampb.UpdateFederationCertificateRequest) error {
		op, err := env.certs.UpdateBulk(ctx, &iampb.UpdateBulkFederationCertificateRequest{FederationId: fedID, Updates: updates})
		if err != nil {
			return err
		}
		if op.ResourceID() != fedID {
			t.Errorf("operation resource id = %s, want the federation %s", op.ResourceID(), fedID)
		}
		_, err = op.Wait(ctx)
		return err
	}
	list := func() []*iampb.FederationCertificate {
		t.Helper()
		page, err := env.certs.ListByFederation(ctx, &iampb.ListFederationCertificateByFederationRequest{FederationId: fedID})
		if err != nil {
			t.Fatal(err)
		}
		return page.GetItems()
	}

	// A bad element rejects the whole call.
	err := bulk(
		&iampb.UpdateFederationCertificateRequest{Metadata: &commonpb.ResourceMetadata{ParentId: fedID, Id: c1, Name: "changed"}, Spec: &iampb.FederationCertificateSpec{}},
		&iampb.UpdateFederationCertificateRequest{Metadata: &commonpb.ResourceMetadata{ParentId: fedID}, Spec: &iampb.FederationCertificateSpec{Data: "garbage"}},
	)
	authKeyWantInvalid(t, "bad element", err, "updates[1].spec.data")
	if err := bulk(&iampb.UpdateFederationCertificateRequest{Metadata: &commonpb.ResourceMetadata{ParentId: fedID, Id: foreign}, Spec: &iampb.FederationCertificateSpec{}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("foreign certificate: code %v (%v)", status.Code(err), err)
	}
	if items := list(); len(items) != 2 {
		t.Fatalf("rejected bulk left %d certificates", len(items))
	}
	if got, _ := env.certs.Get(ctx, &iampb.GetFederationCertificateRequest{Id: c1}); got.GetMetadata().GetName() != "one" {
		t.Errorf("rejected bulk renamed c1 to %q", got.GetMetadata().GetName())
	}

	// c1 updated, c2 omitted (removed), one created.
	err = bulk(
		&iampb.UpdateFederationCertificateRequest{Metadata: &commonpb.ResourceMetadata{ParentId: fedID, Id: c1, Name: "one"}, Spec: &iampb.FederationCertificateSpec{Description: "kept", Data: d1}},
		&iampb.UpdateFederationCertificateRequest{Metadata: &commonpb.ResourceMetadata{ParentId: fedID, Name: "fresh"}, Spec: &iampb.FederationCertificateSpec{Data: d3}},
	)
	if err != nil {
		t.Fatalf("UpdateBulk: %v", err)
	}
	items := list()
	if len(items) != 2 {
		t.Fatalf("after bulk: %d certificates, want 2", len(items))
	}
	if _, err := env.certs.Get(ctx, &iampb.GetFederationCertificateRequest{Id: c2}); status.Code(err) != codes.NotFound {
		t.Errorf("omitted certificate: code %v, want NotFound", status.Code(err))
	}
	if got, _ := env.certs.Get(ctx, &iampb.GetFederationCertificateRequest{Id: c1}); got.GetSpec().GetDescription() != "kept" {
		t.Errorf("c1 = %v", got)
	}
	if n := env.fedState(t, fedID).GetCertificatesCount(); n != 2 {
		t.Errorf("certificates_count = %d, want 2", n)
	}
	if _, err := env.certs.Get(ctx, &iampb.GetFederationCertificateRequest{Id: foreign}); err != nil {
		t.Errorf("another federation's certificate was touched: %v", err)
	}
	if err := bulk(); err != nil || len(list()) != 0 {
		t.Fatalf("empty bulk: %v, %d left", err, len(list()))
	}
}

func TestFederationAuthz(t *testing.T) {
	t.Parallel()
	env := fedStart(t, testkit.WithAuthz(authz.ModeEnforce))
	ctx := t.Context()
	tenant := env.inst.TenantID
	fedID := env.createFed(t, env.feds, "seed")
	data, _ := fedCertPEM(t, time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC))
	certID := env.createCert(t, env.certs, fedID, "", data)

	viewerSDK := env.inst.SDKAs(t, config.DefaultViewerToken)
	vf, vc := iamsvc.NewFederationService(viewerSDK), iamsvc.NewFederationCertificateService(viewerSDK)
	if _, err := vf.Get(ctx, &iampb.GetFederationRequest{Id: fedID}); err != nil {
		t.Errorf("viewer Get: %v", err)
	}
	if _, err := vf.GetByName(ctx, &commonpb.GetByNameRequest{ParentId: tenant, Name: "seed"}); err != nil {
		t.Errorf("viewer GetByName: %v", err)
	}
	if _, err := vf.List(ctx, &iampb.ListFederationsRequest{ParentId: tenant}); err != nil {
		t.Errorf("viewer List: %v", err)
	}
	if _, err := vc.Get(ctx, &iampb.GetFederationCertificateRequest{Id: certID}); err != nil {
		t.Errorf("viewer certificate Get: %v", err)
	}
	if _, err := vc.ListByFederation(ctx, &iampb.ListFederationCertificateByFederationRequest{FederationId: fedID}); err != nil {
		t.Errorf("viewer ListByFederation: %v", err)
	}
	denied := map[string]error{}
	_, denied["Create"] = vf.Create(ctx, &iampb.CreateFederationRequest{Metadata: &commonpb.ResourceMetadata{ParentId: tenant, Name: "v"}, Spec: fedSpec("https://x.example.com")})
	_, denied["Update"] = vf.Update(ctx, &iampb.UpdateFederationRequest{Metadata: &commonpb.ResourceMetadata{Id: fedID, Name: "x"}, Spec: fedSpec("https://x.example.com")})
	_, denied["Activate"] = vf.Activate(ctx, &iampb.ActivateFederationRequest{FederationId: fedID})
	_, denied["Deactivate"] = vf.Deactivate(ctx, &iampb.DeactivateFederationRequest{FederationId: fedID})
	_, denied["Delete"] = vf.Delete(ctx, &iampb.DeleteFederationRequest{Id: fedID})
	_, denied["cert Create"] = vc.Create(ctx, &iampb.CreateFederationCertificateRequest{Metadata: &commonpb.ResourceMetadata{ParentId: fedID}, Spec: &iampb.FederationCertificateSpec{Data: data}})
	_, denied["cert Update"] = vc.Update(ctx, &iampb.UpdateFederationCertificateRequest{Metadata: &commonpb.ResourceMetadata{Id: certID}, Spec: &iampb.FederationCertificateSpec{}})
	_, denied["cert UpdateBulk"] = vc.UpdateBulk(ctx, &iampb.UpdateBulkFederationCertificateRequest{FederationId: fedID})
	_, denied["cert Delete"] = vc.Delete(ctx, &iampb.DeleteFederationCertificateRequest{Id: certID})
	for name, err := range denied {
		if status.Code(err) != codes.PermissionDenied {
			t.Errorf("viewer %s: code %v, want PermissionDenied (%v)", name, status.Code(err), err)
		}
	}

	editorSDK := env.inst.SDKAs(t, config.DefaultEditorToken)
	ef, ec := iamsvc.NewFederationService(editorSDK), iamsvc.NewFederationCertificateService(editorSDK)
	id := env.createFed(t, ef, "by-editor")
	cid := env.createCert(t, ec, id, "", data)
	op, err := ec.UpdateBulk(ctx, &iampb.UpdateBulkFederationCertificateRequest{FederationId: id})
	if err != nil {
		t.Fatalf("editor UpdateBulk: %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := ec.Get(ctx, &iampb.GetFederationCertificateRequest{Id: cid}); status.Code(err) != codes.NotFound {
		t.Errorf("bulk-removed certificate: %v", err)
	}
	dop, err := ef.Deactivate(ctx, &iampb.DeactivateFederationRequest{FederationId: id})
	if err != nil {
		t.Fatalf("editor Deactivate: %v", err)
	}
	if _, err := dop.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	del, err := ef.Delete(ctx, &iampb.DeleteFederationRequest{Id: id})
	if err != nil {
		t.Fatalf("editor Delete: %v", err)
	}
	if _, err := del.Wait(ctx); err != nil {
		t.Fatal(err)
	}

	anon := iamsvc.NewFederationService(env.inst.SDKAs(t, ""))
	_, err = anon.Get(ctx, &iampb.GetFederationRequest{Id: fedID})
	assertCode(t, err, codes.Unauthenticated)
}
