//go:build e2e

package iam

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"strconv"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"
	iamsvc "github.com/nebius/gosdk/services/nebius/iam/v1"
)

// selfSignedCert is a certificate as an identity provider would hand it out.
type selfSignedCert struct {
	pem         string
	fingerprint string // hex sha256 of the DER
}

func newSelfSignedCert(t *testing.T, cn string) selfSignedCert {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	serial, err := rand.Int(rand.Reader, big.NewInt(1<<60))
	if err != nil {
		t.Fatalf("serial: %v", err)
	}
	tpl := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC), NotAfter: time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	sum := sha256.Sum256(der)
	return selfSignedCert{
		pem:         string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		fingerprint: hex.EncodeToString(sum[:]),
	}
}

func federationConfig(tenant, ssoURL, issuer string, autoCreate bool, labelEnv, certName, certDesc, certData string) string {
	return fmt.Sprintf(`
resource "nebius_iam_v1_federation" "fed" {
  parent_id                  = %[1]q
  name                       = "corp-idp"
  labels                     = { env = %[5]q }
  user_account_auto_creation = %[4]t
  saml_settings = {
    idp_issuer  = %[3]q
    sso_url     = %[2]q
    force_authn = true
  }
}

resource "nebius_iam_v1_federation_certificate" "cert" {
  parent_id   = nebius_iam_v1_federation.fed.id
  name        = %[6]q
  description = %[7]q
  data        = %[8]q
}

data "nebius_iam_v1_federation" "by_id" {
  id = nebius_iam_v1_federation.fed.id
  # certificates_count is only final once the certificate exists.
  depends_on = [nebius_iam_v1_federation_certificate.cert]
}

data "nebius_iam_v1_federation" "by_name" {
  parent_id = %[1]q
  name      = nebius_iam_v1_federation.fed.name
}

data "nebius_iam_v1_federation_certificate" "by_id" {
  id = nebius_iam_v1_federation_certificate.cert.id
}

output "fed_id"           { value = nebius_iam_v1_federation.fed.id }
output "cert_id"          { value = nebius_iam_v1_federation_certificate.cert.id }
output "ds_id_sso_url"    { value = data.nebius_iam_v1_federation.by_id.saml_settings.sso_url }
output "ds_id_issuer"     { value = data.nebius_iam_v1_federation.by_id.saml_settings.idp_issuer }
output "ds_id_state"      { value = data.nebius_iam_v1_federation.by_id.status.state }
output "ds_id_certs"      { value = tostring(data.nebius_iam_v1_federation.by_id.status.certificates_count) }
output "ds_name_id"       { value = data.nebius_iam_v1_federation.by_name.id }
output "ds_name_sso_url"  { value = data.nebius_iam_v1_federation.by_name.saml_settings.sso_url }
output "ds_name_auto"     { value = data.nebius_iam_v1_federation.by_name.user_account_auto_creation == true ? "true" : "false" }
output "ds_cert_fp"       { value = data.nebius_iam_v1_federation_certificate.by_id.status.fingerprint }
output "ds_cert_algo"     { value = data.nebius_iam_v1_federation_certificate.by_id.status.algorithm }
output "ds_cert_desc"     { value = data.nebius_iam_v1_federation_certificate.by_id.description }
output "ds_cert_name"     { value = data.nebius_iam_v1_federation_certificate.by_id.name == null ? "" : data.nebius_iam_v1_federation_certificate.by_id.name }
output "ds_cert_data"     { value = data.nebius_iam_v1_federation_certificate.by_id.data }
`, tenant, ssoURL, issuer, autoCreate, labelEnv, certName, certDesc, certData)
}

