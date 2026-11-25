//go:build e2e

package iam

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	iamv2pb "github.com/nebius/gosdk/proto/nebius/iam/v2"
	mysteryboxpb "github.com/nebius/gosdk/proto/nebius/mysterybox/v1"
	iamv2svc "github.com/nebius/gosdk/services/nebius/iam/v2"
	mysteryboxsvc "github.com/nebius/gosdk/services/nebius/mysterybox/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/services/mysterybox"
)

// mysteryBoxKeyConfig: a service account and an access key delivered through
// MysteryBox, with the key also read back by the data source.
func mysteryBoxKeyConfig(project string) string {
	return fmt.Sprintf(`
resource "nebius_iam_v1_service_account" "sa" {
  parent_id = %[1]q
  name      = "mb-writer"
}

resource "nebius_iam_v2_access_key" "key" {
  parent_id            = %[1]q
  description          = "delivered through MysteryBox"
  secret_delivery_mode = "MYSTERY_BOX"
  account = {
    service_account = { id = nebius_iam_v1_service_account.sa.id }
  }
}

data "nebius_iam_v2_access_key" "key" {
  id = nebius_iam_v2_access_key.key.id
}

output "access_key_id"       { value = nebius_iam_v2_access_key.key.id }
output "secret_reference_id" { value = nebius_iam_v2_access_key.key.status.secret_reference_id }
output "key_by_data"         { value = data.nebius_iam_v2_access_key.key.status.secret_reference_id }
`, project)
}

// outputString returns a root output as a string.
func (r *tfRun) outputString(t *testing.T, name string) string {
	t.Helper()
	outs, err := r.tf.Output(t.Context())
	if err != nil {
		t.Fatalf("terraform output: %v", err)
	}
	o, ok := outs[name]
	if !ok {
		t.Fatalf("output %q missing", name)
	}
	var s string
	if err := json.Unmarshal(o.Value, &s); err != nil {
		t.Fatalf("decode output %q: %v", name, err)
	}
	return s
}

// TestAccessKeyMysteryBoxDelivery proves secret_delivery_mode = MYSTERY_BOX:
// the secret goes to a MysteryBox secret in the key's project, the key only
// carries secret_reference_id, and destroying the key leaves the secret.
func TestAccessKeyMysteryBoxDelivery(t *testing.T) {
	inst := testkit.StartT(t, []mocker.Module{iam.New(), mysterybox.New()}, mockerOptions(
		testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce))...)
	sdk := inst.SDK(t)
	keys := iamv2svc.NewAccessKeyService(sdk)
	secrets := mysteryboxsvc.NewSecretService(sdk)

	tf := newTerraform(t, inst)
	tf.writeConfig(t, mysteryBoxKeyConfig(inst.ProjectID))
	tf.init(t)
	tf.expectPlan(t, map[string]string{"sa": "create", "key": "create"})
	tf.apply(t)
	tf.expectNoChanges(t)
	keyID := tf.outputString(t, "access_key_id")
	secretID := tf.outputString(t, "secret_reference_id")
	if got := tf.outputString(t, "key_by_data"); got != secretID {
		t.Errorf("data source secret_reference_id = %q, want %q", got, secretID)
	}

	// The mock holds the key without its secret, and the secret in MysteryBox.
	key, err := keys.Get(t.Context(), &iamv2pb.GetAccessKeyRequest{Id: keyID})
	if err != nil {
		t.Fatalf("AccessKey Get: %v", err)
	}
	if s := key.GetStatus().GetSecret(); s != "" {
		t.Errorf("status.secret = %q, want empty with MYSTERY_BOX delivery", s)
	}
	if key.GetStatus().GetState() != iamv2pb.AccessKeyStatus_ACTIVE {
		t.Errorf("state = %v, want ACTIVE", key.GetStatus().GetState())
	}
	if got := key.GetStatus().GetSecretReferenceId(); got != secretID || !strings.HasPrefix(got, "mbsec-") {
		t.Errorf("status.secret_reference_id = %q, want terraform output %q with the mbsec- prefix", got, secretID)
	}
	sec, err := secrets.Get(t.Context(), &mysteryboxpb.GetSecretRequest{Id: secretID})
	if err != nil {
		t.Fatalf("MysteryBox secret Get: %v", err)
	}
	if sec.GetMetadata().GetParentId() != inst.ProjectID || sec.GetMetadata().GetName() != keyID {
		t.Errorf("secret parent/name = %q/%q, want %q/%q", sec.GetMetadata().GetParentId(), sec.GetMetadata().GetName(), inst.ProjectID, keyID)
	}
	st, err := tf.tf.Show(t.Context())
	if err != nil || st.Values == nil || st.Values.RootModule == nil {
		t.Fatalf("terraform show: %v", err)
	}
	for _, res := range st.Values.RootModule.Resources {
		if res.Address != "nebius_iam_v2_access_key.key" {
			continue
		}
		stat, _ := res.AttributeValues["status"].(map[string]any)
		if s, _ := stat["secret"].(string); s != "" {
			t.Errorf("terraform state status.secret = %q, want empty", s)
		}
		if got, _ := stat["secret_reference_id"].(string); got != secretID {
			t.Errorf("terraform state status.secret_reference_id = %q, want %q", got, secretID)
		}
	}

	// Destroying the key deletes it; the secret stays (Nebius documents no effect on it).
	tf.destroy(t)
	if _, err := keys.Get(t.Context(), &iamv2pb.GetAccessKeyRequest{Id: keyID}); status.Code(err) != codes.NotFound {
		t.Fatalf("AccessKey Get after destroy: %v, want NotFound", err)
	}
	if _, err := secrets.Get(t.Context(), &mysteryboxpb.GetSecretRequest{Id: secretID}); err != nil {
		t.Fatalf("MysteryBox secret after key destroy: %v, want it to remain", err)
	}
}
