//go:build e2e

// Package mysterybox runs the official nebius CLI against NebiusMocker through
// the MysteryBox (SecretStash) flows the Nebius docs show:
// https://docs.nebius.com/mysterybox/secrets.
package mysterybox

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/services/mysterybox"
	"github.com/kfirgollan/NebiusMocker/test/internal/nebiuscli"
)

type metadata struct {
	ID       string `json:"id"`
	ParentID string `json:"parent_id"`
	Name     string `json:"name"`
}

type secret struct {
	Metadata metadata `json:"metadata"`
	Spec     struct {
		Description      string `json:"description"`
		PrimaryVersionID string `json:"primary_version_id"`
	} `json:"spec"`
	Status struct {
		State string `json:"state"`
	} `json:"status"`
}

type version struct {
	Metadata metadata `json:"metadata"`
	Status   struct {
		State string `json:"state"`
	} `json:"status"`
}

type entry struct {
	Key         string `json:"key"`
	StringValue string `json:"string_value"`
	BinaryValue string `json:"binary_value"`
}

type payload struct {
	VersionID string  `json:"version_id"`
	Data      []entry `json:"data"`
}

// TestSecretFlows follows the docs: create a secret with a first version, add
// a version, make it primary, read payloads, delete and restore.
func TestSecretFlows(t *testing.T) {
	inst := testkit.StartT(t, []mocker.Module{iam.New(), mysterybox.New()},
		testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce), testkit.WithOperationDelay(200*time.Millisecond))
	cli := nebiuscli.New(t, nebiuscli.Profile{Addr: inst.Addr, Token: inst.Token, ParentID: inst.ProjectID, TenantID: inst.TenantID})
	bin := []byte{0x00, 0xff, 0x10, 0x80}
	binB64 := base64.StdEncoding.EncodeToString(bin)

	// nebius mysterybox secret create --name … --description … --secret-version-payload '[…]'
	var created secret
	cli.JSON(t, &created, "mysterybox", "secret", "create",
		"--name", "db-credentials", "--description", "database login",
		"--secret-version-payload", `[{"key": "user", "string_value": "admin"}, {"key": "cert", "binary_value": "`+binB64+`"}]`)
	secretID := created.Metadata.ID
	if !strings.HasPrefix(secretID, "mbsec-") || created.Spec.PrimaryVersionID == "" {
		t.Fatalf("secret create = %+v, want an mbsec ID with a primary version", created)
	}
	firstVersion := created.Spec.PrimaryVersionID

	// nebius mysterybox secret list
	if list := listSecrets(t, cli); len(list) != 1 || list[0].Metadata.Name != "db-credentials" {
		t.Fatalf("secret list = %+v, want db-credentials", list)
	}

	// nebius mysterybox payload get --secret-id … (primary version)
	p := getPayload(t, cli, secretID)
	assertPayload(t, p, firstVersion, map[string]string{"user": "admin", "cert": binB64})

	// nebius mysterybox secret-version create --parent-id … --payload '[…]'
	var v2 version
	cli.JSON(t, &v2, "mysterybox", "secret-version", "create", "--parent-id", secretID,
		"--description", "rotated", "--payload", `[{"key": "user", "string_value": "admin2"}]`)
	if v2.Metadata.ID == "" || v2.Metadata.ID == firstVersion {
		t.Fatalf("secret-version create = %+v, want a new version", v2)
	}

	// The CLI does not make new versions primary.
	p = getPayload(t, cli, secretID)
	if p.VersionID != firstVersion {
		t.Fatalf("payload get after version create read %s, want still %s", p.VersionID, firstVersion)
	}

	// nebius mysterybox secret-version list --parent-id …
	if vs := listVersions(t, cli, secretID); len(vs) != 2 {
		t.Fatalf("secret-version list = %+v, want 2 versions", vs)
	}

	// nebius mysterybox secret update <id> --primary-version-id …
	run(t, cli, "mysterybox", "secret", "update", secretID, "--primary-version-id", v2.Metadata.ID)
	p = getPayload(t, cli, secretID)
	assertPayload(t, p, v2.Metadata.ID, map[string]string{"user": "admin2"})

	// nebius mysterybox payload get --secret-id … --version-id …
	p = getPayload(t, cli, secretID, "--version-id", firstVersion)
	assertPayload(t, p, firstVersion, map[string]string{"user": "admin", "cert": binB64})

	// Version delete and undelete.
	run(t, cli, "mysterybox", "secret-version", "delete", firstVersion)
	if vs := listVersions(t, cli, secretID); len(vs) != 1 {
		t.Fatalf("secret-version list after delete = %+v, want 1 version", vs)
	}
	if vs := listVersions(t, cli, secretID, "--show-scheduled-for-deletion"); len(vs) != 2 {
		t.Fatalf("secret-version list --show-scheduled-for-deletion = %+v, want 2 versions", vs)
	}
	run(t, cli, "mysterybox", "secret-version", "undelete", "--id", firstVersion)

	// Secret delete and undelete.
	run(t, cli, "mysterybox", "secret", "delete", secretID)
	if list := listSecrets(t, cli); len(list) != 0 {
		t.Fatalf("secret list after delete = %+v, want none", list)
	}
	if list := listSecrets(t, cli, "--show-scheduled-for-deletion"); len(list) != 1 || list[0].Status.State != "SCHEDULED_FOR_DELETION" {
		t.Fatalf("secret list --show-scheduled-for-deletion = %+v, want the deleted secret", list)
	}
	run(t, cli, "mysterybox", "secret", "undelete", "--id", secretID)
	p = getPayload(t, cli, secretID)
	assertPayload(t, p, v2.Metadata.ID, map[string]string{"user": "admin2"})
}