func TestFederationLifecycle(t *testing.T) {
	inst := authStartMocker(t)
	ctx := t.Context()
	feds := iamsvc.NewFederationService(inst.SDK(t))
	certs := iamsvc.NewFederationCertificateService(inst.SDK(t))
	c1, c2 := newSelfSignedCert(t, "idp-one"), newSelfSignedCert(t, "idp-two")
	const (
		sso1    = "https://idp.example.com/sso"
		sso2    = "https://idp.example.com/sso/v2"
		issuer1 = "https://idp.example.com"
		issuer2 = "https://idp2.example.com"
	)
	getFed := func(id string) *iampb.Federation {
		t.Helper()
		f, err := feds.Get(ctx, &iampb.GetFederationRequest{Id: id})
		if err != nil {
			t.Fatalf("Federation Get(%s): %v", id, err)
		}
		return f
	}
	getCert := func(id string) *iampb.FederationCertificate {
		t.Helper()
		c, err := certs.Get(ctx, &iampb.GetFederationCertificateRequest{Id: id})
		if err != nil {
			t.Fatalf("FederationCertificate Get(%s): %v", id, err)
		}
		return c
	}

	tf := newTerraform(t, inst)
	tf.writeConfig(t, federationConfig(inst.TenantID, sso1, issuer1, false, "dev", "", "first", c1.pem))
	tf.init(t)
	tf.expectPlan(t, map[string]string{"fed": "create", "cert": "create", "by_id": "read", "by_name": "read"})
	tf.apply(t)
	tf.expectNoChanges(t)

	fedID, certID1 := tf.outputString(t, "fed_id"), tf.outputString(t, "cert_id")
	f := getFed(fedID)
	saml := f.GetSpec().GetSamlSettings()
	if f.GetStatus().GetState() != iampb.FederationStatus_ACTIVE || f.GetStatus().GetCertificatesCount() != 1 ||
		saml.GetSsoUrl() != sso1 || saml.GetIdpIssuer() != issuer1 || !saml.GetForceAuthn() || f.GetSpec().GetUserAccountAutoCreation() ||
		f.GetMetadata().GetParentId() != inst.TenantID || f.GetMetadata().GetLabels()["env"] != "dev" {
		t.Errorf("federation after create = %v", f)
	}
	c := getCert(certID1)
	if c.GetStatus().GetFingerprint() != c1.fingerprint || c.GetStatus().GetAlgorithm() != "ECDSA" || c.GetStatus().GetKeySize() != 256 ||
		c.GetSpec().GetDescription() != "first" || c.GetSpec().GetData() != c1.pem || c.GetMetadata().GetParentId() != fedID {
		t.Errorf("certificate after create = %v", c)
	}

	checkDataSources := func(sso, issuer string, auto bool, certsCount int, cert selfSignedCert, certName, certDesc string) {
		t.Helper()
		for out, want := range map[string]string{
			"ds_id_sso_url": sso, "ds_id_issuer": issuer, "ds_id_state": "ACTIVE", "ds_id_certs": strconv.Itoa(certsCount),
			"ds_name_id": fedID, "ds_name_sso_url": sso, "ds_name_auto": strconv.FormatBool(auto),
			"ds_cert_fp": cert.fingerprint, "ds_cert_algo": "ECDSA", "ds_cert_desc": certDesc, "ds_cert_name": certName, "ds_cert_data": cert.pem,
		} {
			if got := tf.outputString(t, out); got != want {
				t.Errorf("data source output %s = %q, want %q", out, got, want)
			}
		}
	}
	checkDataSources(sso1, issuer1, false, 1, c1, "", "first")

	// In-place update: federation sso_url, issuer, labels and auto creation; certificate name and description.
	tf.writeConfig(t, federationConfig(inst.TenantID, sso2, issuer2, true, "prod", "primary", "second", c1.pem))
	tf.expectPlan(t, map[string]string{"fed": "update", "cert": "update", "by_id": "read", "by_name": "read"})
	tf.apply(t)
	tf.expectNoChanges(t)
	if got := tf.outputString(t, "fed_id"); got != fedID {
		t.Errorf("in-place update changed the federation id: %s -> %s", fedID, got)
	}
	if got := tf.outputString(t, "cert_id"); got != certID1 {
		t.Errorf("in-place update changed the certificate id: %s -> %s", certID1, got)
	}
	f = getFed(fedID)
	if f.GetSpec().GetSamlSettings().GetSsoUrl() != sso2 || f.GetSpec().GetSamlSettings().GetIdpIssuer() != issuer2 ||
		!f.GetSpec().GetUserAccountAutoCreation() || f.GetMetadata().GetLabels()["env"] != "prod" ||
		f.GetStatus().GetState() != iampb.FederationStatus_ACTIVE {
		t.Errorf("federation after update = %v", f)
	}
	c = getCert(certID1)
	if c.GetMetadata().GetName() != "primary" || c.GetSpec().GetDescription() != "second" || c.GetStatus().GetFingerprint() != c1.fingerprint {
		t.Errorf("certificate after update = %v", c)
	}
	checkDataSources(sso2, issuer2, true, 1, c1, "primary", "second")

	// Replace: the certificate data is immutable.
	tf.writeConfig(t, federationConfig(inst.TenantID, sso2, issuer2, true, "prod", "primary", "second", c2.pem))
	tf.expectPlan(t, map[string]string{"fed": "no-op", "cert": "replace", "by_id": "read"})
	tf.apply(t)
	tf.expectNoChanges(t)
	certID2 := tf.outputString(t, "cert_id")
	if certID2 == certID1 {
		t.Fatalf("replace kept the certificate id %s", certID1)
	}
	if _, err := certs.Get(ctx, &iampb.GetFederationCertificateRequest{Id: certID1}); status.Code(err) != codes.NotFound {
		t.Errorf("old certificate Get after replace: %v, want NotFound", err)
	}
	if got := getCert(certID2).GetStatus().GetFingerprint(); got != c2.fingerprint {
		t.Errorf("new certificate fingerprint = %s, want %s", got, c2.fingerprint)
	}
	if n := getFed(fedID).GetStatus().GetCertificatesCount(); n != 1 {
		t.Errorf("certificates_count after replace = %d, want 1", n)
	}
	checkDataSources(sso2, issuer2, true, 1, c2, "primary", "second")

	// An active federation cannot be deleted: not through the API, not by terraform destroy.
	_, err := feds.Delete(ctx, &iampb.DeleteFederationRequest{Id: fedID})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("Delete of the active federation: code %v, want FailedPrecondition (%v)", status.Code(err), err)
	}
	if err := tf.tf.Destroy(ctx); err == nil {
		t.Fatal("terraform destroy of an active federation succeeded; want FailedPrecondition")
	}
	if got := getFed(fedID).GetStatus().GetState(); got != iampb.FederationStatus_ACTIVE {
		t.Fatalf("state after the failed destroy = %v", got)
	}

	// A real user deactivates the federation first, then destroy works.
	op, err := feds.Deactivate(ctx, &iampb.DeactivateFederationRequest{FederationId: fedID})
	if err != nil {
		t.Fatalf("Deactivate: %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("Deactivate wait: %v", err)
	}
	if got := getFed(fedID).GetStatus().GetState(); got != iampb.FederationStatus_INACTIVE {
		t.Fatalf("state after Deactivate = %v, want INACTIVE", got)
	}
	tf.destroy(t)
	if _, err := feds.Get(ctx, &iampb.GetFederationRequest{Id: fedID}); status.Code(err) != codes.NotFound {
		t.Errorf("federation Get after destroy: %v, want NotFound", err)
	}
	if _, err := certs.Get(ctx, &iampb.GetFederationCertificateRequest{Id: certID2}); status.Code(err) != codes.NotFound {
		t.Errorf("certificate Get after destroy: %v, want NotFound", err)
	}
	page, err := feds.List(ctx, &iampb.ListFederationsRequest{ParentId: inst.TenantID})
	if err != nil || len(page.GetItems()) != 0 {
		t.Errorf("List after destroy = %v, %v", page, err)
	}
}
