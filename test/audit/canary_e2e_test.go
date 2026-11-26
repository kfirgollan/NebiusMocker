//go:build e2e

package audit_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/genproto/googleapis/rpc/code"
	"google.golang.org/protobuf/encoding/protojson"

	auditv2pb "github.com/nebius/gosdk/proto/nebius/audit/v2"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	auditmod "github.com/kfirgollan/NebiusMocker/services/audit"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/test/internal/nebiuscli"
)

// TestCanaryKeyCLI is the canary-key detection workflow, all with the
// official nebius CLI: the security team plants a service account key and
// deactivates it, so it is never valid. Whoever tries to use it, through a
// profile made from the credentials file or from the key ID and private key
// file, is refused, and the team finds the attempt with
// "nebius audit v2 audit-event list" filtered by the key ID.
func TestCanaryKeyCLI(t *testing.T) {
	inst := testkit.StartT(t, []mocker.Module{iam.New(), auditmod.New()},
		testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce))
	admin := nebiuscli.New(t, nebiuscli.Profile{Addr: inst.Addr, Token: inst.Token, ParentID: inst.ProjectID, TenantID: inst.TenantID})

	// Plant the canary.
	var sa struct {
		Metadata struct {
			ID string `json:"id"`
		} `json:"metadata"`
	}
	admin.JSON(t, &sa, "iam", "service-account", "create", "--name", "canary-holder")
	credsFile := filepath.Join(t.TempDir(), "canary-credentials.json")
	// --output=<file>: CLI 0.12.312 mis-parses the space form before global flags.
	if res, err := admin.Run(t.Context(), "iam", "auth-public-key", "generate",
		"--service-account-id", sa.Metadata.ID, "--output="+credsFile); err != nil {
		t.Fatalf("auth-public-key generate: %v\n%s", err, res.Stderr)
	}
	creds := readCredentials(t, credsFile)
	if res, err := admin.Run(t.Context(), "iam", "auth-public-key", "deactivate", "--id", creds.KID); err != nil {
		t.Fatalf("auth-public-key deactivate: %v\n%s", err, res.Stderr)
	}
	pemFile := filepath.Join(t.TempDir(), "canary.pem")
	if err := os.WriteFile(pemFile, []byte(creds.PrivateKey), 0o600); err != nil {
		t.Fatal(err)
	}

	// The intruder, on another machine (its own CLI home), tries both
	// documented ways to use a service account key. Creating a service
	// account profile exchanges a token right away, so it fails.
	intruder := nebiuscli.New(t, nebiuscli.Profile{Addr: inst.Addr, Token: "intruder-without-a-token", ParentID: inst.ProjectID})
	attempts := [][]string{
		{
			"profile", "create", "stolen-file", "--endpoint", nebiuscli.Endpoint(t, inst.Addr),
			"--service-account-file", credsFile, "--parent-id", inst.ProjectID,
		},
		{
			"profile", "create", "stolen-key", "--endpoint", nebiuscli.Endpoint(t, inst.Addr),
			"--service-account-id", sa.Metadata.ID, "--public-key-id", creds.KID, "--private-key-file", pemFile,
			"--parent-id", inst.ProjectID,
		},
	}
	for _, args := range attempts {
		res, err := intruder.Run(t.Context(), args...)
		if err == nil {
			// Should the CLI defer the exchange, the first command makes it.
			res, err = intruder.Run(t.Context(), "iam", "whoami", "--profile", args[2])
		}
		if err == nil {
			t.Fatalf("%s with the canary key succeeded:\n%s", strings.Join(args, " "), res.Stdout)
		}
	}

	// The security team's query: the first event naming the canary key.
	now := clock.Real{}.Now().UTC()
	var resp auditv2pb.ListAuditEventResponse
	args := []string{
		"audit", "v2", "audit-event", "list", "--parent-id", inst.TenantID, "--region", region,
		"--start", now.Add(-time.Hour).Format(time.RFC3339), "--end", now.Add(time.Hour).Format(time.RFC3339),
		"--filter", fmt.Sprintf("resource.metadata.id = '%s' AND action = 'EXCHANGE'", creds.KID),
		"--format", "json",
	}
	res, err := admin.Run(t.Context(), args...)
	if err != nil {
		t.Fatalf("audit-event list: %v\n%s", err, res.Stderr)
	}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal([]byte(res.Stdout), &resp); err != nil {
		t.Fatalf("decode %q: %v", res.Stdout, err)
	}
	if len(resp.GetItems()) < len(attempts) {
		t.Fatalf("got %d events naming the canary key, want at least %d (one per attempt):\n%s",
			len(resp.GetItems()), len(attempts), res.Stdout)
	}
	for _, ev := range resp.GetItems() {
		switch {
		case ev.GetAuthentication().GetAuthenticated():
			t.Errorf("event %s: authenticated = true", ev.GetId())
		case ev.GetResponse().GetStatusCode() != code.Code_UNAUTHENTICATED:
			t.Errorf("event %s: status_code = %v, want UNAUTHENTICATED", ev.GetId(), ev.GetResponse().GetStatusCode())
		case !strings.Contains(ev.GetResponse().GetErrorMessage(), "INACTIVE"):
			t.Errorf("event %s: error_message = %q, want the key's state", ev.GetId(), ev.GetResponse().GetErrorMessage())
		case ev.GetAuthentication().GetSubject().GetServiceAccountId() != sa.Metadata.ID:
			t.Errorf("event %s: subject = %v, want %s", ev.GetId(), ev.GetAuthentication().GetSubject(), sa.Metadata.ID)
		case ev.GetRequest().GetClientIp() == "" || ev.GetRequest().GetUserAgent() == "":
			t.Errorf("event %s: client_ip %q user_agent %q, want both", ev.GetId(), ev.GetRequest().GetClientIp(), ev.GetRequest().GetUserAgent())
		}
		if strings.Contains(res.Stdout, "PRIVATE KEY") {
			t.Fatal("an event contains the private key")
		}
	}
	t.Logf("first canary event: %s by %s from %s (%s)", resp.GetItems()[0].GetTime().AsTime().Format(time.RFC3339),
		resp.GetItems()[0].GetAuthentication().GetSubject().GetServiceAccountId(),
		resp.GetItems()[0].GetRequest().GetClientIp(), resp.GetItems()[0].GetRequest().GetUserAgent())
}

type subjectCredentials struct {
	KID        string `json:"kid"`
	PrivateKey string `json:"private-key"`
}

func readCredentials(t *testing.T, path string) subjectCredentials {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("credentials file: %v", err)
	}
	var f struct {
		SubjectCredentials subjectCredentials `json:"subject-credentials"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("credentials file is not JSON: %v", err)
	}
	if !strings.HasPrefix(f.SubjectCredentials.KID, "publickey-") || f.SubjectCredentials.PrivateKey == "" {
		t.Fatalf("credentials file lacks kid or private-key: %s", data)
	}
	return f.SubjectCredentials
}
