//go:build e2e

// Package tunnel runs the official nebius CLI against NebiusMocker through the
// Tunnels flows the Nebius docs show (https://docs.nebius.com/tunnels): manage
// tunnels, then grant an agent's service account the applicationtunnel.agent
// role on one tunnel (https://docs.nebius.com/tunnels/connect). "nebius tunnel
// connect" is a client of Nebius's proprietary edge, which the mock does not
// provide, so it is not exercised (docs/services/tunnel.md).
package tunnel

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/services/tunnel"
	"github.com/kfirgollan/NebiusMocker/test/internal/nebiuscli"
)

type metadata struct {
	ID              string            `json:"id"`
	ParentID        string            `json:"parent_id"`
	Name            string            `json:"name"`
	ResourceVersion string            `json:"resource_version"`
	Labels          map[string]string `json:"labels"`
}

type tunnelResource struct {
	Metadata metadata `json:"metadata"`
	Spec     struct {
		Title       string `json:"title"`
		Description string `json:"description"`
	} `json:"spec"`
	Status struct {
		State           string `json:"state"`
		ConnectionState string `json:"connection_state"`
		Services        []any  `json:"services"`
	} `json:"status"`
}

type tunnelList struct {
	Items []tunnelResource `json:"items"`
}

type iamResource struct {
	Metadata metadata `json:"metadata"`
}

func start(t *testing.T) (*testkit.Instance, *nebiuscli.CLI) {
	t.Helper()
	inst := testkit.StartT(t, []mocker.Module{iam.New(), tunnel.New()},
		testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce), testkit.WithOperationDelay(100*time.Millisecond))
	cli := nebiuscli.New(t, nebiuscli.Profile{Addr: inst.Addr, Token: inst.Token, ParentID: inst.ProjectID, TenantID: inst.TenantID})
	return inst, cli
}

func run(t *testing.T, cli *nebiuscli.CLI, args ...string) {
	t.Helper()
	if _, err := cli.Run(t.Context(), args...); err != nil {
		t.Fatalf("%v", err)
	}
}

// denied runs a command that must fail with PermissionDenied.
func denied(t *testing.T, cli *nebiuscli.CLI, args ...string) {
	t.Helper()
	_, err := cli.Run(t.Context(), args...)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "permission") {
		t.Fatalf("nebius %s: err = %v, want PermissionDenied", strings.Join(args, " "), err)
	}
}

// TestTunnelFlows creates, reads, lists, updates and deletes tunnels.
func TestTunnelFlows(t *testing.T) {
	inst, cli := start(t)

	// nebius tunnel create --parent-id … --title … --description … --name … --labels …
	var tn tunnelResource
	cli.JSON(t, &tn, "tunnel", "create", "--parent-id", inst.ProjectID, "--name", "office", "--title", "Office",
		"--description", "the office network", "--labels", "env=dev")
	id := tn.Metadata.ID
	if id == "" || tn.Metadata.ParentID != inst.ProjectID || tn.Metadata.Name != "office" || tn.Spec.Title != "Office" ||
		tn.Spec.Description != "the office network" || tn.Metadata.Labels["env"] != "dev" ||
		tn.Status.State != "CREATED" || tn.Status.ConnectionState != "CONNECTION_STATE_DISCONNECTED" || len(tn.Status.Services) != 0 {
		t.Fatalf("tunnel create = %+v", tn)
	}

	// nebius tunnel get --id …
	var got tunnelResource
	cli.JSON(t, &got, "tunnel", "get", "--id", id)
	if got.Metadata.ID != id || got.Spec.Title != "Office" || got.Status.ConnectionState != "CONNECTION_STATE_DISCONNECTED" {
		t.Fatalf("tunnel get = %+v", got)
	}

	// nebius tunnel list --parent-id …
	var second tunnelResource
	cli.JSON(t, &second, "tunnel", "create", "--parent-id", inst.ProjectID, "--title", "Warehouse")
	var list tunnelList
	cli.JSON(t, &list, "tunnel", "list", "--parent-id", inst.ProjectID)
	if len(list.Items) != 2 {
		t.Fatalf("tunnel list = %+v, want 2 tunnels", list)
	}

	// nebius tunnel update --id … --title …: the CLI sends the current spec with the change.
	run(t, cli, "tunnel", "update", "--id", id, "--title", "HQ", "--description", "the main office",
		"--resource-version", got.Metadata.ResourceVersion)
	got = tunnelResource{}
	cli.JSON(t, &got, "tunnel", "get", "--id", id)
	if got.Spec.Title != "HQ" || got.Spec.Description != "the main office" || got.Metadata.Name != "office" ||
		got.Metadata.ResourceVersion == tn.Metadata.ResourceVersion {
		t.Fatalf("tunnel after update = %+v", got)
	}
	// A stale resource version is refused.
	if _, err := cli.Run(t.Context(), "tunnel", "update", "--id", id, "--title", "stale", "--resource-version", tn.Metadata.ResourceVersion); err == nil {
		t.Fatal("update with a stale resource version succeeded")
	}

	// nebius tunnel delete --id …
	run(t, cli, "tunnel", "delete", "--id", id)
	if _, err := cli.Run(t.Context(), "tunnel", "get", "--id", id); err == nil || !strings.Contains(err.Error(), "NotFound") {
		t.Fatalf("tunnel get after delete: err = %v, want NotFound", err)
	}
	list = tunnelList{}
	cli.JSON(t, &list, "tunnel", "list", "--parent-id", inst.ProjectID)
	if len(list.Items) != 1 || list.Items[0].Metadata.ID != second.Metadata.ID {
		t.Fatalf("tunnel list after delete = %+v", list)
	}
}

