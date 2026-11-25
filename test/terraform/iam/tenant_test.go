//go:build e2e

package iam

import (
	"fmt"
	"strings"
	"testing"

	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/services/iam"
)

// TestTenantDataSourcesAsViewer reads the seeded tenant through the v1 and v2
// data sources as a read-only principal.
func TestTenantDataSourcesAsViewer(t *testing.T) {
	inst := startProjectMocker(t, []mocker.Module{iam.New()})
	tf := newTerraformAs(t, inst, config.DefaultViewerToken)
	tf.writeConfig(t, fmt.Sprintf(`
data "nebius_iam_v1_tenant" "v1" { id = %[1]q }
data "nebius_iam_v2_tenant" "v2_id" { id = %[1]q }
data "nebius_iam_v2_tenant" "v2_name" {
  name      = "default"
  parent_id = %[1]q
}

output "v1"      { value = [data.nebius_iam_v1_tenant.v1.id, data.nebius_iam_v1_tenant.v1.name, data.nebius_iam_v1_tenant.v1.status.container_state] }
output "v2_id"   { value = [data.nebius_iam_v2_tenant.v2_id.id, data.nebius_iam_v2_tenant.v2_id.name, data.nebius_iam_v2_tenant.v2_id.status.tenant_state] }
output "v2_name" { value = [data.nebius_iam_v2_tenant.v2_name.id, data.nebius_iam_v2_tenant.v2_name.name, data.nebius_iam_v2_tenant.v2_name.status.tenant_state] }
`, inst.TenantID))
	tf.init(t)
	tf.applyNow(t)
	outs, err := tf.tf.Output(t.Context())
	if err != nil {
		t.Fatalf("terraform output: %v", err)
	}
	for name, want := range map[string]string{
		"v1":    fmt.Sprintf(`[%q,"default","ACTIVE"]`, inst.TenantID),
		"v2_id": fmt.Sprintf(`[%q,"default","ACTIVE"]`, inst.TenantID),
	} {
		o, ok := outs[name]
		if !ok {
			t.Fatalf("output %s missing", name)
		}
		if got := strings.Join(strings.Fields(string(o.Value)), ""); got != want {
			t.Errorf("%s = %s, want %s", name, got, want)
		}
	}
	tf.expectNoChanges(t)
}
