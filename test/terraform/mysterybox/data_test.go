//go:build e2e

package mysterybox

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// payloadResources is a secret with an inline primary version (username, blob)
// plus a second, non-primary version (password).
func payloadResources(projectID string) string {
	return fmt.Sprintf(`
resource "nebius_mysterybox_v1_secret" "s" {
  parent_id = %q
  name      = "readable"
  labels    = { env = "test" }

  secret_version = {
    description = "inline"
    set_primary = true
    payload = [
      { key = "username", string_value = "alice" },
      { key = "blob", binary_value = "aGVsbG8=" }, # base64("hello")
    ]
  }
}

resource "nebius_mysterybox_v1_secret_version" "v2" {
  parent_id   = nebius_mysterybox_v1_secret.s.id
  description = "second"
  set_primary = false
  payload     = [{ key = "password", string_value = "hunter2" }]
}
`, projectID)
}

// TestDataSources reads secrets, versions and payload entries back through the
// official data sources. Data source values land in the Terraform state; that
// is expected for data sources.
func TestDataSources(t *testing.T) {
	e := newEnv(t)
	project := e.inst.ProjectID
	e.tf.writeConfig(t, payloadResources(project)+fmt.Sprintf(`
data "nebius_mysterybox_v1_secret" "by_id" {
  id = nebius_mysterybox_v1_secret.s.id
}

data "nebius_mysterybox_v1_secret" "by_name" {
  name      = nebius_mysterybox_v1_secret.s.name
  parent_id = %q
}

data "nebius_mysterybox_v1_secret_version" "second" {
  id = nebius_mysterybox_v1_secret_version.v2.id
}

# No version_id: the primary version is read.
data "nebius_mysterybox_v1_secret_payload_entry" "username" {
  secret_id = nebius_mysterybox_v1_secret.s.id
  key       = "username"
}

data "nebius_mysterybox_v1_secret_payload_entry" "blob" {
  secret_id = nebius_mysterybox_v1_secret.s.id
  key       = "blob"
}

data "nebius_mysterybox_v1_secret_payload_entry" "password" {
  secret_id  = nebius_mysterybox_v1_secret.s.id
  version_id = nebius_mysterybox_v1_secret_version.v2.id
  key        = "password"
}
`, project))
	e.tf.init(t)
	e.tf.expectPlan(t, map[string]string{"s": "create", "v2": "create"})
	e.tf.apply(t)
	ids := e.tf.stateIDs(t)
	e.assertSecrets(t, map[string]string{"readable": ids["s"]})
	if got := len(e.activeVersions(t, ids["s"])); got != 2 {
		t.Fatalf("mock holds %d versions, want 2", got)
	}
	e.tf.expectNoChanges(t)

	st := e.tf.state(t)
	for _, name := range []string{"by_id", "by_name"} {
		d := st["data.nebius_mysterybox_v1_secret."+name]
		if d == nil {
			t.Fatalf("data source %s missing from state; have %v", name, slicesOfKeys(st))
		}
		if d["id"] != ids["s"] || d["name"] != "readable" || d["parent_id"] != project {
			t.Errorf("secret data source %s: id/name/parent = %v/%v/%v", name, d["id"], d["name"], d["parent_id"])
		}
		if attr(t, d, "status.state") != "ACTIVE" {
			t.Errorf("secret data source %s: status.state = %v, want ACTIVE", name, attr(t, d, "status.state"))
		}
		if l, _ := d["labels"].(map[string]any); l["env"] != "test" {
			t.Errorf("secret data source %s: labels = %v", name, d["labels"])
		}
	}
	if got, want := st["data.nebius_mysterybox_v1_secret.by_id"]["primary_version_id"], e.primaryVersion(t, ids["s"]); got != want {
		t.Errorf("secret data source primary_version_id = %v, want the mock's %v", got, want)
	}

	v := st["data.nebius_mysterybox_v1_secret_version.second"]
	if v["id"] != ids["v2"] || v["parent_id"] != ids["s"] || v["description"] != "second" || attr(t, v, "status.state") != "ACTIVE" {
		t.Errorf("secret_version data source = %v", v)
	}

	entries := map[string]struct{ key, field, want string }{
		"username": {"username", "string_value", "alice"},
		"blob":     {"blob", "binary_value", "aGVsbG8="},
		"password": {"password", "string_value", "hunter2"},
	}
	for name, want := range entries {
		d := st["data.nebius_mysterybox_v1_secret_payload_entry."+name]
		if d == nil {
			t.Fatalf("payload entry data source %s missing from state", name)
		}
		if got := attr(t, d, "data.key"); got != want.key {
			t.Errorf("payload entry %s: data.key = %v, want %s", name, got, want.key)
		}
		if got := attr(t, d, "data."+want.field); got != want.want {
			t.Errorf("payload entry %s: data.%s = %v, want %s", name, want.field, got, want.want)
		}
	}
	if got := e.payloadStrings(t, ids["s"], ids["v2"]); !maps.Equal(got, map[string]string{"password": "hunter2"}) {
		t.Errorf("mock payload of v2 = %v", got)
	}

	e.tf.destroy(t)
	e.assertSecrets(t, map[string]string{})
	e.assertSecretSoftDeleted(t, ids["s"])
	e.assertVersionSoftDeleted(t, ids["v2"])
}

