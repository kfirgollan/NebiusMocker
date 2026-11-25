//go:build e2e

package mysterybox

import (
	"fmt"
	"maps"
	"slices"
	"testing"
)

// secretConfig renders one nebius_mysterybox_v1_secret named "s" with an inline
// first version (a string entry and a binary entry).
func secretConfig(projectID, name, description, labels, versionDescription string) string {
	return fmt.Sprintf(`
resource "nebius_mysterybox_v1_secret" "s" {
  parent_id   = %q
  name        = %q
  description = %q
  labels      = { %s }

  secret_version = {
    description = %q
    set_primary = true
    payload = [
      { key = "username", string_value = "alice" },
      { key = "blob", binary_value = "aGVsbG8=" }, # base64("hello")
    ]
  }
}
`, projectID, name, description, labels, versionDescription)
}

// TestSecretLifecycle drives create → update in place → replace → destroy of
// nebius_mysterybox_v1_secret and checks, after every step, Terraform's plan
// and the mock's state (including the soft delete).
func TestSecretLifecycle(t *testing.T) {
	e := newEnv(t)
	project := e.inst.ProjectID

	// Step 1: create with an inline first version.
	e.tf.writeConfig(t, secretConfig(project, "app", "first", `team = "platform"`, "initial"))
	e.tf.init(t)
	e.tf.expectPlan(t, map[string]string{"s": "create"})
	e.tf.apply(t)
	id := e.tf.stateIDs(t)["s"]
	e.assertSecrets(t, map[string]string{"app": id})
	sec := e.getSecret(t, id)
	if sec.GetSpec().GetDescription() != "first" || sec.GetMetadata().GetLabels()["team"] != "platform" || sec.GetMetadata().GetParentId() != project {
		t.Fatalf("mock secret after create = %v", sec)
	}
	versions := e.activeVersions(t, id)
	if len(versions) != 1 {
		t.Fatalf("mock versions after create = %v, want exactly the inline one", versions)
	}
	firstVersion := versions[0]
	if got := e.getVersion(t, firstVersion).GetSpec().GetDescription(); got != "initial" {
		t.Fatalf("inline version description = %q, want initial", got)
	}
	if got, want := e.payloadStrings(t, id, ""), map[string]string{"username": "alice", "blob": "hello"}; !maps.Equal(got, want) {
		t.Fatalf("mock payload = %v, want %v", got, want)
	}
	if got := e.primaryVersion(t, id); got != firstVersion {
		t.Fatalf("primary version = %s, want the inline version %s", got, firstVersion)
	}
	e.tf.expectNoChanges(t)

	// Step 2: description, labels and name are updated in place (same ID, same version).
	e.tf.writeConfig(t, secretConfig(project, "app-renamed", "second", `team = "infra", tier = "1"`, "initial"))
	e.tf.expectPlan(t, map[string]string{"s": "update"})
	e.tf.apply(t)
	if got := e.tf.stateIDs(t)["s"]; got != id {
		t.Fatalf("secret recreated on in-place update: %s → %s", id, got)
	}
	e.assertSecrets(t, map[string]string{"app-renamed": id})
	sec = e.getSecret(t, id)
	if sec.GetSpec().GetDescription() != "second" || sec.GetMetadata().GetLabels()["tier"] != "1" || sec.GetMetadata().GetLabels()["team"] != "infra" {
		t.Fatalf("mock secret after update = %v", sec)
	}
	if got := e.activeVersions(t, id); !slices.Equal(got, versions) {
		t.Fatalf("versions after in-place update = %v, want unchanged %v", got, versions)
	}
	e.tf.expectNoChanges(t)

	// Step 3: removing a label clears it (reset mask).
	e.tf.writeConfig(t, secretConfig(project, "app-renamed", "second", `tier = "1"`, "initial"))
	e.tf.expectPlan(t, map[string]string{"s": "update"})
	e.tf.apply(t)
	if _, has := e.getSecret(t, id).GetMetadata().GetLabels()["team"]; has {
		t.Fatalf("removed label still on the mock secret: %v", e.getSecret(t, id).GetMetadata().GetLabels())
	}
	e.tf.expectNoChanges(t)

	// Step 4: the inline version spec is immutable, so changing it replaces the secret.
	e.tf.writeConfig(t, secretConfig(project, "app-renamed", "second", `tier = "1"`, "rotated"))
	e.tf.expectPlan(t, map[string]string{"s": "replace"})
	e.tf.apply(t)
	newID := e.tf.stateIDs(t)["s"]
	if newID == id {
		t.Fatalf("secret was not replaced: still %s", id)
	}
	e.assertSecrets(t, map[string]string{"app-renamed": newID})
	e.assertSecretSoftDeleted(t, id)
	if got := e.getVersion(t, e.activeVersions(t, newID)[0]).GetSpec().GetDescription(); got != "rotated" {
		t.Fatalf("replacement version description = %q, want rotated", got)
	}
	e.tf.expectNoChanges(t)

	// Step 5: destroy soft-deletes the secret.
	e.tf.destroy(t)
	if got := e.tf.stateIDs(t); len(got) != 0 {
		t.Fatalf("state after destroy = %v, want empty", got)
	}
	e.assertSecrets(t, map[string]string{})
	e.assertSecretSoftDeleted(t, newID)
}

