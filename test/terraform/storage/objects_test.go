//go:build e2e_storage

package storage

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"

	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"
	iamv2pb "github.com/nebius/gosdk/proto/nebius/iam/v2"
	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"
	iamsvc "github.com/nebius/gosdk/services/nebius/iam/v1"
	iamv2svc "github.com/nebius/gosdk/services/nebius/iam/v2"
	storagesvc "github.com/nebius/gosdk/services/nebius/storage/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/services/storage"
	"github.com/kfirgollan/NebiusMocker/test/internal/tfcache"
	"github.com/kfirgollan/NebiusMocker/test/rustfsfixture"
)

// TestMain removes the private Terraform plugin cache and the RustFS container.
func TestMain(m *testing.M) {
	code := tfcache.Main(m)
	rustfsfixture.Cleanup()
	os.Exit(code)
}

// credentialsConfig creates, with the nebius provider only, everything an S3
// client needs: a service account with editor on the project (group +
// membership + permit), an access key for it, and a versioned bucket.
func credentialsConfig(tenant, project, suffix, bucket, saDescription string) string {
	return fmt.Sprintf(`
resource "nebius_iam_v1_service_account" "s3" {
  parent_id   = %[2]q
  name        = "s3-writer-%[3]s"
  description = %[5]q
}

resource "nebius_iam_v1_group" "writers" {
  parent_id = %[1]q
  name      = "s3-writers-%[3]s"
}

resource "nebius_iam_v1_group_membership" "s3" {
  parent_id = nebius_iam_v1_group.writers.id
  member_id = nebius_iam_v1_service_account.s3.id
}

resource "nebius_iam_v1_access_permit" "edit_project" {
  parent_id   = nebius_iam_v1_group.writers.id
  resource_id = %[2]q
  role        = "editor"
}

resource "nebius_iam_v2_access_key" "s3" {
  parent_id            = %[2]q
  description          = "terraform s3 key"
  secret_delivery_mode = "EXPLICIT"
  account = {
    service_account = { id = nebius_iam_v1_service_account.s3.id }
  }
}

resource "nebius_storage_v1_bucket" "data" {
  parent_id         = %[2]q
  name              = %[4]q
  versioning_policy = "ENABLED"
}

data "nebius_iam_v1_service_account" "s3" {
  id = nebius_iam_v1_service_account.s3.id
}

data "nebius_iam_v2_access_key" "s3" {
  id = nebius_iam_v2_access_key.s3.id
}

output "sa_by_data"        { value = data.nebius_iam_v1_service_account.s3.name }
output "key_by_data"       { value = data.nebius_iam_v2_access_key.s3.status.aws_access_key_id }
output "access_key_id"     { value = nebius_iam_v2_access_key.s3.id }
output "aws_access_key_id" { value = nebius_iam_v2_access_key.s3.status.aws_access_key_id }
output "service_account"   { value = nebius_iam_v1_service_account.s3.id }
output "bucket_id"         { value = nebius_storage_v1_bucket.data.id }
`, tenant, project, suffix, bucket, saDescription)
}

// objectsConfig manages an object with the unmodified hashicorp/aws provider,
// configured as Nebius documents for S3 tools, with the secret read by the
// nebius_iam_v2_access_key_secret ephemeral resource (never stored in state).
func objectsConfig(nebiusAddr, token, s3Addr, accessKeyID, bucket, project, content, extra string) string {
	extraObject, extraDep := "", ""
	if extra != "" {
		extraDep = ", aws_s3_object.extra"
		extraObject = fmt.Sprintf(`
resource "aws_s3_object" "extra" {
  bucket  = %q
  key     = "extra/second.txt"
  content = %q
}
`, bucket, extra)
	}
	return fmt.Sprintf(`terraform {
  required_providers {
    nebius = {
      source  = "nebius/nebius"
      version = "= %[1]s"
    }
    aws = {
      source  = "hashicorp/aws"
      version = "= %[2]s"
    }
  }
}

provider "nebius" {
  resolvers       = { "*" = %[3]q }
  address_options = { "*" = { insecure = true } }
  token           = %[4]q
}

ephemeral "nebius_iam_v2_access_key_secret" "s3" {
  id = %[6]q
}

provider "aws" {
  region     = "eu-north1"
  access_key = ephemeral.nebius_iam_v2_access_key_secret.s3.aws_access_key_id
  secret_key = ephemeral.nebius_iam_v2_access_key_secret.s3.secret

  # Nebius's documented settings for S3 tools (endpoint = the mocker's S3 front).
  endpoints {
    s3 = "http://%[5]s"
  }
  s3_use_path_style           = true
  skip_credentials_validation = true
  skip_region_validation      = true
  skip_requesting_account_id  = true
  skip_metadata_api_check     = true
}

resource "aws_s3_object" "hello" {
  bucket       = %[7]q
  key          = "greetings/hello.txt"
  content      = %[8]q
  content_type = "text/plain"
}

data "aws_s3_object" "hello" {
  bucket = aws_s3_object.hello.bucket
  key    = aws_s3_object.hello.key
}

output "body" { value = data.aws_s3_object.hello.body }
%[9]s
# Read after the objects, so status.counters reflects them.
data "nebius_storage_v1_bucket" "counted" {
  parent_id  = %[10]q
  name       = %[7]q
  depends_on = [aws_s3_object.hello%[11]s]
}

output "counters" { value = data.nebius_storage_v1_bucket.counted.status.counters }
`, nebiusProviderVersion, awsProviderVersion, nebiusAddr, token, s3Addr, accessKeyID, bucket, content, extraObject, project, extraDep)
}

