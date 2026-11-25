//go:build e2e

// Package kms runs the official Terraform binary with the official
// nebius/nebius provider from the Terraform Registry against NebiusMocker's
// KMS service. Nothing is patched or forked.
//
// Run with: make e2e   (needs terraform on PATH and registry access on first run)
package kms

import (
	"fmt"
	"maps"
	"slices"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	kmspb "github.com/nebius/gosdk/proto/nebius/kms/v1"
	kmssvc "github.com/nebius/gosdk/services/nebius/kms/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/services/kms"
)

func startKMS(t *testing.T) *testkit.Instance {
	t.Helper()
	// A non-zero delay makes operations asynchronous, so the provider's
	// OperationService polling path is exercised too.
	return testkit.StartT(t, []mocker.Module{iam.New(), kms.New()},
		mockerOptions(testkit.WithOperationDelay(200*time.Millisecond), testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce))...)
}

func symmetricConfig(parent, algorithm, labels, description, rotation string) string {
	rot := ""
	if rotation != "" {
		rot = fmt.Sprintf("  rotation_period = %q\n", rotation)
	}
	return fmt.Sprintf(`
resource "nebius_kms_v1_symmetric_key" "k" {
  parent_id   = %q
  name        = "tf-sym"
  labels      = { %s }
  description = %q
  algorithm   = %q
%s}
`, parent, labels, description, algorithm, rot)
}

func getSym(t *testing.T, svc kmssvc.SymmetricKeyService, id string, scheduled bool) *kmspb.SymmetricKey {
	t.Helper()
	k, err := svc.Get(t.Context(), &kmspb.GetSymmetricKeyRequest{Id: id, ShowScheduledForDeletion: scheduled})
	if err != nil {
		t.Fatalf("Get symmetric key %s (scheduled=%v): %v", id, scheduled, err)
	}
	return k
}

func getAsym(t *testing.T, svc kmssvc.AsymmetricKeyService, id string, scheduled bool) *kmspb.AsymmetricKey {
	t.Helper()
	k, err := svc.Get(t.Context(), &kmspb.GetAsymmetricKeyRequest{Id: id, ShowScheduledForDeletion: scheduled})
	if err != nil {
		t.Fatalf("Get asymmetric key %s (scheduled=%v): %v", id, scheduled, err)
	}
	return k
}

