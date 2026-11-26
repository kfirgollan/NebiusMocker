//go:build e2e

package iam

import (
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"
	iamsvc "github.com/nebius/gosdk/services/nebius/iam/v1"

	mockauth "github.com/kfirgollan/NebiusMocker/pkg/auth"
	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/iam"
)

// fcTFConfig declares a service account in each of two projects and federated
// credentials in project index `in` (0 or 1) for that project's account.
func fcTFConfig(projects [2]string, in int, issuer, sub, team, jwks string) string {
	jwksLine := ""
	if jwks != "" {
		jwksLine = fmt.Sprintf("jwk_set_json = %q", jwks)
	}
	return fmt.Sprintf(`
resource "nebius_iam_v1_service_account" "sa0" {
  parent_id = %[1]q
  name      = "ci"
}

resource "nebius_iam_v1_service_account" "sa1" {
  parent_id = %[2]q
  name      = "ci"
}

locals {
  sa = [nebius_iam_v1_service_account.sa0.id, nebius_iam_v1_service_account.sa1.id]
}

resource "nebius_iam_v1_federated_credentials" "fc" {
  parent_id            = [%[1]q, %[2]q][%[3]d]
  name                 = "github-ci"
  labels               = { team = %[6]q }
  subject_id           = local.sa[%[3]d]
  federated_subject_id = %[5]q
  oidc_provider = {
    issuer_url = %[4]q
    %[7]s
  }
}

data "nebius_iam_v1_federated_credentials" "by_id" {
  id = nebius_iam_v1_federated_credentials.fc.id
}

data "nebius_iam_v1_federated_credentials" "by_name" {
  parent_id = nebius_iam_v1_federated_credentials.fc.parent_id
  name      = nebius_iam_v1_federated_credentials.fc.name
}

output "fc_id"      { value = nebius_iam_v1_federated_credentials.fc.id }
output "sa_id"      { value = local.sa[%[3]d] }
output "ds_id_sub"  { value = data.nebius_iam_v1_federated_credentials.by_id.federated_subject_id }
output "ds_id_iss"  { value = data.nebius_iam_v1_federated_credentials.by_id.oidc_provider.issuer_url }
output "ds_name_id" { value = data.nebius_iam_v1_federated_credentials.by_name.id }
output "ds_name_sa" { value = data.nebius_iam_v1_federated_credentials.by_name.subject_id }
`, projects[0], projects[1], in, issuer, sub, team, jwksLine)
}

func fcTFJWKS(t *testing.T) string {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	b, err := mockauth.JWKSet{Keys: []mockauth.JWK{{KeyID: "tf-key", Key: &k.PublicKey}}}.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// fcTFCheck reads the credentials through gosdk and compares them with the step's config.
func fcTFCheck(t *testing.T, svc iamsvc.FederatedCredentialsService, tf *tfRun, project, sub, team, jwks string) string {
	t.Helper()
	id := tf.outputString(t, "fc_id")
	c, err := svc.Get(t.Context(), &iampb.GetFederatedCredentialsRequest{Id: id})
	if err != nil {
		t.Fatalf("Get federated credentials %s: %v", id, err)
	}
	if c.GetMetadata().GetParentId() != project || c.GetMetadata().GetName() != "github-ci" ||
		c.GetMetadata().GetLabels()["team"] != team || c.GetSpec().GetFederatedSubjectId() != sub ||
		c.GetSpec().GetSubjectId() != tf.outputString(t, "sa_id") || c.GetSpec().GetOidcProvider().GetJwkSetJson() != jwks {
		t.Errorf("federated credentials = %v, want parent %s sub %s team %s jwks %q", c, project, sub, team, jwks)
	}
	for out, want := range map[string]string{
		"ds_id_sub": sub, "ds_id_iss": c.GetSpec().GetOidcProvider().GetIssuerUrl(),
		"ds_name_id": id, "ds_name_sa": c.GetSpec().GetSubjectId(),
	} {
		if got := tf.outputString(t, out); got != want {
			t.Errorf("data source %s = %q, want %q", out, got, want)
		}
	}
	return id
}

func TestFederatedCredentialsLifecycle(t *testing.T) {
	inst := testkit.StartT(t, []mocker.Module{iam.New()}, mockerOptions(
		testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce), testkit.WithProjects(config.ProjectSeed{Name: "second"}))...)
	projects := [2]string{inst.Projects[0].ID, inst.Projects[1].ID}
	svc := iamsvc.NewFederatedCredentialsService(inst.SDK(t))
	issuer := "http://" + inst.Addr + "/_mocker/oidc/github"
	jwks := fcTFJWKS(t)

	tf := newTerraform(t, inst)
	tf.writeConfig(t, fcTFConfig(projects, 0, issuer, "repo:org/app:ref:main", "ci", ""))
	tf.init(t)
	tf.expectPlan(t, map[string]string{"sa0": "create", "sa1": "create", "fc": "create", "by_id": "read", "by_name": "read"})
	tf.apply(t)
	tf.expectNoChanges(t)
	id1 := fcTFCheck(t, svc, tf, projects[0], "repo:org/app:ref:main", "ci", "")

	// In place: federated_subject_id, labels and jwk_set_json.
	tf.writeConfig(t, fcTFConfig(projects, 0, issuer, "repo:org/app:environment:prod", "infra", jwks))
	tf.expectPlan(t, map[string]string{"sa0": "no-op", "sa1": "no-op", "fc": "update", "by_id": "read", "by_name": "read"})
	tf.apply(t)
	tf.expectNoChanges(t)
	if id := fcTFCheck(t, svc, tf, projects[0], "repo:org/app:environment:prod", "infra", jwks); id != id1 {
		t.Fatalf("in-place update changed the id from %s to %s", id1, id)
	}

	// Dropping jwk_set_json is in place too: back to discovery.
	tf.writeConfig(t, fcTFConfig(projects, 0, issuer, "repo:org/app:environment:prod", "infra", ""))
	tf.expectPlan(t, map[string]string{"sa0": "no-op", "sa1": "no-op", "fc": "update", "by_id": "read", "by_name": "read"})
	tf.apply(t)
	tf.expectNoChanges(t)
	fcTFCheck(t, svc, tf, projects[0], "repo:org/app:environment:prod", "infra", "")

	// Replace: parent_id moves to the second project (with its own service account).
	tf.writeConfig(t, fcTFConfig(projects, 1, issuer, "repo:org/app:environment:prod", "infra", ""))
	tf.expectPlan(t, map[string]string{"sa0": "no-op", "sa1": "no-op", "fc": "replace", "by_id": "read", "by_name": "read"})
	tf.apply(t)
	tf.expectNoChanges(t)
	id2 := fcTFCheck(t, svc, tf, projects[1], "repo:org/app:environment:prod", "infra", "")
	if id2 == id1 {
		t.Fatalf("replace kept the id %s", id1)
	}
	if _, err := svc.Get(t.Context(), &iampb.GetFederatedCredentialsRequest{Id: id1}); status.Code(err) != codes.NotFound {
		t.Errorf("old credentials Get after replace: %v, want NotFound", err)
	}

	tf.destroy(t)
	if _, err := svc.Get(t.Context(), &iampb.GetFederatedCredentialsRequest{Id: id2}); status.Code(err) != codes.NotFound {
		t.Errorf("Get after destroy: %v, want NotFound", err)
	}
}
