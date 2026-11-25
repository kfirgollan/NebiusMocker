//go:build e2e

package ai

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	aipb "github.com/nebius/gosdk/proto/nebius/ai/v1"
	aisvc "github.com/nebius/gosdk/services/nebius/ai/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/ai"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/services/mysterybox"
)

func startMockWithMysteryBox(t *testing.T) *testkit.Instance {
	t.Helper()
	modules := []mocker.Module{iam.New(), mysterybox.New(), ai.New(ai.WithListen("127.0.0.1:0"))}
	return testkit.StartT(t, modules, testkit.WithAuthz(authz.ModeEnforce))
}

// mysteryBoxEndpointConfig renders a secret holding one payload entry and an
// endpoint whose auth token comes from that secret.
func mysteryBoxEndpointConfig(projectID, payloadKey string) string {
	return fmt.Sprintf(`
resource "nebius_mysterybox_v1_secret" "s" {
  parent_id = %q
  name      = "endpoint-auth"

  secret_version = {
    description = "token"
    set_primary = true
    payload     = [{ key = %q, string_value = "mb-token-1" }]
  }
}

resource "nebius_ai_v1_endpoint" "e" {
  parent_id = %q
  name      = "web"
  image     = "nginx:alpine"
  platform  = "cpu-d3"
  preset    = "4vcpu-16gb"
  disk      = { type = "NETWORK_SSD", size_bytes = 21474836480 }
  ports     = [{ container_port = 80, protocol = "HTTP" }]
  auth_token_mysterybox_secret = { secret_id = nebius_mysterybox_v1_secret.s.id }
}

output "mb_endpoint" {
  value = nebius_ai_v1_endpoint.e.status.public_endpoints[0]
}
`, projectID, payloadKey, projectID)
}

// TestEndpointMysteryBoxAuth proves endpoint auth reads AUTH_TOKEN from a real
// MysteryBox secret created through Terraform.
func TestEndpointMysteryBoxAuth(t *testing.T) {
	inst := startMockWithMysteryBox(t)
	endpoints := aisvc.NewEndpointService(inst.SDK(t))
	tf := newTerraform(t, inst)

	tf.writeConfig(t, mysteryBoxEndpointConfig(inst.ProjectID, "AUTH_TOKEN"))
	tf.init(t)
	tf.apply(t)
	ids := tf.stateIDs(t)
	if ids["e"] == "" || ids["s"] == "" {
		t.Fatalf("missing IDs in state: %v", ids)
	}

	got, err := endpoints.Get(t.Context(), &aipb.GetEndpointRequest{Id: ids["e"]})
	if err != nil {
		t.Fatalf("Get endpoint in mock: %v", err)
	}
	ref := got.GetSpec().GetAuthTokenMysteryboxSecret()
	if ref.GetSecretId() != ids["s"] || !strings.HasPrefix(ref.GetVersionId(), "mbsecver-") {
		t.Errorf("auth_token_mysterybox_secret = %v, want the secret with a pinned mbsecver- version", ref)
	}
	if got.GetStatus().GetState() != aipb.EndpointStatus_RUNNING {
		t.Errorf("mock state = %v, want RUNNING", got.GetStatus().GetState())
	}
	tf.expectNoChanges(t)

	url := tf.outputString(t, "mb_endpoint")
	assertCall(t, url, "mb-token-1", http.StatusOK)
	assertCall(t, url, "wrong", http.StatusUnauthorized)
	assertCall(t, url, "", http.StatusUnauthorized)

	tf.destroy(t)
}

// TestEndpointMysteryBoxMissingKey proves a secret without AUTH_TOKEN is rejected.
func TestEndpointMysteryBoxMissingKey(t *testing.T) {
	inst := startMockWithMysteryBox(t)
	tf := newTerraform(t, inst)

	tf.writeConfig(t, mysteryBoxEndpointConfig(inst.ProjectID, "OTHER_KEY"))
	tf.init(t)
	err := tf.tf.Apply(t.Context())
	if err == nil {
		t.Fatal("apply succeeded, want failure for a secret without AUTH_TOKEN")
	}
	if !strings.Contains(err.Error(), "AUTH_TOKEN") && !strings.Contains(tf.out.String(), "AUTH_TOKEN") {
		t.Fatalf("apply error does not mention AUTH_TOKEN: %v\n%s", err, tf.out.String())
	}
}