func TestSymmetricKeyLifecycle(t *testing.T) {
	inst := startKMS(t)
	svc := kmssvc.NewSymmetricKeyService(inst.SDK(t))
	tf := newTerraform(t, inst)
	cfg := func(algorithm, labels, desc, rotation string) string {
		return symmetricConfig(inst.ProjectID, algorithm, labels, desc, rotation)
	}

	// Create with the default rotation period.
	tf.writeConfig(t, cfg("AES_256", `team = "sec"`, "first", ""))
	tf.init(t)
	tf.expectPlan(t, map[string]string{"k": "create"})
	tf.apply(t)
	id := tf.stateIDs(t)["k"]
	k := getSym(t, svc, id, false)
	if k.GetMetadata().GetName() != "tf-sym" || k.GetMetadata().GetParentId() != inst.ProjectID ||
		k.GetMetadata().GetLabels()["team"] != "sec" || k.GetSpec().GetDescription() != "first" ||
		k.GetSpec().GetAlgorithm() != kmspb.SymmetricAlgorithm_AES_256 ||
		k.GetStatus().GetState() != kmspb.KeyState_ACTIVE {
		t.Fatalf("mock key after create = %v", k)
	}
	if got := k.GetSpec().GetRotationPeriod().AsDuration(); got != 90*24*time.Hour {
		t.Fatalf("default rotation_period = %v, want 90 days", got)
	}
	attrs := tf.stateAttrs(t)["nebius_kms_v1_symmetric_key.k"]
	t.Logf("state rotation_period after create: %v", attrs["rotation_period"])
	tf.expectNoChanges(t)
	version := k.GetMetadata().GetResourceVersion()

	// In-place update, including the rotation period.
	tf.writeConfig(t, cfg("AES_256", `team = "platform", tier = "1"`, "second", "172800s"))
	tf.expectPlan(t, map[string]string{"k": "update"})
	tf.apply(t)
	if got := tf.stateIDs(t)["k"]; got != id {
		t.Fatalf("key recreated on update: %s -> %s", id, got)
	}
	k = getSym(t, svc, id, false)
	if k.GetMetadata().GetLabels()["team"] != "platform" || k.GetMetadata().GetLabels()["tier"] != "1" ||
		k.GetSpec().GetDescription() != "second" || k.GetSpec().GetRotationPeriod().AsDuration() != 48*time.Hour {
		t.Fatalf("mock key after update = %v", k)
	}
	if k.GetMetadata().GetResourceVersion() <= version {
		t.Errorf("resource_version = %d, want > %d", k.GetMetadata().GetResourceVersion(), version)
	}
	tf.expectNoChanges(t)

	// Changing only rotation_period is an in-place update that reschedules
	// automatic rotation; the key and its material stay.
	tf.writeConfig(t, cfg("AES_256", `team = "platform", tier = "1"`, "second", "2592000s"))
	tf.expectPlan(t, map[string]string{"k": "update"})
	tf.apply(t)
	if got := tf.stateIDs(t)["k"]; got != id {
		t.Fatalf("key recreated on rotation_period update: %s -> %s", id, got)
	}
	if got := getSym(t, svc, id, false).GetSpec().GetRotationPeriod().AsDuration(); got != 30*24*time.Hour {
		t.Fatalf("rotation_period after update = %v, want 720h", got)
	}
	tf.expectNoChanges(t)

	// A rotation outside Terraform (automatic or manual: both add a key
	// version and bump resource_version and updated_at) is not drift.
	before := getSym(t, svc, id, false).GetMetadata().GetResourceVersion()
	rot, err := svc.Rotate(t.Context(), &kmspb.RotateSymmetricKeyRequest{Id: id})
	if err == nil {
		_, err = rot.Wait(t.Context())
	}
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if got := getSym(t, svc, id, false).GetMetadata().GetResourceVersion(); got != before+1 {
		t.Fatalf("resource_version after rotation = %d, want %d", got, before+1)
	}
	tf.expectNoChanges(t)

	// Removing a label is applied too (reset mask).
	tf.writeConfig(t, cfg("AES_256", `tier = "1"`, "second", "172800s"))
	tf.expectPlan(t, map[string]string{"k": "update"})
	tf.apply(t)
	if _, has := getSym(t, svc, id, false).GetMetadata().GetLabels()["team"]; has {
		t.Fatalf("label team was not cleared")
	}
	tf.expectNoChanges(t)

	// Changing the algorithm is a replacement. The provider destroys first and
	// the mock rejects AES_128, so the apply fails after the old key is gone.
	tf.writeConfig(t, cfg("AES_128", `tier = "1"`, "second", "172800s"))
	tf.expectPlan(t, map[string]string{"k": "replace"})
	tf.expectApplyError(t, "eprecated")
	if got := getSym(t, svc, id, true).GetStatus().GetState(); got != kmspb.KeyState_SCHEDULED_FOR_DELETION {
		t.Fatalf("replaced key state = %v, want SCHEDULED_FOR_DELETION", got)
	}

	// Restore the config: a fresh AES_256 key is created, then destroyed.
	tf.writeConfig(t, cfg("AES_256", `tier = "1"`, "second", "172800s"))
	tf.expectPlan(t, map[string]string{"k": "create"})
	tf.apply(t)
	id2 := tf.stateIDs(t)["k"]
	if id2 == "" || id2 == id {
		t.Fatalf("restored key ID = %q, old %q", id2, id)
	}
	if got := getSym(t, svc, id2, false).GetStatus().GetState(); got != kmspb.KeyState_ACTIVE {
		t.Fatalf("restored key state = %v, want ACTIVE", got)
	}
	tf.expectNoChanges(t)
	id = id2
	tf.destroy(t)
	if got := tf.stateIDs(t); len(got) != 0 {
		t.Fatalf("state after destroy = %v, want empty", got)
	}
	if _, err := svc.Get(t.Context(), &kmspb.GetSymmetricKeyRequest{Id: id}); status.Code(err) != codes.NotFound {
		t.Fatalf("Get after destroy: code %v, want NotFound (%v)", status.Code(err), err)
	}
	if got := getSym(t, svc, id, true).GetStatus().GetState(); got != kmspb.KeyState_SCHEDULED_FOR_DELETION {
		t.Fatalf("state after destroy = %v, want SCHEDULED_FOR_DELETION", got)
	}
}

func asymmetricConfig(parent, algorithm, labels, description string) string {
	return fmt.Sprintf(`
resource "nebius_kms_v1_asymmetric_key" "k" {
  parent_id   = %q
  name        = "tf-asym"
  labels      = { %s }
  description = %q
  algorithm   = %q
}
`, parent, labels, description, algorithm)
}

