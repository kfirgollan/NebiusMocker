//go:build e2e_storage && !e2e

package iam

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-exec/tfexec"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/services/mysterybox"
	"github.com/kfirgollan/NebiusMocker/services/storage"
	"github.com/kfirgollan/NebiusMocker/test/internal/tfcache"
	"github.com/kfirgollan/NebiusMocker/test/rustfsfixture"
)

// Pinned official provider releases under test.
const (
	nebiusProviderVersion = "0.6.91"
	awsProviderVersion    = "6.70.0"
)

// TestMain removes the private Terraform plugin cache and the RustFS container.
func TestMain(m *testing.M) {
	code := tfcache.Main(m)
	rustfsfixture.Cleanup()
	os.Exit(code)
}

const nebiusProviderBlock = `
terraform {
  required_providers {
    nebius = {
      source  = "nebius/nebius"
      version = "= %s"
    }
%s  }
}

provider "nebius" {
  resolvers       = { "*" = %q }
  address_options = { "*" = { insecure = true } }
  token           = %q
}
`

// mbCredsConfig creates, with the nebius provider only, what an S3 client
// needs: a service account with editor on the project (group + membership +
// permit), an access key delivered through MysteryBox, and a bucket.
func mbCredsConfig(addr, token, tenant, project, bucket string) string {
	return fmt.Sprintf(nebiusProviderBlock, nebiusProviderVersion, "", addr, token) + fmt.Sprintf(`
resource "nebius_iam_v1_service_account" "sa" {
  parent_id = %[2]q
  name      = "mb-writer"
}

resource "nebius_iam_v1_group" "writers" {
  parent_id = %[1]q
  name      = "mb-writers"
}

resource "nebius_iam_v1_group_membership" "member" {
  parent_id = nebius_iam_v1_group.writers.id
  member_id = nebius_iam_v1_service_account.sa.id
}

resource "nebius_iam_v1_access_permit" "edit_project" {
  parent_id   = nebius_iam_v1_group.writers.id
  resource_id = %[2]q
  role        = "editor"
}

resource "nebius_iam_v2_access_key" "key" {
  parent_id            = %[2]q
  secret_delivery_mode = "MYSTERY_BOX"
  account = {
    service_account = { id = nebius_iam_v1_service_account.sa.id }
  }
}

resource "nebius_storage_v1_bucket" "data" {
  parent_id = %[2]q
  name      = %[3]q
}

output "aws_access_key_id"   { value = nebius_iam_v2_access_key.key.status.aws_access_key_id }
output "secret_reference_id" { value = nebius_iam_v2_access_key.key.status.secret_reference_id }
`, tenant, project, bucket)
}

// mbReaderConfig has hashicorp/aws authenticate with the key from
// mbCredsConfig, reading the secret from MysteryBox through the documented
// ephemeral resource (it never reaches the state).
func mbReaderConfig(addr, token, s3Addr, awsKeyID, secretID, bucket string) string {
	aws := fmt.Sprintf("    aws = {\n      source  = \"hashicorp/aws\"\n      version = \"= %s\"\n    }\n", awsProviderVersion)
	return fmt.Sprintf(nebiusProviderBlock, nebiusProviderVersion, aws, addr, token) + fmt.Sprintf(`
ephemeral "nebius_mysterybox_v1_secret_payload_entry" "s3" {
  secret_id = %[2]q
  key       = "secret"
}

provider "aws" {
  region     = "eu-north1"
  access_key = %[1]q
  secret_key = ephemeral.nebius_mysterybox_v1_secret_payload_entry.s3.data.string_value

  # Nebius's documented settings for S3 tools (endpoint = the mocker's S3 front).
  endpoints {
    s3 = "http://%[3]s"
  }
  s3_use_path_style           = true
  skip_credentials_validation = true
  skip_region_validation      = true
  skip_requesting_account_id  = true
  skip_metadata_api_check     = true
}

resource "aws_s3_object" "hello" {
  bucket       = %[4]q
  key          = "delivered/hello.txt"
  content      = "hello via mysterybox"
  content_type = "text/plain"
}

data "aws_s3_object" "hello" {
  bucket = aws_s3_object.hello.bucket
  key    = aws_s3_object.hello.key
}

output "body" { value = data.aws_s3_object.hello.body }
`, awsKeyID, secretID, s3Addr, bucket)
}

// mbTF wraps one Terraform working directory, isolated from the developer's
// Terraform CLI config and AWS credentials.
type mbTF struct {
	tf  *tfexec.Terraform
	dir string
	out bytes.Buffer
}

