//go:build e2e

// Package registry runs the official Terraform provider (nebius/nebius from
// the Terraform Registry) against NebiusMocker's Container Registry and checks
// the mock's state with the official Go SDK after every step.
//
// Run with: make e2e
package registry

import (
	"fmt"
	"maps"
	"os"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	registrypb "github.com/nebius/gosdk/proto/nebius/registry/v1"
	registrysvc "github.com/nebius/gosdk/services/nebius/registry/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/services/registry"
	"github.com/kfirgollan/NebiusMocker/test/internal/tfcache"
	"github.com/kfirgollan/NebiusMocker/test/internal/tfharness"
)

const (
	resAddr  = "nebius_registry_v1_registry.app"
	dataByID = "data.nebius_registry_v1_registry.by_id"
	dataByNm = "data.nebius_registry_v1_registry.by_name"
)

func tfConfig(projectID, name, description, labels string) string {
	return fmt.Sprintf(`
resource "nebius_registry_v1_registry" "app" {
  parent_id   = %[1]q
  name        = %[2]q
  description = %[3]q
  labels      = { %[4]s }
}

data "nebius_registry_v1_registry" "by_id" {
  id = nebius_registry_v1_registry.app.id
}

data "nebius_registry_v1_registry" "by_name" {
  parent_id  = %[1]q
  name       = nebius_registry_v1_registry.app.name
  depends_on = [nebius_registry_v1_registry.app]
}
`, projectID, name, description, labels)
}

// TestRegistryCRUD: create → empty plan → in-place updates (description,
// labels, name) → empty plan → destroy, as the editor under enforced authz,
// with asynchronous operations so the provider polls OperationService.
func TestRegistryCRUD(t *testing.T) {
	inst := testkit.StartT(t, []mocker.Module{iam.New(), registry.New()},
		testkit.WithOperationDelay(300*time.Millisecond), testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce))
	registries := registrysvc.NewRegistryService(inst.SDK(t))
	tf := tfharness.New(t, inst.Addr, config.DefaultEditorToken)

	tf.Write(t, tfConfig(inst.ProjectID, "app", "application images", `team = "core"`))
	tf.Init(t)
	tf.ExpectPlan(t, map[string]string{resAddr: "create"})
	tf.Apply(t)
	state := tf.State(t)
	id, _ := state[resAddr]["id"].(string)
	got := getRegistry(t, registries, id)
	if got.GetMetadata().GetName() != "app" || got.GetSpec().GetDescription() != "application images" ||
		got.GetMetadata().GetLabels()["team"] != "core" || got.GetStatus().GetState() != registrypb.RegistryStatus_ACTIVE {
		t.Fatalf("mock registry after create = %v", got)
	}
	assertDataSources(t, state, id, got.GetStatus().GetRegistryFqdn())
	tf.ExpectNoChanges(t)

	steps := []struct {
		name, description, labels string
		check                     func(*registrypb.Registry) bool
	}{
		{"app", "new description", `team = "core", tier = "1"`, func(r *registrypb.Registry) bool {
			return r.GetSpec().GetDescription() == "new description" && maps.Equal(r.GetMetadata().GetLabels(), map[string]string{"team": "core", "tier": "1"})
		}},
		{"app", "new description", `tier = "1"`, func(r *registrypb.Registry) bool { // removed label is cleared (reset mask)
			return maps.Equal(r.GetMetadata().GetLabels(), map[string]string{"tier": "1"})
		}},
		{"app-renamed", "", `tier = "1"`, func(r *registrypb.Registry) bool {
			return r.GetMetadata().GetName() == "app-renamed" && r.GetSpec().GetDescription() == ""
		}},
	}
	for i, st := range steps {
		tf.Write(t, tfConfig(inst.ProjectID, st.name, st.description, st.labels))
		tf.ExpectPlan(t, map[string]string{resAddr: "update"})
		tf.Apply(t)
		state := tf.State(t)
		if newID := state[resAddr]["id"]; newID != id {
			t.Fatalf("step %d: registry recreated: %s → %v", i, id, newID)
		}
		r := getRegistry(t, registries, id)
		if !st.check(r) {
			t.Fatalf("step %d: mock registry = %v does not reflect the config", i, r)
		}
		if v := r.GetMetadata().GetResourceVersion(); v != int64(i+2) {
			t.Errorf("step %d: resource_version = %d, want %d", i, v, i+2)
		}
		assertDataSources(t, state, id, r.GetStatus().GetRegistryFqdn())
		tf.ExpectNoChanges(t)
	}

	tf.Destroy(t)
	if _, err := registries.Get(t.Context(), &registrypb.GetRegistryRequest{Id: id}); status.Code(err) != codes.NotFound {
		t.Fatalf("Get after destroy: %v, want NotFound", err)
	}
	if st := tf.State(t); len(st) != 0 {
		t.Fatalf("state after destroy = %v, want empty", st)
	}
}

// TestMain gives this binary a private Terraform plugin cache (see tfcache).
func TestMain(m *testing.M) { os.Exit(tfcache.Main(m)) }

func getRegistry(t *testing.T, svc registrysvc.RegistryService, id string) *registrypb.Registry {
	t.Helper()
	if id == "" {
		t.Fatal("registry id missing from terraform state")
	}
	r, err := svc.Get(t.Context(), &registrypb.GetRegistryRequest{Id: id})
	if err != nil {
		t.Fatalf("Get(%s) from mock: %v", id, err)
	}
	return r
}

// assertDataSources checks that both data sources resolved the managed registry.
func assertDataSources(t *testing.T, state map[string]map[string]any, id, fqdn string) {
	t.Helper()
	for _, addr := range []string{dataByID, dataByNm} {
		ds := state[addr]
		if ds["id"] != id {
			t.Errorf("%s.id = %v, want %s", addr, ds["id"], id)
		}
		st, _ := ds["status"].(map[string]any)
		if st["registry_fqdn"] != fqdn || st["state"] != "ACTIVE" {
			t.Errorf("%s.status = %v, want ACTIVE with fqdn %s", addr, st, fqdn)
		}
	}
}