func TestAsymmetricKeyLifecycle(t *testing.T) {
	inst := startKMS(t)
	svc := kmssvc.NewAsymmetricKeyService(inst.SDK(t))
	tf := newTerraform(t, inst)
	cfg := func(algorithm, labels, desc string) string {
		return asymmetricConfig(inst.ProjectID, algorithm, labels, desc)
	}

	tf.writeConfig(t, cfg("ECDSA_NIST_P256_SHA_256", `team = "sec"`, "first"))
	tf.init(t)
	tf.expectPlan(t, map[string]string{"k": "create"})
	tf.apply(t)
	id := tf.stateIDs(t)["k"]
	k := getAsym(t, svc, id, false)
	if k.GetMetadata().GetName() != "tf-asym" || k.GetMetadata().GetParentId() != inst.ProjectID ||
		k.GetSpec().GetAlgorithm() != kmspb.AsymmetricAlgorithm_ECDSA_NIST_P256_SHA_256 ||
		k.GetStatus().GetState() != kmspb.KeyState_ACTIVE {
		t.Fatalf("mock key after create = %v", k)
	}
	tf.expectNoChanges(t)
	version := k.GetMetadata().GetResourceVersion()

	tf.writeConfig(t, cfg("ECDSA_NIST_P256_SHA_256", `team = "platform"`, "second"))
	tf.expectPlan(t, map[string]string{"k": "update"})
	tf.apply(t)
	if got := tf.stateIDs(t)["k"]; got != id {
		t.Fatalf("key recreated on update: %s -> %s", id, got)
	}
	k = getAsym(t, svc, id, false)
	if k.GetMetadata().GetLabels()["team"] != "platform" || k.GetSpec().GetDescription() != "second" {
		t.Fatalf("mock key after update = %v", k)
	}
	if k.GetMetadata().GetResourceVersion() <= version {
		t.Errorf("resource_version = %d, want > %d", k.GetMetadata().GetResourceVersion(), version)
	}
	tf.expectNoChanges(t)

	// Algorithm change replaces the key.
	tf.writeConfig(t, cfg("ECDSA_NIST_P384_SHA_384", `team = "platform"`, "second"))
	tf.expectPlan(t, map[string]string{"k": "replace"})
	tf.apply(t)
	newID := tf.stateIDs(t)["k"]
	if newID == "" || newID == id {
		t.Fatalf("replace kept the ID: %s -> %s", id, newID)
	}
	if got := getAsym(t, svc, id, true).GetStatus().GetState(); got != kmspb.KeyState_SCHEDULED_FOR_DELETION {
		t.Fatalf("old key state = %v, want SCHEDULED_FOR_DELETION", got)
	}
	nk := getAsym(t, svc, newID, false)
	if nk.GetStatus().GetState() != kmspb.KeyState_ACTIVE || nk.GetSpec().GetAlgorithm() != kmspb.AsymmetricAlgorithm_ECDSA_NIST_P384_SHA_384 {
		t.Fatalf("new key = %v, want ACTIVE P384", nk)
	}
	tf.expectNoChanges(t)

	tf.destroy(t)
	if got := tf.stateIDs(t); len(got) != 0 {
		t.Fatalf("state after destroy = %v, want empty", got)
	}
	if _, err := svc.Get(t.Context(), &kmspb.GetAsymmetricKeyRequest{Id: newID}); status.Code(err) != codes.NotFound {
		t.Fatalf("Get after destroy: code %v, want NotFound (%v)", status.Code(err), err)
	}
	if got := getAsym(t, svc, newID, true).GetStatus().GetState(); got != kmspb.KeyState_SCHEDULED_FOR_DELETION {
		t.Fatalf("state after destroy = %v, want SCHEDULED_FOR_DELETION", got)
	}
}

func TestKeyDataSources(t *testing.T) {
	inst := startKMS(t)
	tf := newTerraform(t, inst)
	tf.writeConfig(t, fmt.Sprintf(`
resource "nebius_kms_v1_symmetric_key" "sym" {
  parent_id   = %[1]q
  name        = "ds-sym"
  description = "sym desc"
  algorithm   = "AES_256"
}

resource "nebius_kms_v1_asymmetric_key" "asym" {
  parent_id   = %[1]q
  name        = "ds-asym"
  description = "asym desc"
  algorithm   = "ECDSA_NIST_P256_SHA_256"
}

data "nebius_kms_v1_symmetric_key" "sym_by_id" {
  id = nebius_kms_v1_symmetric_key.sym.id
}

data "nebius_kms_v1_symmetric_key" "sym_by_name" {
  parent_id = %[1]q
  name      = nebius_kms_v1_symmetric_key.sym.name
}

data "nebius_kms_v1_asymmetric_key" "asym_by_id" {
  id = nebius_kms_v1_asymmetric_key.asym.id
}

data "nebius_kms_v1_asymmetric_key" "asym_by_name" {
  parent_id = %[1]q
  name      = nebius_kms_v1_asymmetric_key.asym.name
}
`, inst.ProjectID))
	tf.init(t)
	tf.expectPlan(t, map[string]string{"sym": "create", "asym": "create"})
	tf.apply(t)
	attrs := tf.stateAttrs(t)
	ids := tf.stateIDs(t)
	check := func(addr, wantID, wantAlgo, wantDesc string) {
		t.Helper()
		a, ok := attrs[addr]
		if !ok {
			t.Fatalf("%s missing from state; have %v", addr, slices.Sorted(maps.Keys(attrs)))
		}
		if a["id"] != wantID || a["algorithm"] != wantAlgo || a["description"] != wantDesc {
			t.Errorf("%s: id/algorithm/description = %v/%v/%v, want %s/%s/%s", addr, a["id"], a["algorithm"], a["description"], wantID, wantAlgo, wantDesc)
		}
	}
	for _, n := range []string{"sym_by_id", "sym_by_name"} {
		check("data.nebius_kms_v1_symmetric_key."+n, ids["sym"], "AES_256", "sym desc")
	}
	for _, n := range []string{"asym_by_id", "asym_by_name"} {
		check("data.nebius_kms_v1_asymmetric_key."+n, ids["asym"], "ECDSA_NIST_P256_SHA_256", "asym desc")
	}
	tf.expectNoChanges(t)
	tf.destroy(t)
}