func newMBTF(t *testing.T) *mbTF {
	t.Helper()
	bin, err := exec.LookPath("terraform")
	if err != nil {
		t.Skip("terraform not found on PATH")
	}
	dir := t.TempDir()
	tf, err := tfexec.NewTerraform(dir, bin)
	if err != nil {
		t.Fatalf("tfexec: %v", err)
	}
	rc := filepath.Join(dir, "terraform.rc")
	if err := os.WriteFile(rc, []byte("# intentionally empty: official registry only\n"), 0o600); err != nil {
		t.Fatalf("write CLI config: %v", err)
	}
	env := map[string]string{}
	for _, kv := range os.Environ() { //nolint:forbidigo // the terraform child process must inherit the developer's environment
		if k, v, ok := strings.Cut(kv, "="); ok && !strings.HasPrefix(k, "TF_") {
			env[k] = v
			if strings.HasPrefix(k, "AWS_") {
				env[k] = "" // only the provider block's credentials may authenticate
			}
		}
	}
	maps.Copy(env, map[string]string{
		"CHECKPOINT_DISABLE":          "1",
		"TF_CLI_CONFIG_FILE":          rc,
		"TF_REGISTRY_CLIENT_TIMEOUT":  "60",
		"NEBIUS_IAM_TOKEN":            "",
		"AWS_CONFIG_FILE":             filepath.Join(dir, "no-aws-config"),
		"AWS_SHARED_CREDENTIALS_FILE": filepath.Join(dir, "no-aws-credentials"),
	})
	maps.Copy(env, tfcache.Env(t))
	if err := tf.SetEnv(env); err != nil {
		t.Fatalf("tfexec env: %v", err)
	}
	r := &mbTF{tf: tf, dir: dir}
	tf.SetStdout(&r.out)
	tf.SetStderr(&r.out)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("terraform output (%s):\n%s", dir, r.out.String())
		}
	})
	return r
}

// applyConfig writes main.tf, initializes and applies it.
func (r *mbTF) applyConfig(t *testing.T, cfg string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(r.dir, "main.tf"), []byte(cfg), 0o600); err != nil {
		t.Fatalf("write main.tf: %v", err)
	}
	if err := r.tf.Init(t.Context()); err != nil {
		t.Fatalf("terraform init: %v", err)
	}
	if _, err := r.tf.Plan(t.Context(), tfexec.Out("tfplan")); err != nil {
		t.Fatalf("terraform plan: %v", err)
	}
	if err := r.tf.Apply(t.Context(), tfexec.DirOrPlan("tfplan")); err != nil {
		t.Fatalf("terraform apply: %v", err)
	}
}

// expectNoChanges asserts that a fresh plan is empty.
func (r *mbTF) expectNoChanges(t *testing.T) {
	t.Helper()
	changed, err := r.tf.Plan(t.Context())
	if err != nil {
		t.Fatalf("terraform plan: %v", err)
	}
	if changed {
		t.Fatalf("plan after apply is not empty: the mock's responses drift from the applied config")
	}
}

func (r *mbTF) destroy(t *testing.T) {
	t.Helper()
	if err := r.tf.Destroy(t.Context()); err != nil {
		t.Fatalf("terraform destroy: %v", err)
	}
}

// output returns a root output as a string.
func (r *mbTF) output(t *testing.T, name string) string {
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

// TestAccessKeyMysteryBoxS3 proves the key delivered through MysteryBox works
// end to end the documented way: Terraform reads the secret from MysteryBox
// with the ephemeral payload entry resource and hashicorp/aws uses it against
// the S3 front backed by RustFS.
func TestAccessKeyMysteryBoxS3(t *testing.T) {
	srv := rustfsfixture.Start(t)
	mod := storage.New(storage.WithS3Listen("127.0.0.1:0"))
	inst := testkit.StartT(t, []mocker.Module{iam.New(), mysterybox.New(), mod},
		testkit.WithObjStore(srv.Client(t)), testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce))
	bucket := "mb-delivery-" + strings.ToLower(rand.Text()[:8]) // unique across runs sharing a RustFS

	creds := newMBTF(t)
	creds.applyConfig(t, mbCredsConfig(inst.Addr, inst.Token, inst.TenantID, inst.ProjectID, bucket))
	creds.expectNoChanges(t)
	awsKeyID := creds.output(t, "aws_access_key_id")
	secretID := creds.output(t, "secret_reference_id")

	reader := newMBTF(t)
	reader.applyConfig(t, mbReaderConfig(inst.Addr, inst.Token, mod.S3Addr(), awsKeyID, secretID, bucket))
	if got := reader.output(t, "body"); got != "hello via mysterybox" {
		t.Fatalf("object body = %q, want %q", got, "hello via mysterybox")
	}
	reader.expectNoChanges(t)
	reader.destroy(t)
	creds.destroy(t)
}
