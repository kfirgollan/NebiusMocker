//go:build e2e

package iam

import (
	"fmt"
	"strings"
	"testing"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/iam"
)

const (
	deployerToken = "deployer-token"
	deployerID    = "serviceaccount-e00mock00000000deployer"
)

// TestAccessGrantedThroughTerraform proves enforcement end to end with the
// official provider: a service account is denied, an admin grants it the editor
// role with nebius_iam_v1_group_membership + nebius_iam_v1_access_permit, the
// service account succeeds, and after revocation it is denied again.
func TestAccessGrantedThroughTerraform(t *testing.T) {
	inst := testkit.StartT(t, []mocker.Module{iam.New()}, mockerOptions(
		testkit.WithAuthnRequired(),
		testkit.WithAuthz(authz.ModeEnforce),
		testkit.WithTokens(map[string]string{deployerToken: deployerID}),
	)...)
	admin := newTerraform(t, inst)
	deployer := newTerraformAs(t, inst, deployerToken)
	deployerConfig := fmt.Sprintf(`
resource "nebius_iam_v1_group" "made_by_deployer" {
  parent_id = %q
  name      = "made-by-deployer"
}
`, inst.TenantID)

	// 1. Without a grant the deployer cannot create anything.
	deployer.writeConfig(t, deployerConfig)
	deployer.init(t)
	deployer.expectApplyError(t, "PermissionDenied")

	// 2. The admin grants the deployer editor on the tenant.
	admin.writeConfig(t, fmt.Sprintf(`
resource "nebius_iam_v1_group" "deployers" {
  parent_id = %[1]q
  name      = "deployers"
}

resource "nebius_iam_v1_group_membership" "deployer" {
  parent_id = nebius_iam_v1_group.deployers.id
  member_id = %[2]q
}

resource "nebius_iam_v1_access_permit" "deployers_edit_tenant" {
  parent_id   = nebius_iam_v1_group.deployers.id
  resource_id = %[1]q
  role        = "editor"
}
`, inst.TenantID, deployerID))
	admin.init(t)
	admin.expectPlan(t, map[string]string{"deployers": "create", "deployer": "create", "deployers_edit_tenant": "create"})
	admin.apply(t)
	admin.expectNoChanges(t)

	// 3. Now the deployer succeeds, and its config round-trips without drift.
	deployer.expectPlan(t, map[string]string{"made_by_deployer": "create"})
	deployer.apply(t)
	deployer.expectNoChanges(t)

	// 4. The deployer cleans up while still granted; then the admin revokes.
	deployer.destroy(t)
	admin.destroy(t)

	// 5. Without the grant the deployer is denied again.
	deployer.expectApplyError(t, "PermissionDenied")
}

// expectApplyError runs a plan+apply that must fail with want in the output.
func (r *tfRun) expectApplyError(t *testing.T, want string) {
	t.Helper()
	before := r.out.Len()
	err := r.tf.Apply(t.Context())
	if err == nil {
		t.Fatalf("terraform apply succeeded, want failure containing %q", want)
	}
	out := r.out.String()[before:] + err.Error()
	if !strings.Contains(out, want) {
		t.Fatalf("terraform apply failed without %q:\n%s", want, out)
	}
}
