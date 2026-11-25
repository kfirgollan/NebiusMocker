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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/test/internal/nebiuscli"
)

type saAuthResource struct {
	Metadata struct {
		ID string `json:"id"`
	} `json:"metadata"`
	Spec struct {
		Description string `json:"description"`
	} `json:"spec"`
	Status struct {
		State       string `json:"state"`
		Algorithm   string `json:"algorithm"`
		KeySize     int    `json:"key_size"`
		Fingerprint string `json:"fingerprint"`
	} `json:"status"`
}

type saAuthList struct {
	Items []saAuthResource `json:"items"`
}

func (l saAuthList) contains(id string) bool {
	for _, it := range l.Items {
		if it.Metadata.ID == id {
			return true
		}
	}
	return false
}

// saAuthSetup creates a service account in the given group, generates a key
// for it with "iam auth-public-key generate" and adds a profile that uses the
// credentials file. The profile is named after the service account.
func saAuthSetup(t *testing.T, cli *nebiuscli.CLI, inst *testkit.Instance, group, name string) (sa, key saAuthResource) {
	t.Helper()
	cli.JSON(t, &sa, "iam", "service-account", "create", "--name", name)
	var g saAuthResource
	cli.JSON(t, &g, "iam", "group", "get-by-name", "--name", group, "--parent-id", inst.TenantID)
	if _, err := cli.Run(t.Context(), "iam", "group-membership", "create",
		"--parent-id", g.Metadata.ID, "--member-id", sa.Metadata.ID); err != nil {
		t.Fatalf("group-membership create: %v", err)
	}
	// The docs show "--output <file>"; CLI 0.12.312 mis-parses the space form
	// when global flags follow it (Run appends --insecure ...), so use --output=<file>.
	credsFile := filepath.Join(t.TempDir(), name+"-credentials.json")
	if _, err := cli.Run(t.Context(), "iam", "auth-public-key", "generate",
		"--service-account-id", sa.Metadata.ID, "--output="+credsFile); err != nil {
		t.Fatalf("auth-public-key generate: %v", err)
	}
	var creds struct {
		SubjectCredentials struct {
			KID string `json:"kid"`
			Iss string `json:"iss"`
			Sub string `json:"sub"`
		} `json:"subject-credentials"`
	}
	data, err := os.ReadFile(credsFile)
	if err != nil {
		t.Fatalf("credentials file: %v", err)
	}
	if err := json.Unmarshal(data, &creds); err != nil {
		t.Fatalf("credentials file is not JSON: %v\n%s", err, data)
	}
	sc := creds.SubjectCredentials
	if !strings.HasPrefix(sc.KID, "publickey-") || sc.Iss != sa.Metadata.ID || sc.Sub != sa.Metadata.ID {
		t.Fatalf("subject-credentials = %+v, want kid publickey-... and iss=sub=%s", sc, sa.Metadata.ID)
	}
	key.Metadata.ID = sc.KID
	cli.AddServiceAccountProfile(t, name, inst.Addr, credsFile, inst.ProjectID)
	return sa, key
}

func startSAAuth(t *testing.T) (*testkit.Instance, *nebiuscli.CLI) {
	t.Helper()
	inst := testkit.StartT(t, []mocker.Module{iam.New()}, testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce))
	cli := nebiuscli.New(t, nebiuscli.Profile{Addr: inst.Addr, Token: inst.Token, ParentID: inst.ProjectID, TenantID: inst.TenantID})
	return inst, cli
}