// TestAgentServiceAccount follows https://docs.nebius.com/tunnels/connect:
// a service account with an authorized key and the applicationtunnel.agent
// role on one tunnel reads that tunnel and nothing else.
func TestAgentServiceAccount(t *testing.T) {
	inst, cli := start(t)
	var tn, other tunnelResource
	cli.JSON(t, &tn, "tunnel", "create", "--parent-id", inst.ProjectID, "--title", "Office")
	cli.JSON(t, &other, "tunnel", "create", "--parent-id", inst.ProjectID, "--title", "Other")

	// nebius iam service-account create, then a group that holds the agent role on the tunnel.
	var sa, group iamResource
	cli.JSON(t, &sa, "iam", "service-account", "create", "--name", "tunnel-agent")
	cli.JSON(t, &group, "iam", "group", "create", "--parent-id", inst.TenantID, "--name", "tunnel-agents")
	run(t, cli, "iam", "access-permit", "create", "--parent-id", group.Metadata.ID, "--resource-id", tn.Metadata.ID,
		"--role", "applicationtunnel.agent")
	run(t, cli, "iam", "group-membership", "create", "--parent-id", group.Metadata.ID, "--member-id", sa.Metadata.ID)

	// openssl genrsa 4096, its public half registered with nebius iam auth-public-key create.
	privPEM, pubPEM := opensslKeyPair(t)
	var key iamResource
	cli.JSON(t, &key, "iam", "auth-public-key", "create", "--account-service-account-id", sa.Metadata.ID, "--data", pubPEM)
	if key.Metadata.ID == "" {
		t.Fatal("auth-public-key create returned no id")
	}
	credsFile := filepath.Join(t.TempDir(), "tunnel-agent-credentials.json")
	creds, err := json.Marshal(map[string]any{"subject-credentials": map[string]string{
		"type": "JWT", "alg": "RS256", "private-key": privPEM, "kid": key.Metadata.ID, "iss": sa.Metadata.ID, "sub": sa.Metadata.ID,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(credsFile, creds, 0o600); err != nil {
		t.Fatal(err)
	}
	const profile = "tunnel-agent"
	cli.AddServiceAccountProfile(t, profile, inst.Addr, credsFile, inst.ProjectID)

	// The agent reads its tunnel ...
	var got tunnelResource
	cli.JSON(t, &got, "tunnel", "get", "--id", tn.Metadata.ID, "--profile", profile)
	if got.Metadata.ID != tn.Metadata.ID || got.Spec.Title != "Office" {
		t.Fatalf("agent tunnel get = %+v", got)
	}
	// ... and nothing more: the role grants only applicationtunnel.tunnels.get on that tunnel.
	denied(t, cli, "tunnel", "update", "--id", tn.Metadata.ID, "--title", "hijacked", "--profile", profile)
	// A denied get reads as NotFound, so a caller cannot probe which tunnels exist.
	if _, err := cli.Run(t.Context(), "tunnel", "get", "--id", other.Metadata.ID, "--profile", profile); err == nil || !strings.Contains(err.Error(), "NotFound") {
		t.Fatalf("agent get of another tunnel: err = %v, want NotFound", err)
	}
	denied(t, cli, "tunnel", "delete", "--id", tn.Metadata.ID, "--profile", profile)
	denied(t, cli, "tunnel", "list", "--parent-id", inst.ProjectID, "--profile", profile)
}

// opensslKeyPair runs "openssl genrsa 4096" as the docs do and returns the
// private key as PKCS#8 PEM and the public key as PEM.
func opensslKeyPair(t *testing.T) (priv, pub string) {
	t.Helper()
	bin, err := exec.LookPath("openssl")
	if err != nil {
		t.Skip("openssl not found on PATH")
	}
	keyFile := filepath.Join(t.TempDir(), "private.pem")
	sh := func(args ...string) string {
		t.Helper()
		out, err := exec.CommandContext(t.Context(), bin, args...).Output()
		if err != nil {
			t.Fatalf("openssl %s: %v", strings.Join(args, " "), err)
		}
		return string(out)
	}
	sh("genrsa", "-out", keyFile, "4096")
	return sh("pkcs8", "-topk8", "-nocrypt", "-in", keyFile), sh("rsa", "-in", keyFile, "-pubout")
}
