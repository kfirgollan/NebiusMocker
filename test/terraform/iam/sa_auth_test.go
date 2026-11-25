//go:build e2e

package iam

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/genproto/googleapis/rpc/code"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	auditv2pb "github.com/nebius/gosdk/proto/nebius/audit/v2"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"
	auditsvc "github.com/nebius/gosdk/services/nebius/audit/v2"
	iamsvc "github.com/nebius/gosdk/services/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	auditmod "github.com/kfirgollan/NebiusMocker/services/audit"
	"github.com/kfirgollan/NebiusMocker/services/iam"
)

// authKeyPair is an RSA key pair as a user would generate it with openssl.
type authKeyPair struct {
	privatePEM  string // PKCS#8
	publicPEM   string // PKIX
	fingerprint string // hex sha256 of the DER SPKI
}

func newAuthKeyPair(t *testing.T) authKeyPair {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	priv, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal private key: %v", err)
	}
	pub, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatalf("marshal public key: %v", err)
	}
	sum := sha256.Sum256(pub)
	return authKeyPair{
		privatePEM:  string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: priv})),
		publicPEM:   string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pub})),
		fingerprint: hex.EncodeToString(sum[:]),
	}
}

func authStartMocker(t *testing.T) *testkit.Instance {
	t.Helper()
	return testkit.StartT(t, []mocker.Module{iam.New(), auditmod.New()}, mockerOptions(
		testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce))...)
}

func authKeyConfig(project, publicPEM, name, description, expires string) string {
	return fmt.Sprintf(`
resource "nebius_iam_v1_service_account" "sa" {
  parent_id = %[1]q
  name      = "keyholder"
}

resource "nebius_iam_v1_auth_public_key" "key" {
  parent_id   = %[1]q
  name        = %[3]q
  description = %[4]q
  labels      = { team = "platform", env = %[3]q }
  expires_at  = %[5]q
  data        = %[2]q
  account = {
    service_account = { id = nebius_iam_v1_service_account.sa.id }
  }
}

data "nebius_iam_v1_auth_public_key" "by_id" {
  id = nebius_iam_v1_auth_public_key.key.id
}

output "key_id"          { value = nebius_iam_v1_auth_public_key.key.id }
output "ds_fingerprint"  { value = data.nebius_iam_v1_auth_public_key.by_id.status.fingerprint }
output "ds_description"  { value = data.nebius_iam_v1_auth_public_key.by_id.description }
output "ds_name"         { value = data.nebius_iam_v1_auth_public_key.by_id.name }
output "ds_data"         { value = data.nebius_iam_v1_auth_public_key.by_id.data }
output "ds_algorithm"    { value = data.nebius_iam_v1_auth_public_key.by_id.status.algorithm }
output "ds_state"        { value = data.nebius_iam_v1_auth_public_key.by_id.status.state }
`, project, publicPEM, name, description, expires)
}

func getAuthKey(t *testing.T, svc iamsvc.AuthPublicKeyService, id string) *iampb.AuthPublicKey {
	t.Helper()
	k, err := svc.Get(t.Context(), &iampb.GetAuthPublicKeyRequest{Id: id})
	if err != nil {
		t.Fatalf("AuthPublicKey Get(%s): %v", id, err)
	}
	return k
}