// TestObjectsWithAWSProvider is the Terraform done-criterion for objects:
// credentials and bucket via nebius/nebius, then create → read back (data
// source and AWS SDK) → no drift → update content → destroy via hashicorp/aws
// through the mocker's S3 front, backed by RustFS.
func TestObjectsWithAWSProvider(t *testing.T) {
	srv := rustfsfixture.Start(t)
	mod := storage.New(storage.WithS3Listen("127.0.0.1:0"))
	inst := testkit.StartT(t, []mocker.Module{iam.New(), mod}, mockerOptions(
		testkit.WithObjStore(srv.Client(t)), testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce),
		testkit.WithOperationDelay(100*time.Millisecond))...)
	sdk := inst.SDK(t)
	suffix := randomSuffix() // unique names across runs sharing a RustFS
	bucket := "tf-objects-" + suffix

	// 1. Credentials + bucket with the nebius provider.
	creds := newTerraform(t)
	creds.write(t, nebiusProvider(inst.Addr, inst.Token)+credentialsConfig(inst.TenantID, inst.ProjectID, suffix, bucket, "v1"))
	creds.init(t)
	creds.expectPlan(t, map[string]string{
		"nebius_iam_v1_service_account.s3": "create", "nebius_iam_v1_group.writers": "create",
		"nebius_iam_v1_group_membership.s3": "create", "nebius_iam_v1_access_permit.edit_project": "create",
		"nebius_iam_v2_access_key.s3": "create", "nebius_storage_v1_bucket.data": "create",
	})
	creds.apply(t)
	creds.expectNoChanges(t)
	keyNID, _ := creds.output(t, "access_key_id").(string)
	awsKeyID, _ := creds.output(t, "aws_access_key_id").(string)
	saID, _ := creds.output(t, "service_account").(string)
	if got := creds.output(t, "sa_by_data"); got != "s3-writer-"+suffix {
		t.Fatalf("data.nebius_iam_v1_service_account name = %v", got)
	}
	if got := creds.output(t, "key_by_data"); got != awsKeyID || awsKeyID == "" {
		t.Fatalf("data.nebius_iam_v2_access_key aws id = %v, want %q", got, awsKeyID)
	}

	// In-place update of the service account (description) and no drift after.
	creds.write(t, nebiusProvider(inst.Addr, inst.Token)+credentialsConfig(inst.TenantID, inst.ProjectID, suffix, bucket, "v2"))
	creds.expectPlan(t, map[string]string{
		"nebius_iam_v1_service_account.s3": "update", "nebius_iam_v1_group.writers": "no-op",
		"nebius_iam_v1_group_membership.s3": "no-op", "nebius_iam_v1_access_permit.edit_project": "no-op",
		"nebius_iam_v2_access_key.s3": "no-op", "nebius_storage_v1_bucket.data": "no-op",
	})
	creds.apply(t)
	creds.expectNoChanges(t)
	sa, err := iamsvc.NewServiceAccountService(sdk).Get(t.Context(), &iampb.GetServiceAccountRequest{Id: saID})
	if err != nil || sa.GetSpec().GetDescription() != "v2" {
		t.Fatalf("service account after update = %v, %v", sa, err)
	}
	if v, err := srv.Client(t).Versioning(t.Context(), bucket); err != nil || v != objstore.VersioningEnabled {
		t.Fatalf("RustFS versioning of %s = %v, %v; want enabled", bucket, v, err)
	}

	// An S3 client with the same key, to check what Terraform did.
	sec, err := iamv2svc.NewAccessKeyService(sdk).GetSecret(t.Context(), &iamv2pb.GetAccessKeySecretRequest{Id: keyNID})
	if err != nil || sec.GetAwsAccessKeyId() != awsKeyID {
		t.Fatalf("GetSecret = %v, %v", sec, err)
	}
	s3c := s3.New(s3.Options{
		Region: "eu-north1", BaseEndpoint: aws.String("http://" + mod.S3Addr()), UsePathStyle: true,
		Credentials: credentials.NewStaticCredentialsProvider(awsKeyID, sec.GetSecret(), ""),
	})
	readObject := func() (string, error) {
		out, err := s3c.GetObject(t.Context(), &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String("greetings/hello.txt")})
		if err != nil {
			return "", err
		}
		defer func() { _ = out.Body.Close() }() // read fully below
		b, err := io.ReadAll(out.Body)
		return string(b), err
	}

	// 2. Objects with the aws provider: create, read back, update, no drift.
	objects := newTerraform(t)
	for i, content := range []string{"hello from terraform", "hello again"} {
		objects.write(t, objectsConfig(inst.Addr, inst.Token, mod.S3Addr(), keyNID, bucket, inst.ProjectID, content, ""))
		action := "update"
		if i == 0 {
			objects.init(t)
			action = "create"
		}
		objects.expectPlan(t, map[string]string{"aws_s3_object.hello": action})
		objects.apply(t)
		if got := objects.output(t, "body"); got != content {
			t.Fatalf("step %d: data source body = %v, want %q", i, got, content)
		}
		if got, err := readObject(); err != nil || got != content {
			t.Fatalf("step %d: S3 GetObject = %q, %v; want %q", i, got, err, content)
		}
		objects.expectNoChanges(t)
	}

	// 2b. Bucket status.counters follow the objects: add one, then remove it.
	standard := func() (quantity, size int64) {
		t.Helper()
		list, _ := objects.output(t, "counters").([]any)
		for _, e := range list {
			m, _ := e.(map[string]any)
			if m["storage_class"] != "STANDARD" {
				continue
			}
			cur, _ := m["counters"].(map[string]any)
			q, _ := cur["simple_objects_quantity"].(float64)
			sz, _ := cur["simple_objects_size"].(float64)
			return int64(q), int64(sz)
		}
		t.Fatalf("no STANDARD entry in counters output %v", list)
		return 0, 0
	}
	const hello, second = "hello again", "0123456789"
	if q, sz := standard(); q != 1 || sz != int64(len(hello)) {
		t.Fatalf("counters after create+update = %d objects / %d bytes, want 1 / %d", q, sz, len(hello))
	}
	objects.write(t, objectsConfig(inst.Addr, inst.Token, mod.S3Addr(), keyNID, bucket, inst.ProjectID, hello, second))
	objects.expectPlan(t, map[string]string{"aws_s3_object.hello": "no-op", "aws_s3_object.extra": "create"})
	objects.apply(t)
	if q, sz := standard(); q != 2 || sz != int64(len(hello)+len(second)) {
		t.Fatalf("counters after adding an object = %d / %d, want 2 / %d", q, sz, len(hello)+len(second))
	}
	objects.expectNoChanges(t)
	objects.write(t, objectsConfig(inst.Addr, inst.Token, mod.S3Addr(), keyNID, bucket, inst.ProjectID, hello, ""))
	objects.expectPlan(t, map[string]string{"aws_s3_object.hello": "no-op", "aws_s3_object.extra": "delete"})
	objects.apply(t)
	// The removed object no longer exists in the config, so nothing orders the
	// data source after its deletion: refresh re-reads it.
	if err := objects.tf.Refresh(t.Context()); err != nil {
		t.Fatalf("terraform refresh: %v", err)
	}
	if q, sz := standard(); q != 1 || sz != int64(len(hello)) {
		t.Fatalf("counters after removing an object = %d / %d, want 1 / %d", q, sz, len(hello))
	}
	objects.expectNoChanges(t)

	// 3. Destroy the object, then the credentials and the bucket.
	objects.destroy(t)
	if _, err := readObject(); s3Code(err) != "NoSuchKey" {
		t.Fatalf("GetObject after destroy: %v, want NoSuchKey", err)
	}
	creds.destroy(t)
	b, err := storagesvc.NewBucketService(sdk).GetByName(t.Context(), &storagepb.GetBucketByNameRequest{ParentId: inst.ProjectID, Name: bucket})
	if err != nil || b.GetStatus().GetState() != storagepb.BucketStatus_SCHEDULED_FOR_DELETION {
		t.Fatalf("bucket after destroy = %v, %v; want SCHEDULED_FOR_DELETION", b, err)
	}
	if _, err := readObject(); s3Code(err) != "InvalidAccessKeyId" {
		t.Fatalf("S3 with the destroyed key: %v, want InvalidAccessKeyId", err)
	}
}

func s3Code(err error) string {
	if ae, ok := errors.AsType[smithy.APIError](err); ok {
		return ae.ErrorCode()
	}
	return ""
}

func randomSuffix() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b) // crypto/rand.Read does not fail
	return hex.EncodeToString(b)
}
