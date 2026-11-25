//go:build e2e_storage

// Package iam runs the official nebius CLI against NebiusMocker through the
// IAM v2 access key flows the Nebius docs show, including secret delivery
// through MysteryBox (the LanceDB tutorial flow).
package iam

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/services/mysterybox"
	"github.com/kfirgollan/NebiusMocker/services/storage"
	"github.com/kfirgollan/NebiusMocker/test/internal/awscli"
	"github.com/kfirgollan/NebiusMocker/test/internal/nebiuscli"
	"github.com/kfirgollan/NebiusMocker/test/rustfsfixture"
)

func TestMain(m *testing.M) { os.Exit(rustfsfixture.Main(m)) }

type resource struct {
	Metadata struct {
		ID string `json:"id"`
	} `json:"metadata"`
	Status struct {
		AWSAccessKeyID    string `json:"aws_access_key_id"`
		Secret            string `json:"secret"`
		SecretReferenceID string `json:"secret_reference_id"`
	} `json:"status"`
}

// TestAccessKeyMysteryBoxDelivery follows the Nebius docs: create an access key
// with --secret-delivery-mode mystery_box, read the key ID from access-key get
// and the secret from the MysteryBox payload, then use both with the AWS CLI.
func TestAccessKeyMysteryBoxDelivery(t *testing.T) {
	aws := awscli.New(t)
	rustfs := rustfsfixture.Start(t)
	s3Mod := storage.New(storage.WithS3Listen("127.0.0.1:0"))
	inst := testkit.StartT(t, []mocker.Module{iam.New(), mysterybox.New(), s3Mod},
		testkit.WithObjStore(rustfs.Client(t)), testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce), testkit.WithOperationDelay(200*time.Millisecond))
	cli := nebiuscli.New(t, nebiuscli.Profile{Addr: inst.Addr, Token: inst.Token, ParentID: inst.ProjectID, TenantID: inst.TenantID})

	// A service account that may use Object Storage: member of the editors group.
	var sa, editors resource
	cli.JSON(t, &sa, "iam", "service-account", "create", "--name", "lancedb-sa")
	cli.JSON(t, &editors, "iam", "group", "get-by-name", "--name", "editors", "--parent-id", inst.TenantID)
	if _, err := cli.Run(t.Context(), "iam", "group-membership", "create",
		"--parent-id", editors.Metadata.ID, "--member-id", sa.Metadata.ID); err != nil {
		t.Fatalf("group-membership create: %v", err)
	}

	const bucket = "lancedb-data"
	cli.JSON(t, &resource{}, "storage", "bucket", "create", "--name", bucket)

	// nebius iam v2 access-key create … --secret-delivery-mode mystery_box
	var created resource
	cli.JSON(t, &created, "iam", "v2", "access-key", "create",
		"--parent-id", inst.ProjectID, "--account-service-account-id", sa.Metadata.ID,
		"--description", "LanceDB", "--secret-delivery-mode", "mystery_box")
	if created.Status.Secret != "" {
		t.Fatalf("access-key create returned the secret inline: %+v", created.Status)
	}
	if !strings.HasPrefix(created.Status.SecretReferenceID, "mbsec-") {
		t.Fatalf("secret_reference_id = %q, want an mbsec ID", created.Status.SecretReferenceID)
	}

	// AWS_ACCESS_KEY_ID: access-key get --format jsonpath='{.status.aws_access_key_id}'
	keyID := cliText(t, cli, "iam", "v2", "access-key", "get", "--id", created.Metadata.ID,
		"--format", "jsonpath={.status.aws_access_key_id}")
	if keyID == "" {
		t.Fatal("access-key get returned no aws_access_key_id")
	}
	var got resource
	cli.JSON(t, &got, "iam", "v2", "access-key", "get", "--id", created.Metadata.ID)
	if got.Status.Secret != "" || got.Status.SecretReferenceID != created.Status.SecretReferenceID {
		t.Fatalf("access-key get status = %+v, want no secret and the same reference", got.Status)
	}

	// AWS_SECRET_ACCESS_KEY: mysterybox payload get … '{.data[0].string_value}'
	payloadArgs := []string{
		"mysterybox", "payload", "get", "--secret-id", created.Status.SecretReferenceID,
		"--format", "jsonpath={.data[0].string_value}",
	}
	secret := cliText(t, cli, payloadArgs...)
	if len(secret) != 40 {
		t.Fatalf("secret from MysteryBox = %q (%d chars), want 40", secret, len(secret))
	}

	// The credentials work against Object Storage with the AWS CLI.
	aws.Configure(t, keyID, secret, "eu-north1", "http://"+s3Mod.S3Addr())
	src := filepath.Join(t.TempDir(), "table.lance")
	if err := os.WriteFile(src, []byte("lance data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := aws.Run(t.Context(), "s3", "cp", src, "s3://"+bucket+"/table.lance"); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "back.lance")
	if _, err := aws.Run(t.Context(), "s3", "cp", "s3://"+bucket+"/table.lance", dst); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(dst); err != nil || string(data) != "lance data" {
		t.Fatalf("downloaded object = %q, %v", data, err)
	}

	// Deleting the key leaves the MysteryBox secret in place.
	if _, err := cli.Run(t.Context(), "iam", "v2", "access-key", "delete", "--id", created.Metadata.ID); err != nil {
		t.Fatalf("access-key delete: %v", err)
	}
	if again := cliText(t, cli, payloadArgs...); again != secret {
		t.Fatalf("secret after key delete = %q, want %q", again, secret)
	}
	// …but the deleted key no longer authenticates S3 requests (aws s3 cp
	// starts with HeadObject, whose 403 has no body to carry an error code).
	if _, err := aws.Run(t.Context(), "s3", "cp", "s3://"+bucket+"/table.lance", dst); err == nil ||
		!strings.Contains(err.Error(), "(403)") {
		t.Fatalf("s3 cp with the deleted key: %v, want 403", err)
	}
}

// cliText runs the CLI and returns trimmed stdout.
func cliText(t *testing.T, cli *nebiuscli.CLI, args ...string) string {
	t.Helper()
	res, err := cli.Run(t.Context(), args...)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(res.Stdout)
}