func TestAuthPublicKeyLifecycle(t *testing.T) {
	inst := authStartMocker(t)
	keys := iamsvc.NewAuthPublicKeyService(inst.SDK(t))
	kp1, kp2 := newAuthKeyPair(t), newAuthKeyPair(t)
	const (
		expiry1 = "2099-01-01T00:00:00Z"
		expiry2 = "2098-06-01T00:00:00Z"
	)

	tf := newTerraform(t, inst)
	tf.writeConfig(t, authKeyConfig(inst.ProjectID, kp1.publicPEM, "ci-key", "first", expiry1))
	tf.init(t)
	tf.expectPlan(t, map[string]string{"sa": "create", "key": "create", "by_id": "read"})
	tf.apply(t)
	tf.expectNoChanges(t)

	id1 := tf.outputString(t, "key_id")
	k := getAuthKey(t, keys, id1)
	if k.GetStatus().GetState() != iampb.AuthPublicKeyStatus_ACTIVE ||
		k.GetStatus().GetFingerprint() != kp1.fingerprint ||
		k.GetStatus().GetAlgorithm() != "RSA" || k.GetStatus().GetKeySize() != 2048 {
		t.Errorf("status = %v, want ACTIVE, fingerprint %s, RSA, 2048", k.GetStatus(), kp1.fingerprint)
	}
	if k.GetSpec().GetDescription() != "first" || k.GetMetadata().GetParentId() != inst.ProjectID ||
		k.GetSpec().GetAccount().GetServiceAccount().GetId() == "" ||
		k.GetMetadata().GetLabels()["team"] != "platform" {
		t.Errorf("spec/metadata = %v / %v", k.GetSpec(), k.GetMetadata())
	}
	if want := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC); !k.GetSpec().GetExpiresAt().AsTime().Equal(want) {
		t.Errorf("expires_at = %v, want %v", k.GetSpec().GetExpiresAt().AsTime(), want)
	}

	// The data source reads the same fields by id.
	for out, want := range map[string]string{
		"ds_fingerprint": kp1.fingerprint, "ds_description": "first", "ds_name": "ci-key",
		"ds_data": kp1.publicPEM, "ds_algorithm": "RSA", "ds_state": "ACTIVE",
	} {
		if got := tf.outputString(t, out); got != want {
			t.Errorf("data source %s = %q, want %q", out, got, want)
		}
	}

	// In-place update: name, labels, description.
	tf.writeConfig(t, authKeyConfig(inst.ProjectID, kp1.publicPEM, "ci-key-2", "second", expiry1))
	tf.expectPlan(t, map[string]string{"sa": "no-op", "key": "update", "by_id": "read"})
	tf.apply(t)
	tf.expectNoChanges(t)
	k = getAuthKey(t, keys, id1)
	if k.GetMetadata().GetName() != "ci-key-2" || k.GetSpec().GetDescription() != "second" ||
		k.GetMetadata().GetLabels()["env"] != "ci-key-2" {
		t.Errorf("after update: metadata %v spec %v", k.GetMetadata(), k.GetSpec())
	}
	if tf.outputString(t, "key_id") != id1 {
		t.Errorf("in-place update changed the key id")
	}

	// Replace: the data is immutable.
	tf.writeConfig(t, authKeyConfig(inst.ProjectID, kp2.publicPEM, "ci-key-2", "second", expiry1))
	tf.expectPlan(t, map[string]string{"sa": "no-op", "key": "replace", "by_id": "read"})
	tf.apply(t)
	tf.expectNoChanges(t)
	id2 := tf.outputString(t, "key_id")
	if id2 == id1 {
		t.Fatalf("replace kept the id %s", id1)
	}
	if _, err := keys.Get(t.Context(), &iampb.GetAuthPublicKeyRequest{Id: id1}); status.Code(err) != codes.NotFound {
		t.Errorf("old key Get after replace: %v, want NotFound", err)
	}
	if got := getAuthKey(t, keys, id2).GetStatus().GetFingerprint(); got != kp2.fingerprint {
		t.Errorf("new key fingerprint = %s, want %s", got, kp2.fingerprint)
	}

	// Replace: expires_at is immutable.
	tf.writeConfig(t, authKeyConfig(inst.ProjectID, kp2.publicPEM, "ci-key-2", "second", expiry2))
	tf.expectPlan(t, map[string]string{"sa": "no-op", "key": "replace", "by_id": "read"})
	tf.apply(t)
	tf.expectNoChanges(t)
	id3 := tf.outputString(t, "key_id")
	if _, err := keys.Get(t.Context(), &iampb.GetAuthPublicKeyRequest{Id: id2}); status.Code(err) != codes.NotFound {
		t.Errorf("replaced key Get: %v, want NotFound", err)
	}
	if want := time.Date(2098, 6, 1, 0, 0, 0, 0, time.UTC); !getAuthKey(t, keys, id3).GetSpec().GetExpiresAt().AsTime().Equal(want) {
		t.Errorf("expires_at after replace = %v", getAuthKey(t, keys, id3).GetSpec().GetExpiresAt())
	}

	tf.destroy(t)
	if _, err := keys.Get(t.Context(), &iampb.GetAuthPublicKeyRequest{Id: id3}); status.Code(err) != codes.NotFound {
		t.Errorf("Get after destroy: %v, want NotFound", err)
	}
}

// saCredentials is a service account with an uploaded public key, set up the
// way an admin would: through the API, then added to the seeded editors group.
type saCredentials struct {
	saID, keyID string
	kp          authKeyPair
	dir         string
}