// TestViewerCannotReadPayload checks the documented split: metadata readers
// do not get payloads.
func TestViewerCannotReadPayload(t *testing.T) {
	inst := testkit.StartT(t, []mocker.Module{iam.New(), mysterybox.New()},
		testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce))
	admin := nebiuscli.New(t, nebiuscli.Profile{Addr: inst.Addr, Token: inst.Token, ParentID: inst.ProjectID, TenantID: inst.TenantID})
	var created secret
	admin.JSON(t, &created, "mysterybox", "secret", "create", "--name", "s",
		"--secret-version-payload", `[{"key": "k", "string_value": "v"}]`)

	for _, token := range []string{config.DefaultViewerToken, config.DefaultEditorToken} {
		cli := nebiuscli.New(t, nebiuscli.Profile{Addr: inst.Addr, Token: token, ParentID: inst.ProjectID, TenantID: inst.TenantID})
		var got secret
		cli.JSON(t, &got, "mysterybox", "secret", "get", created.Metadata.ID)
		if got.Metadata.ID != created.Metadata.ID {
			t.Fatalf("%s: secret get = %+v", token, got)
		}
		res, err := cli.Run(t.Context(), "mysterybox", "payload", "get", "--secret-id", created.Metadata.ID)
		if err == nil {
			t.Fatalf("%s: payload get succeeded (%s), want permission denied", token, res.Stdout)
		}
		if strings.Contains(res.Stdout, `"v"`) {
			t.Fatalf("%s: payload leaked: %s", token, res.Stdout)
		}
	}
}

// listSecrets decodes into a fresh value: the CLI omits empty lists.
func listSecrets(t *testing.T, cli *nebiuscli.CLI, flags ...string) []secret {
	t.Helper()
	var out struct {
		Items []secret `json:"items"`
	}
	cli.JSON(t, &out, append([]string{"mysterybox", "secret", "list"}, flags...)...)
	return out.Items
}

func listVersions(t *testing.T, cli *nebiuscli.CLI, secretID string, flags ...string) []version {
	t.Helper()
	var out struct {
		Items []version `json:"items"`
	}
	cli.JSON(t, &out, append([]string{"mysterybox", "secret-version", "list", "--parent-id", secretID}, flags...)...)
	return out.Items
}

func getPayload(t *testing.T, cli *nebiuscli.CLI, secretID string, flags ...string) payload {
	t.Helper()
	var out payload
	cli.JSON(t, &out, append([]string{"mysterybox", "payload", "get", "--secret-id", secretID}, flags...)...)
	return out
}

func run(t *testing.T, cli *nebiuscli.CLI, args ...string) {
	t.Helper()
	if _, err := cli.Run(t.Context(), args...); err != nil {
		t.Fatal(err)
	}
}

// assertPayload compares entries by key; binary values are base64 text.
func assertPayload(t *testing.T, p payload, versionID string, want map[string]string) {
	t.Helper()
	if p.VersionID != versionID {
		t.Fatalf("payload version = %s, want %s", p.VersionID, versionID)
	}
	got := map[string]string{}
	for _, e := range p.Data {
		got[e.Key] = e.StringValue + e.BinaryValue
	}
	if len(got) != len(want) {
		t.Fatalf("payload entries = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("payload[%s] = %q, want %q", k, got[k], v)
		}
	}
}