func slicesOfKeys[K comparable, V any](m map[K]V) []K {
	keys := make([]K, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// ephemeralChecks asserts, through terraform_data preconditions, the values an
// ephemeral resource returned. Ephemeral values never reach plan or state, so a
// failing precondition is the only observable: wrongUser flips one expectation
// so the negative case proves the checks really compare the mock's values.
func ephemeralChecks(secretID, primaryVersion, secondVersion, wrongUser string) string {
	return fmt.Sprintf(`
ephemeral "nebius_mysterybox_v1_secret_payload" "primary" {
  secret_id = %[1]q
}

ephemeral "nebius_mysterybox_v1_secret_payload" "second" {
  secret_id  = %[1]q
  version_id = %[3]q
}

ephemeral "nebius_mysterybox_v1_secret_payload_entry" "username" {
  secret_id = %[1]q
  key       = "username"
}

ephemeral "nebius_mysterybox_v1_secret_payload_entry" "blob" {
  secret_id  = %[1]q
  version_id = %[2]q
  key        = "blob"
}

ephemeral "nebius_mysterybox_v1_secret_payload_entry" "password" {
  secret_id  = %[1]q
  version_id = %[3]q
  key        = "password"
}

locals {
  primary = { for d in ephemeral.nebius_mysterybox_v1_secret_payload.primary.data : d.key => d }
  second  = { for d in ephemeral.nebius_mysterybox_v1_secret_payload.second.data : d.key => d }
}

resource "terraform_data" "payload" {
  lifecycle {
    precondition {
      condition     = ephemeral.nebius_mysterybox_v1_secret_payload.primary.version_id == %[2]q
      error_message = "ephemeral payload: wrong primary version id"
    }
    precondition {
      condition     = local.primary["username"].string_value == %[4]q && local.primary["blob"].binary_value == "aGVsbG8="
      error_message = "ephemeral payload: primary entries do not match the expected values"
    }
    precondition {
      condition     = length(local.primary) == 2 && length(local.second) == 1 && local.second["password"].string_value == "hunter2"
      error_message = "ephemeral payload: version_id selection returned the wrong entries"
    }
  }
}

resource "terraform_data" "entry" {
  lifecycle {
    precondition {
      condition     = ephemeral.nebius_mysterybox_v1_secret_payload_entry.username.data.string_value == %[4]q
      error_message = "ephemeral payload_entry: username does not match"
    }
    precondition {
      condition     = ephemeral.nebius_mysterybox_v1_secret_payload_entry.blob.data.binary_value == "aGVsbG8="
      error_message = "ephemeral payload_entry: blob does not match"
    }
    precondition {
      condition     = ephemeral.nebius_mysterybox_v1_secret_payload_entry.password.data.string_value == "hunter2"
      error_message = "ephemeral payload_entry: password of the explicit version does not match"
    }
  }
}
`, secretID, primaryVersion, secondVersion, wrongUser)
}

// TestEphemeralPayload reads payloads through the ephemeral resources
// nebius_mysterybox_v1_secret_payload and ..._payload_entry. Their values are
// observed via terraform_data preconditions; a deliberately wrong expectation
// must fail, and nothing may leak into the state.
func TestEphemeralPayload(t *testing.T) {
	e := newEnv(t)
	e.tf.requireVersion(t, 1, 10)
	e.tf.writeConfig(t, payloadResources(e.inst.ProjectID))
	e.tf.init(t)
	e.tf.expectPlan(t, map[string]string{"s": "create", "v2": "create"})
	e.tf.apply(t)
	ids := e.tf.stateIDs(t)
	primary := e.primaryVersion(t, ids["s"])
	resources := payloadResources(e.inst.ProjectID)

	// A wrong expectation must fail, naming the first failing precondition.
	e.tf.writeConfig(t, resources+ephemeralChecks(ids["s"], primary, ids["v2"], "mallory"))
	e.tf.expectApplyError(t, "primary entries do not match the expected values")
	e.tf.expectApplyError(t, "ephemeral payload_entry: username does not match")

	// The right expectations pass in plan and apply, and the plan is stable.
	e.tf.writeConfig(t, resources+ephemeralChecks(ids["s"], primary, ids["v2"], "alice"))
	e.tf.expectPlan(t, map[string]string{"s": "no-op", "v2": "no-op", "payload": "create", "entry": "create"})
	e.tf.apply(t)
	e.tf.expectNoChanges(t)
	for addr, vals := range e.tf.state(t) {
		if strings.HasPrefix(addr, "terraform_data.") {
			for k, v := range vals {
				if k != "id" && v != nil {
					t.Errorf("%s.%s holds %v: ephemeral values must not reach the state", addr, k, v)
				}
			}
		}
	}

	// A nonexistent version surfaces as a provider error rather than an empty value.
	e.tf.writeConfig(t, resources+ephemeralChecks(ids["s"], primary, "mbsecver-doesnotexist00000", "alice"))
	e.tf.expectApplyError(t, "mbsecver-doesnotexist00000")

	e.tf.writeConfig(t, resources)
	e.tf.destroy(t)
	e.assertSecrets(t, map[string]string{})
}

// TestRotationFlow is the realistic flow: a secret with an inline first
// version, a new version added and made primary, and the primary payload read
// back through an ephemeral resource before everything is destroyed.
func TestRotationFlow(t *testing.T) {
	e := newEnv(t)
	e.tf.requireVersion(t, 1, 10)
	project := e.inst.ProjectID
	secret := fmt.Sprintf(`
resource "nebius_mysterybox_v1_secret" "db" {
  parent_id = %q
  name      = "db-credentials"

  secret_version = {
    description = "v1"
    set_primary = true
    payload     = [{ key = "password", string_value = "v1-pass" }]
  }
}
`, project)
	rotated := `
resource "nebius_mysterybox_v1_secret_version" "v2" {
  parent_id   = nebius_mysterybox_v1_secret.db.id
  description = "v2"
  set_primary = true
  payload     = [{ key = "password", string_value = "v2-pass" }]
}
`
	// versionRef selects an explicit version; empty reads the primary version.
	// An ephemeral resource is opened during plan whenever its config is known,
	// so reading the primary while a new version is still to be created would
	// see the old primary: referencing the new version by id defers the read
	// to apply time instead.
	readPrimary := func(want, versionRef string) string {
		versionLine := ""
		if versionRef != "" {
			versionLine = "version_id = " + versionRef
		}
		return fmt.Sprintf(`
ephemeral "nebius_mysterybox_v1_secret_payload_entry" "password" {
  secret_id = nebius_mysterybox_v1_secret.db.id
  key       = "password"
  %s
}

resource "terraform_data" "primary_is_expected" {
  lifecycle {
    precondition {
      condition     = ephemeral.nebius_mysterybox_v1_secret_payload_entry.password.data.string_value == %q
      error_message = "primary payload is not the expected version"
    }
  }
}
`, versionLine, want)
	}

	// Phase 1: only v1 exists and is primary.
	e.tf.writeConfig(t, secret+readPrimary("v1-pass", ""))
	e.tf.init(t)
	e.tf.expectPlan(t, map[string]string{"db": "create", "primary_is_expected": "create"})
	e.tf.apply(t)
	id := e.tf.stateIDs(t)["db"]
	v1 := e.primaryVersion(t, id)
	e.tf.expectNoChanges(t)

	// Phase 2: add v2 as primary. The ephemeral reads v2 by id during apply,
	// and the secret itself is not planned for any change.
	e.tf.writeConfig(t, secret+rotated+readPrimary("v2-pass", "nebius_mysterybox_v1_secret_version.v2.id"))
	e.tf.expectPlan(t, map[string]string{"db": "no-op", "v2": "create", "primary_is_expected": "no-op"})
	e.tf.apply(t)
	v2 := e.tf.stateIDs(t)["v2"]
	if v2 == "" || v2 == v1 {
		t.Fatalf("v2 id = %q, v1 = %q", v2, v1)
	}
	if got := e.primaryVersion(t, id); got != v2 {
		t.Fatalf("mock primary version = %s, want v2 %s", got, v2)
	}
	if got := e.payloadStrings(t, id, v1); got["password"] != "v1-pass" {
		t.Fatalf("v1 payload = %v, want it kept", got)
	}
	e.tf.expectNoChanges(t)

	// Phase 3: with no version_id the ephemeral follows the primary, now v2.
	e.tf.writeConfig(t, secret+rotated+readPrimary("v2-pass", ""))
	e.tf.expectPlan(t, map[string]string{"db": "no-op", "v2": "no-op", "primary_is_expected": "no-op"})
	e.tf.apply(t)
	e.tf.expectNoChanges(t)

	// The old expectation must now fail: the ephemeral really tracks the primary.
	e.tf.writeConfig(t, secret+rotated+readPrimary("v1-pass", ""))
	e.tf.expectApplyError(t, "primary payload is not the expected version")

	// Rotate back by removing v2 from the config: v2 is soft-deleted and the
	// ephemeral is no longer asserted.
	e.tf.writeConfig(t, secret)
	e.tf.expectPlan(t, map[string]string{"db": "no-op", "v2": "delete", "primary_is_expected": "delete"})
	e.tf.apply(t)
	e.assertVersionSoftDeleted(t, v2)
	e.tf.expectNoChanges(t)

	e.tf.destroy(t)
	e.assertSecrets(t, map[string]string{})
	e.assertSecretSoftDeleted(t, id)
}

// TestWriteOnlyPayload uses the provider's write-only `sensitive` blocks, whose
// values must reach the mock but never the Terraform state. It needs
// Terraform >= 1.11.
func TestWriteOnlyPayload(t *testing.T) {
	e := newEnv(t)
	e.tf.requireVersion(t, 1, 11)
	project := e.inst.ProjectID
	description := "first"
	config := func(version, secretValue string) string {
		return fmt.Sprintf(`
resource "nebius_mysterybox_v1_secret" "s" {
  parent_id   = %[1]q
  name        = "write-only"
  description = %[4]q

  sensitive = {
    version = "1"
    secret_version = {
      payload = [{ key = "token", string_value = "inline-wo-secret" }]
    }
  }
}

resource "nebius_mysterybox_v1_secret_version" "v" {
  parent_id   = nebius_mysterybox_v1_secret.s.id
  set_primary = true

  sensitive = {
    version = %[2]q
    payload = [{ key = "token", string_value = %[3]q }]
  }
}
`, project, version, secretValue, description)
	}
	noPlaintext := func(values ...string) {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(e.tf.dir, "terraform.tfstate"))
		if err != nil {
			t.Fatalf("read state: %v", err)
		}
		for _, v := range values {
			if strings.Contains(string(raw), v) {
				t.Errorf("write-only value %q leaked into the Terraform state", v)
			}
		}
	}

	e.tf.writeConfig(t, config("1", "wo-secret-one"))
	e.tf.init(t)
	e.tf.expectPlan(t, map[string]string{"s": "create", "v": "create"})
	e.tf.apply(t)
	ids := e.tf.stateIDs(t)
	inline := e.activeVersions(t, ids["s"])
	if len(inline) != 2 {
		t.Fatalf("mock versions = %v, want the inline one and v", inline)
	}
	if got := e.payloadStrings(t, ids["s"], inline[0]); got["token"] != "inline-wo-secret" {
		t.Fatalf("mock payload of the first version = %v", got)
	}
	if got := e.payloadStrings(t, ids["s"], ids["v"]); got["token"] != "wo-secret-one" {
		t.Fatalf("mock payload of v = %v, want token=wo-secret-one", got)
	}
	noPlaintext("wo-secret-one", "inline-wo-secret")
	e.tf.expectNoChanges(t)

	// Bumping sensitive.version is how a changed write-only value is applied.
	e.tf.writeConfig(t, config("2", "wo-secret-two"))
	// The version resource's write-only fields are immutable, so it is replaced.
	e.tf.expectPlan(t, map[string]string{"s": "no-op", "v": "replace"})
	e.tf.apply(t)
	noPlaintext("wo-secret-one", "wo-secret-two", "inline-wo-secret")
	e.tf.expectNoChanges(t)
	if got := e.payloadStrings(t, ids["s"], ""); got["token"] != "wo-secret-two" {
		t.Fatalf("primary payload after version bump = %v, want token=wo-secret-two", got)
	}

	// An in-place update of a secret whose inline version is write-only: the
	// provider resends secret_version, and the mock must accept it unchanged.
	description = "second"
	e.tf.writeConfig(t, config("2", "wo-secret-two"))
	e.tf.expectPlan(t, map[string]string{"s": "update", "v": "no-op"})
	e.tf.apply(t)
	if got := e.getSecret(t, ids["s"]).GetSpec().GetDescription(); got != "second" {
		t.Fatalf("secret description after in-place update = %q, want second", got)
	}
	if got := e.payloadStrings(t, ids["s"], inline[0]); got["token"] != "inline-wo-secret" {
		t.Fatalf("inline payload after in-place update = %v", got)
	}
	noPlaintext("wo-secret-two", "inline-wo-secret")
	e.tf.expectNoChanges(t)

	e.tf.destroy(t)
	e.assertSecrets(t, map[string]string{})
	e.assertSecretSoftDeleted(t, ids["s"])
}