// TestSAAuthCLI follows the Nebius docs for service account authorized keys:
// generate and create keys, create a profile with --service-account-file and
// run commands as the service account, with authz enforced.
func TestSAAuthCLI(t *testing.T) {
	inst, cli := startSAAuth(t)
	const profile = "sa-auth-editor"
	sa, gen := saAuthSetup(t, cli, inst, "editors", profile)

	// A second key from a user-made pair: PEM PKIX public key, as openssl writes it.
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&rsaKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	pubPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
	sum := sha256.Sum256(der)
	wantFP := hex.EncodeToString(sum[:])

	var created saAuthResource
	cli.JSON(t, &created, "iam", "auth-public-key", "create",
		"--account-service-account-id", sa.Metadata.ID, "--data", pubPEM)
	if created.Metadata.ID == "" {
		t.Fatal("auth-public-key create returned no id")
	}
	var got saAuthResource
	cli.JSON(t, &got, "iam", "auth-public-key", "get", "--id", created.Metadata.ID)
	if got.Status.State != "ACTIVE" || got.Status.Algorithm != "RSA" || got.Status.KeySize != 2048 || got.Status.Fingerprint != wantFP {
		t.Fatalf("key status = %+v, want ACTIVE RSA 2048 fingerprint %s", got.Status, wantFP)
	}

	var byAcct, all saAuthList
	cli.JSON(t, &byAcct, "iam", "auth-public-key", "list-by-account", "--account-service-account-id", sa.Metadata.ID)
	cli.JSON(t, &all, "iam", "auth-public-key", "list", "--parent-id", inst.ProjectID)
	for _, id := range []string{gen.Metadata.ID, created.Metadata.ID} {
		if !byAcct.contains(id) {
			t.Fatalf("list-by-account missing %s: %+v", id, byAcct)
		}
		if !all.contains(id) {
			t.Fatalf("list missing %s: %+v", id, all)
		}
	}

	if _, err := cli.Run(t.Context(), "iam", "auth-public-key", "update", "--id", created.Metadata.ID, "--description", "rotated"); err != nil {
		t.Fatalf("update: %v", err)
	}
	cli.JSON(t, &got, "iam", "auth-public-key", "get", "--id", created.Metadata.ID)
	if got.Spec.Description != "rotated" {
		t.Fatalf("description = %q, want rotated", got.Spec.Description)
	}
	for _, step := range []struct{ verb, state string }{{"deactivate", "INACTIVE"}, {"activate", "ACTIVE"}} {
		if _, err := cli.Run(t.Context(), "iam", "auth-public-key", step.verb, "--id", created.Metadata.ID); err != nil {
			t.Fatalf("%s: %v", step.verb, err)
		}
		cli.JSON(t, &got, "iam", "auth-public-key", "get", "--id", created.Metadata.ID)
		if got.Status.State != step.state {
			t.Fatalf("state after %s = %q, want %s", step.verb, got.Status.State, step.state)
		}
	}

	// Commands as the service account.
	var who struct {
		ServiceAccountProfile struct {
			Info struct {
				Metadata struct {
					ID string `json:"id"`
				} `json:"metadata"`
			} `json:"info"`
		} `json:"service_account_profile"`
	}
	cli.JSON(t, &who, "iam", "whoami", "--profile", profile)
	if who.ServiceAccountProfile.Info.Metadata.ID != sa.Metadata.ID {
		t.Fatalf("whoami = %+v, want service account %s", who, sa.Metadata.ID)
	}
	res, err := cli.Run(t.Context(), "iam", "get-access-token", "--profile", profile)
	if err != nil {
		t.Fatalf("get-access-token: %v", err)
	}
	if !strings.HasPrefix(strings.TrimSpace(res.Stdout), "nmock.v1.") {
		t.Fatalf("access token = %q, want nmock.v1. prefix", res.Stdout)
	}
	var grp, adminGet saAuthResource
	cli.JSON(t, &grp, "iam", "group", "create", "--name", "made-by-sa", "--parent-id", inst.TenantID, "--profile", profile)
	cli.JSON(t, &adminGet, "iam", "group", "get", "--id", grp.Metadata.ID)
	if adminGet.Metadata.ID != grp.Metadata.ID {
		t.Fatalf("admin get of the SA-created group = %+v", adminGet)
	}

	// Deactivating the generated key locks the profile out; activating restores it.
	whoamiFails := func(when string) {
		t.Helper()
		_, err := cli.Run(t.Context(), "iam", "whoami", "--profile", profile)
		if err == nil {
			t.Fatalf("whoami %s succeeded", when)
		}
		if !isAuthFailure(err.Error()) {
			t.Fatalf("whoami %s: %v, want an authentication error", when, err)
		}
	}
	if _, err := cli.Run(t.Context(), "iam", "auth-public-key", "deactivate", "--id", gen.Metadata.ID); err != nil {
		t.Fatalf("deactivate generated key: %v", err)
	}
	whoamiFails("with a deactivated key")
	if _, err := cli.Run(t.Context(), "iam", "auth-public-key", "activate", "--id", gen.Metadata.ID); err != nil {
		t.Fatalf("activate generated key: %v", err)
	}
	if _, err := cli.Run(t.Context(), "iam", "whoami", "--profile", profile); err != nil {
		t.Fatalf("whoami after re-activation: %v", err)
	}
	if _, err := cli.Run(t.Context(), "iam", "auth-public-key", "delete", "--id", gen.Metadata.ID); err != nil {
		t.Fatalf("delete generated key: %v", err)
	}
	whoamiFails("with a deleted key")
}

// TestSAAuthViewerCLI: a service account in the viewers group may read but not write.
func TestSAAuthViewerCLI(t *testing.T) {
	inst, cli := startSAAuth(t)
	const profile = "sa-auth-viewer"
	saAuthSetup(t, cli, inst, "viewers", profile)

	if _, err := cli.Run(t.Context(), "iam", "group", "list", "--parent-id", inst.TenantID, "--profile", profile); err != nil {
		t.Fatalf("viewer group list: %v", err)
	}
	_, err := cli.Run(t.Context(), "iam", "group", "create", "--name", "viewer-made", "--parent-id", inst.TenantID, "--profile", profile)
	if err == nil {
		t.Fatal("viewer group create succeeded")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "permission") {
		t.Fatalf("viewer group create: %v, want PermissionDenied", err)
	}
}

func isAuthFailure(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "unauthenticated") || strings.Contains(m, "authenticat") || strings.Contains(m, "invalid_grant") || strings.Contains(m, "token exchange")
}
