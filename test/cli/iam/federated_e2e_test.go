//go:build e2e

package iam

import (
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kfirgollan/NebiusMocker/test/internal/nebiuscli"
)

// fedMint asks the mocker's fake OIDC issuer for a JWT and writes it to a
// file, as `curl -d sub=… -o token.jwt <issuer>/token` would.
func fedMint(t *testing.T, issuer, sub string) string {
	t.Helper()
	form := url.Values{"sub": {sub}}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, issuer+"/token", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s/token: %v", issuer, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("POST %s/token: %d %s (%v)", issuer, resp.StatusCode, body, err)
	}
	path := filepath.Join(t.TempDir(), "token.jwt")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestFederatedCredentialsCLI follows the federated credentials flow with the
// Nebius CLI: create federated credentials for a service account, get a JWT
// from an OIDC issuer (the mocker's fake one), and use a profile created with
// --service-account-id and --federated-subject-credentials-file-path.
func TestFederatedCredentialsCLI(t *testing.T) {
	inst, cli := startSAAuth(t)
	_, port, err := net.SplitHostPort(inst.Addr)
	if err != nil {
		t.Fatal(err)
	}
	// The address users reach the mocker at, as the CLI endpoint uses.
	issuer := "http://localhost:" + port + "/_mocker/oidc/ci"

	var sa, g saAuthResource
	cli.JSON(t, &sa, "iam", "service-account", "create", "--name", "federated-ci")
	cli.JSON(t, &g, "iam", "group", "get-by-name", "--name", "viewers", "--parent-id", inst.TenantID)
	if _, err := cli.Run(t.Context(), "iam", "group-membership", "create", "--parent-id", g.Metadata.ID, "--member-id", sa.Metadata.ID); err != nil {
		t.Fatalf("group-membership create: %v", err)
	}
	var fc saAuthResource
	cli.JSON(t, &fc, "iam", "federated-credentials", "create", "--name", "ci", "--parent-id", inst.ProjectID,
		"--subject-id", sa.Metadata.ID, "--federated-subject-id", "ci-runner", "--oidc-provider-issuer-url", issuer)
	if !strings.HasPrefix(fc.Metadata.ID, "federatedcredentials-") {
		t.Fatalf("federated-credentials create = %+v", fc)
	}
	var listed struct {
		Items []saAuthResource `json:"items"`
	}
	cli.JSON(t, &listed, "iam", "federated-credentials", "list", "--parent-id", inst.ProjectID)
	if len(listed.Items) != 1 || listed.Items[0].Metadata.ID != fc.Metadata.ID {
		t.Fatalf("federated-credentials list = %+v", listed)
	}

	const profile = "federated"
	tokenFile := fedMint(t, issuer, "ci-runner")
	if _, err := cli.Run(t.Context(), "profile", "create", profile, "--endpoint", nebiuscli.Endpoint(t, inst.Addr),
		"--service-account-id", sa.Metadata.ID, "--federated-subject-credentials-file-path", tokenFile,
		"--parent-id", inst.ProjectID); err != nil {
		t.Fatalf("profile create: %v", err)
	}
	if _, err := cli.Run(t.Context(), "profile", "activate", nebiuscli.ProfileName); err != nil {
		t.Fatalf("profile activate: %v", err)
	}

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
	// The viewers membership applies to the federated identity: a list
	// succeeds, a write is denied.
	var sas struct {
		Items []saAuthResource `json:"items"`
	}
	cli.JSON(t, &sas, "iam", "service-account", "list", "--parent-id", inst.ProjectID, "--profile", profile)
	if len(sas.Items) == 0 {
		t.Fatal("service-account list as the federated profile returned nothing")
	}
	if _, err := cli.Run(t.Context(), "iam", "group", "create", "--name", "by-federated", "--parent-id", inst.TenantID, "--profile", profile); err == nil ||
		!strings.Contains(strings.ToLower(err.Error()), "permission") {
		t.Fatalf("group create as a viewer: %v, want permission denied", err)
	}

	// A profile whose token is for another subject cannot authenticate. (The
	// CLI caches exchanged tokens per profile, so this is a fresh profile.)
	_, err = cli.Run(t.Context(), "profile", "create", "federated-untrusted", "--endpoint", nebiuscli.Endpoint(t, inst.Addr),
		"--service-account-id", sa.Metadata.ID, "--federated-subject-credentials-file-path", fedMint(t, issuer, "someone-else"),
		"--parent-id", inst.ProjectID)
	if err == nil {
		_, err = cli.Run(t.Context(), "iam", "whoami", "--profile", "federated-untrusted")
	}
	if err == nil || !isAuthFailure(err.Error()) || !strings.Contains(err.Error(), "do not trust subject") {
		t.Fatalf("authenticating with an untrusted subject: %v, want an authentication error naming the subject", err)
	}
}