func newEditorServiceAccount(t *testing.T, inst *testkit.Instance) saCredentials {
	t.Helper()
	ctx := t.Context()
	sdk := inst.SDK(t)
	kp := newAuthKeyPair(t)

	saOp, err := iamsvc.NewServiceAccountService(sdk).Create(ctx, &iampb.CreateServiceAccountRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: inst.ProjectID, Name: "tf-runner"},
		Spec:     &iampb.ServiceAccountSpec{},
	})
	if err != nil {
		t.Fatalf("create service account: %v", err)
	}
	if _, err := saOp.Wait(ctx); err != nil {
		t.Fatalf("service account wait: %v", err)
	}
	saID := saOp.ResourceID()

	keyOp, err := iamsvc.NewAuthPublicKeyService(sdk).Create(ctx, &iampb.CreateAuthPublicKeyRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: inst.ProjectID, Name: "runner-key"},
		Spec: &iampb.AuthPublicKeySpec{
			Account:   &iampb.Account{Type: &iampb.Account_ServiceAccount_{ServiceAccount: &iampb.Account_ServiceAccount{Id: saID}}},
			ExpiresAt: timestamppb.New(time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)),
			Data:      kp.publicPEM,
		},
	})
	if err != nil {
		t.Fatalf("create auth public key: %v", err)
	}
	if _, err := keyOp.Wait(ctx); err != nil {
		t.Fatalf("auth public key wait: %v", err)
	}

	editors, err := iamsvc.NewGroupService(sdk).GetByName(ctx, &iampb.GetGroupByNameRequest{ParentId: inst.TenantID, Name: "editors"})
	if err != nil {
		t.Fatalf("get editors group: %v", err)
	}
	memOp, err := iamsvc.NewGroupMembershipService(sdk).Create(ctx, &iampb.CreateGroupMembershipRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: editors.GetMetadata().GetId()},
		Spec:     &iampb.GroupMembershipSpec{MemberId: saID},
	})
	if err != nil {
		t.Fatalf("add to editors: %v", err)
	}
	if _, err := memOp.Wait(ctx); err != nil {
		t.Fatalf("membership wait: %v", err)
	}

	return saCredentials{saID: saID, keyID: keyOp.ResourceID(), kp: kp, dir: t.TempDir()}
}

func (c saCredentials) write(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(c.dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return p
}

// credentialsFile writes the file format the Nebius CLI and SDKs read.
func (c saCredentials) credentialsFile(t *testing.T) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"subject-credentials": map[string]string{
		"type": "JWT", "alg": "RS256", "private-key": c.kp.privatePEM,
		"kid": c.keyID, "iss": c.saID, "sub": c.saID,
	}})
	if err != nil {
		t.Fatalf("marshal credentials: %v", err)
	}
	return c.write(t, "credentials.json", string(b))
}

func groupConfig(tenant string) string {
	return fmt.Sprintf(`
resource "nebius_iam_v1_group" "made_by_sa" {
  parent_id = %q
  name      = "made-by-sa"
}
`, tenant)
}

// runAsServiceAccount drives plan, apply, empty plan and destroy with the
// provider authenticated only by auth, and checks that the audit log credits
// the create to the service account (so the exchange happened server-side).
func runAsServiceAccount(t *testing.T, inst *testkit.Instance, c saCredentials, auth string) {
	t.Helper()
	runAsServiceAccountWith(t, inst, c, auth, groupConfig(inst.TenantID))
}

// runAsServiceAccountWith is runAsServiceAccount with a custom body that must
// declare the resource nebius_iam_v1_group.made_by_sa.
func runAsServiceAccountWith(t *testing.T, inst *testkit.Instance, c saCredentials, auth, body string) {
	t.Helper()
	tf := newTerraformAs(t, inst, "")
	tf.setAuth(auth)
	tf.writeConfig(t, body)
	tf.init(t)
	tf.expectPlan(t, map[string]string{"made_by_sa": "create"})
	tf.apply(t)
	tf.expectNoChanges(t)

	ctx := t.Context()
	sdk := inst.SDK(t)
	g, err := iamsvc.NewGroupService(sdk).GetByName(ctx, &iampb.GetGroupByNameRequest{ParentId: inst.TenantID, Name: "made-by-sa"})
	if err != nil {
		t.Fatalf("group made by the service account: %v", err)
	}
	filter := fmt.Sprintf("resource.metadata.id='%s' AND action='CREATE' AND status='DONE'", g.GetMetadata().GetId())
	var events []*auditv2pb.AuditEvent
	for range 50 {
		resp, err := auditsvc.NewAuditEventService(sdk).List(ctx, &auditv2pb.ListAuditEventRequest{
			ParentId: inst.TenantID, Region: "eu-north1", Filter: filter, PageSize: 100,
			Start: timestamppb.New(clock.Real{}.Now().Add(-time.Hour)), End: timestamppb.New(clock.Real{}.Now().Add(time.Hour)),
		})
		if err != nil {
			t.Fatalf("AuditEventService.List: %v", err)
		}
		if events = resp.GetItems(); len(events) > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond) //nolint:forbidigo // poll for the asynchronously written audit event
	}
	if len(events) != 1 {
		t.Fatalf("audit events for the create = %d, want 1", len(events))
	}
	sub := events[0].GetAuthentication().GetSubject()
	if !events[0].GetAuthentication().GetAuthenticated() || (sub.GetName() != c.saID && sub.GetServiceAccountId() != c.saID) {
		t.Errorf("create principal = %v, want service account %s", events[0].GetAuthentication(), c.saID)
	}
	if events[0].GetResponse().GetStatusCode() != code.Code_OK {
		t.Errorf("create status = %v, want OK", events[0].GetResponse().GetStatusCode())
	}

	tf.destroy(t)
	if _, err := iamsvc.NewGroupService(sdk).Get(ctx, &iampb.GetGroupRequest{Id: g.GetMetadata().GetId()}); status.Code(err) != codes.NotFound {
		t.Errorf("group Get after destroy: %v, want NotFound", err)
	}
}