// TestSecretVersionLifecycle covers nebius_mysterybox_v1_secret_version: every
// attribute of the provider schema is RequiresReplace, so there is no in-place
// update; a payload change replaces the version, and delete is a soft delete.
func TestSecretVersionLifecycle(t *testing.T) {
	e := newEnv(t)
	project := e.inst.ProjectID
	config := func(payloadValue, labels string) string {
		return fmt.Sprintf(`
resource "nebius_mysterybox_v1_secret" "s" {
  parent_id = %q
  name      = "versioned"
}

resource "nebius_mysterybox_v1_secret_version" "v" {
  parent_id   = nebius_mysterybox_v1_secret.s.id
  description = "managed by terraform"
  labels      = { %s }
  set_primary = true
  payload = [
    { key = "password", string_value = %q },
    { key = "blob", binary_value = "aGVsbG8=" },
  ]
}
`, project, labels, payloadValue)
	}

	// Step 1: a secret without inline version, then a primary version on top.
	e.tf.writeConfig(t, config("one", `stage = "a"`))
	e.tf.init(t)
	e.tf.expectPlan(t, map[string]string{"s": "create", "v": "create"})
	e.tf.apply(t)
	ids := e.tf.stateIDs(t)
	e.assertSecrets(t, map[string]string{"versioned": ids["s"]})
	if got := e.activeVersions(t, ids["s"]); !slices.Equal(got, []string{ids["v"]}) {
		t.Fatalf("mock versions = %v, want [%s]", got, ids["v"])
	}
	if got := e.primaryVersion(t, ids["s"]); got != ids["v"] {
		t.Fatalf("primary version = %s, want %s (set_primary)", got, ids["v"])
	}
	ver := e.getVersion(t, ids["v"])
	if ver.GetSpec().GetDescription() != "managed by terraform" || ver.GetMetadata().GetParentId() != ids["s"] || ver.GetMetadata().GetLabels()["stage"] != "a" {
		t.Fatalf("mock version = %v", ver)
	}
	if got, want := e.payloadStrings(t, ids["s"], ids["v"]), map[string]string{"password": "one", "blob": "hello"}; !maps.Equal(got, want) {
		t.Fatalf("mock payload = %v, want %v", got, want)
	}
	e.tf.expectNoChanges(t)

	// Step 2: a label change replaces the version (labels RequiresReplace); the
	// secret is untouched.
	e.tf.writeConfig(t, config("one", `stage = "b"`))
	e.tf.expectPlan(t, map[string]string{"s": "no-op", "v": "replace"})
	e.tf.apply(t)
	second := e.tf.stateIDs(t)
	if second["s"] != ids["s"] || second["v"] == ids["v"] {
		t.Fatalf("after label change: secret %s → %s (want same), version %s → %s (want new)", ids["s"], second["s"], ids["v"], second["v"])
	}
	e.assertVersionSoftDeleted(t, ids["v"])
	if got := e.primaryVersion(t, ids["s"]); got != second["v"] {
		t.Fatalf("primary version = %s, want the replacement %s", got, second["v"])
	}
	e.tf.expectNoChanges(t)

	// Step 3: a payload change also replaces it, and the new payload is served.
	e.tf.writeConfig(t, config("two", `stage = "b"`))
	e.tf.expectPlan(t, map[string]string{"s": "no-op", "v": "replace"})
	e.tf.apply(t)
	third := e.tf.stateIDs(t)
	if third["v"] == second["v"] {
		t.Fatalf("version was not replaced on payload change: %s", third["v"])
	}
	e.assertVersionSoftDeleted(t, second["v"])
	if got := e.payloadStrings(t, ids["s"], ""); got["password"] != "two" {
		t.Fatalf("primary payload = %v, want password=two", got)
	}
	e.tf.expectNoChanges(t)

	// Step 4: destroy soft-deletes the version and the secret.
	e.tf.destroy(t)
	if got := e.tf.stateIDs(t); len(got) != 0 {
		t.Fatalf("state after destroy = %v, want empty", got)
	}
	e.assertSecrets(t, map[string]string{})
	e.assertVersionSoftDeleted(t, third["v"])
	e.assertSecretSoftDeleted(t, ids["s"])
}