func TestProviderServiceAccountCredentialsFile(t *testing.T) {
	inst := authStartMocker(t)
	c := newEditorServiceAccount(t, inst)
	runAsServiceAccount(t, inst, c, fmt.Sprintf("service_account = { credentials_file = %q }", c.credentialsFile(t)))
}

func TestProviderServiceAccountPrivateKeyFile(t *testing.T) {
	inst := authStartMocker(t)
	c := newEditorServiceAccount(t, inst)
	pemPath := c.write(t, "private.pem", c.kp.privatePEM)
	runAsServiceAccount(t, inst, c, fmt.Sprintf(
		"service_account = { account_id = %q, public_key_id = %q, private_key_file = %q }", c.saID, c.keyID, pemPath))
}

// TestProviderServiceAccountDeactivatedKey: once the key is deactivated the
// provider can no longer exchange its assertion for a token.
func TestProviderServiceAccountDeactivatedKey(t *testing.T) {
	inst := authStartMocker(t)
	c := newEditorServiceAccount(t, inst)
	auth := fmt.Sprintf("service_account = { credentials_file = %q }", c.credentialsFile(t))

	tf := newTerraformAs(t, inst, "")
	tf.setAuth(auth)
	tf.writeConfig(t, groupConfig(inst.TenantID))
	tf.init(t)
	tf.expectPlan(t, map[string]string{"made_by_sa": "create"}) // works while the key is active

	keys := iamsvc.NewAuthPublicKeyService(inst.SDK(t))
	op, err := keys.Deactivate(t.Context(), &iampb.DeactivateAuthPublicKeyRequest{Id: c.keyID})
	if err != nil {
		t.Fatalf("Deactivate: %v", err)
	}
	if _, err := op.Wait(t.Context()); err != nil {
		t.Fatalf("Deactivate wait: %v", err)
	}
	if got := getAuthKey(t, keys, c.keyID).GetStatus().GetState(); got != iampb.AuthPublicKeyStatus_INACTIVE {
		t.Fatalf("state after Deactivate = %v, want INACTIVE", got)
	}

	before := tf.out.Len()
	if err := tf.tf.Apply(t.Context()); err == nil {
		t.Fatalf("terraform apply succeeded with a deactivated key, want an authentication failure")
	}
	if out := tf.out.String()[before:]; !strings.Contains(out, "Unauthenticated") || !strings.Contains(out, "INACTIVE") {
		t.Errorf("apply failed without an Unauthenticated/INACTIVE error:\n%s", out)
	}
	if _, err := iamsvc.NewGroupService(inst.SDK(t)).GetByName(t.Context(), &iampb.GetGroupByNameRequest{ParentId: inst.TenantID, Name: "made-by-sa"}); status.Code(err) != codes.NotFound {
		t.Errorf("group exists after a failed apply: %v", err)
	}
}

// TestProviderIAMTokenEphemeral evaluates the provider's nebius_iam_token
// ephemeral resource while the provider authenticates as a service account:
// the provider exchanges the service account's JWT for a token, the ephemeral
// resource yields that token, and a second provider configured only with it
// creates a group, which the audit log credits to the service account.
func TestProviderIAMTokenEphemeral(t *testing.T) {
	inst := authStartMocker(t)
	c := newEditorServiceAccount(t, inst)
	auth := fmt.Sprintf("service_account = { credentials_file = %q }", c.credentialsFile(t))
	runAsServiceAccountWith(t, inst, c, auth, fmt.Sprintf(`
ephemeral "nebius_iam_token" "sa" {}

provider "nebius" {
  alias           = "via_token"
  resolvers       = { "*" = %[1]q }
  address_options = { "*" = { insecure = true } }
  token           = ephemeral.nebius_iam_token.sa.token
}

resource "nebius_iam_v1_group" "made_by_sa" {
  provider  = nebius.via_token
  parent_id = %[2]q
  name      = "made-by-sa"
}
`, inst.Addr, inst.TenantID))
}
