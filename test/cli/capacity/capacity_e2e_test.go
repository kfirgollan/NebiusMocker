//go:build e2e

// Package capacity runs the official nebius CLI against NebiusMocker through
// the capacity flows the Nebius docs show: reading capacity block groups and
// intervals, asking resource advice, and managing project limits (capacity
// allowances), see https://docs.nebius.com/overview/limits/manage-project-limits.
package capacity

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/capacity"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/test/internal/nebiuscli"
)

type metadata struct {
	ID              string            `json:"id"`
	ParentID        string            `json:"parent_id"`
	Name            string            `json:"name"`
	ResourceVersion string            `json:"resource_version"`
	Labels          map[string]string `json:"labels"`
}

type affinity struct {
	ComputeV1 struct {
		Platform string `json:"platform"`
		Fabric   string `json:"fabric"`
	} `json:"compute_v1"`
}

type blockGroup struct {
	Metadata metadata `json:"metadata"`
	Status   struct {
		Region           string   `json:"region"`
		ResourceAffinity affinity `json:"resource_affinity"`
		Service          string   `json:"service"`
		State            string   `json:"state"`
		CurrentLimit     string   `json:"current_limit"`
	} `json:"status"`
}

type interval struct {
	Metadata metadata `json:"metadata"`
	Status   struct {
		Region   string `json:"region"`
		Quantity string `json:"quantity"`
		State    string `json:"state"`
	} `json:"status"`
}

type allowance struct {
	Metadata metadata `json:"metadata"`
	Spec     struct {
		CapacityBlockGroupID string  `json:"capacity_block_group_id"`
		Limit                *string `json:"limit"`
	} `json:"spec"`
	Status struct {
		State string `json:"state"`
		Unit  string `json:"unit"`
	} `json:"status"`
}

type env struct {
	cli     *nebiuscli.CLI
	tenant  string
	project string
}

func start(t *testing.T, token string) *env {
	t.Helper()
	inst := testkit.StartT(t, []mocker.Module{iam.New(), capacity.New()},
		testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce))
	cli := nebiuscli.New(t, nebiuscli.Profile{Addr: inst.Addr, Token: token, ParentID: inst.ProjectID, TenantID: inst.TenantID})
	return &env{cli: cli, tenant: inst.TenantID, project: inst.ProjectID}
}

// fails runs a command that must fail with an error mentioning want.
func (e *env) fails(t *testing.T, want string, args ...string) {
	t.Helper()
	_, err := e.cli.Run(t.Context(), args...)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("nebius %s: err = %v, want one mentioning %q", strings.Join(args, " "), err, want)
	}
}

// TestBlockGroupsIntervalsAndAdvice: the read-only capacity flows, as the
// editor.
func TestBlockGroupsIntervalsAndAdvice(t *testing.T) {
	e := start(t, config.DefaultEditorToken)

	// nebius capacity capacity-block-group list --parent-id <tenant>
	var list struct {
		Items []blockGroup `json:"items"`
	}
	e.cli.JSON(t, &list, "capacity", "capacity-block-group", "list", "--parent-id", e.tenant)
	if len(list.Items) != 3 {
		t.Fatalf("block group list = %d groups, want 3", len(list.Items))
	}
	byPlatform := map[string]blockGroup{}
	for _, g := range list.Items {
		byPlatform[g.Status.ResourceAffinity.ComputeV1.Platform] = g
	}
	h100, h200, b200 := byPlatform["gpu-h100-sxm"], byPlatform["gpu-h200-sxm"], byPlatform["gpu-b200-sxm"]
	if h100.Status.Region != "eu-north1" || h100.Status.ResourceAffinity.ComputeV1.Fabric != "fabric-2" ||
		h100.Status.CurrentLimit != "16" || h100.Status.State != "STATE_ACTIVE" || h100.Status.Service != "compute" {
		t.Fatalf("h100 block group = %+v", h100)
	}
	if h200.Status.CurrentLimit != "32" || h200.Status.ResourceAffinity.ComputeV1.Fabric != "fabric-7" {
		t.Fatalf("h200 block group = %+v", h200)
	}
	if b200.Status.Region != "us-central1" || b200.Status.State != "STATE_INACTIVE" {
		t.Fatalf("b200 block group = %+v, want inactive in us-central1", b200)
	}

	// nebius capacity capacity-block-group get <id>
	var got blockGroup
	e.cli.JSON(t, &got, "capacity", "capacity-block-group", "get", "--id", h100.Metadata.ID)
	if got.Metadata.ID != h100.Metadata.ID || got.Status.CurrentLimit != "16" {
		t.Fatalf("get = %+v, want %s", got, h100.Metadata.ID)
	}

	// nebius capacity capacity-block-group get-by-resource-affinity …
	got = blockGroup{}
	e.cli.JSON(t, &got, "capacity", "capacity-block-group", "get-by-resource-affinity", "--parent-id", e.tenant,
		"--region", "eu-north1", "--resource-affinity-compute-v1-platform", "gpu-h200-sxm", "--resource-affinity-compute-v1-fabric", "fabric-7")
	if got.Metadata.ID != h200.Metadata.ID {
		t.Fatalf("get-by-resource-affinity = %+v, want %s", got, h200.Metadata.ID)
	}
	e.fails(t, "NotFound", "capacity", "capacity-block-group", "get-by-resource-affinity", "--parent-id", e.tenant,
		"--region", "eu-north1", "--resource-affinity-compute-v1-platform", "gpu-h200-sxm", "--resource-affinity-compute-v1-fabric", "fabric-2")

	// nebius capacity capacity-block-group list-resources <id>: nothing runs in the group yet.
	var res struct {
		Items []any `json:"items"`
	}
	e.cli.JSON(t, &res, "capacity", "capacity-block-group", "list-resources", h100.Metadata.ID)
	if len(res.Items) != 0 {
		t.Fatalf("list-resources = %v, want none", res.Items)
	}

	// nebius capacity capacity-interval list --parent-id <block group>
	var ivs struct {
		Items []interval `json:"items"`
	}
	e.cli.JSON(t, &ivs, "capacity", "capacity-interval", "list", "--parent-id", h100.Metadata.ID)
	if len(ivs.Items) != 2 {
		t.Fatalf("interval list of the h100 group = %d, want 2", len(ivs.Items))
	}
	states := map[string]string{}
	for _, iv := range ivs.Items {
		if iv.Metadata.ParentID != h100.Metadata.ID || iv.Status.Region != "eu-north1" {
			t.Fatalf("interval = %+v, want a child of %s in eu-north1", iv, h100.Metadata.ID)
		}
		states[iv.Status.Quantity] = iv.Status.State
	}
	if states["16"] != "STATE_ACTIVE" || states["8"] != "STATE_SCHEDULED" {
		t.Fatalf("interval states by quantity = %v, want 16 active and 8 scheduled", states)
	}

	// nebius capacity resource-advice list --parent-id <tenant>
	var advice struct {
		Items []struct {
			Spec struct {
				Region          string `json:"region"`
				Fabric          string `json:"fabric"`
				ComputeInstance struct {
					Platform string `json:"platform"`
					Preset   struct {
						Name string `json:"name"`
					} `json:"preset"`
				} `json:"compute_instance"`
			} `json:"spec"`
			Status struct {
				Reserved struct {
					Available int `json:"available"`
					Limit     int `json:"limit"`
				} `json:"reserved"`
			} `json:"status"`
		} `json:"items"`
	}
	e.cli.JSON(t, &advice, "capacity", "resource-advice", "list", "--parent-id", e.tenant)
	if len(advice.Items) != 6 { // 3 block groups x 2 presets of their platform
		t.Fatalf("resource-advice list = %d items, want 6", len(advice.Items))
	}
	found := false
	for _, a := range advice.Items {
		ci := a.Spec.ComputeInstance
		if a.Spec.Region == "eu-north1" && a.Spec.Fabric == "fabric-2" && ci.Platform == "gpu-h100-sxm" && ci.Preset.Name == "8gpu-128vcpu-1600gb" {
			found = true
			// 16 reserved GPUs make two 8-GPU VMs.
			if a.Status.Reserved.Limit != 2 || a.Status.Reserved.Available != 2 {
				t.Fatalf("h100 8gpu advice reserved = %+v, want 2 of 2", a.Status.Reserved)
			}
		}
	}
	if !found {
		t.Fatalf("no advice for eu-north1/fabric-2/gpu-h100-sxm/8gpu-128vcpu-1600gb in %+v", advice.Items)
	}
}

// TestProjectLimits follows "Manage project limits": create an allowance for
// a block group, read it back, change and clear its limit, list and delete it.
func TestProjectLimits(t *testing.T) {
	e := start(t, config.DefaultEditorToken)

	var list struct {
		Items []blockGroup `json:"items"`
	}
	e.cli.JSON(t, &list, "capacity", "capacity-block-group", "list", "--parent-id", e.tenant)
	var cbg string
	for _, g := range list.Items {
		if g.Status.ResourceAffinity.ComputeV1.Platform == "gpu-h100-sxm" {
			cbg = g.Metadata.ID
		}
	}
	if cbg == "" {
		t.Fatalf("no h100 block group in %+v", list)
	}

	// Before: no allowance is created for the group.
	var byGroup struct {
		Items []allowance `json:"items"`
	}
	e.cli.JSON(t, &byGroup, "capacity", "capacity-allowance", "list-by-capacity-block-group", "--capacity-block-group-id", cbg)
	if len(byGroup.Items) != 0 {
		t.Fatalf("list-by-capacity-block-group before create = %+v", byGroup)
	}
	var def allowance
	e.cli.JSON(t, &def, "capacity", "capacity-allowance", "get-by-parent-and-capacity-block-group",
		"--parent-id", e.project, "--capacity-block-group-id", cbg)
	if def.Metadata.ID != "" || def.Spec.Limit != nil || def.Spec.CapacityBlockGroupID != cbg {
		t.Fatalf("default allowance = %+v, want no id and no limit", def)
	}

	// nebius capacity capacity-allowance create --capacity-block-group-id … --parent-id … --limit 4
	var created allowance
	e.cli.JSON(t, &created, "capacity", "capacity-allowance", "create", "--capacity-block-group-id", cbg,
		"--parent-id", e.project, "--limit", "4", "--name", "ml-limit", "--labels", "team=ml")
	id := created.Metadata.ID
	if !strings.HasPrefix(id, "capacityallowance-") || created.Spec.Limit == nil || *created.Spec.Limit != "4" ||
		created.Spec.CapacityBlockGroupID != cbg || created.Metadata.ParentID != e.project || created.Metadata.Name != "ml-limit" ||
		created.Metadata.Labels["team"] != "ml" || created.Status.Unit != "GPU" {
		t.Fatalf("allowance create = %+v", created)
	}
	// One allowance per project and block group.
	e.fails(t, "AlreadyExists", "capacity", "capacity-allowance", "create", "--capacity-block-group-id", cbg,
		"--parent-id", e.project, "--limit", "2")

	// get, get-by-parent-and-capacity-block-group, list, list-by-capacity-block-group.
	var got allowance
	e.cli.JSON(t, &got, "capacity", "capacity-allowance", "get", id)
	if got.Metadata.ID != id || got.Spec.Limit == nil || *got.Spec.Limit != "4" {
		t.Fatalf("allowance get = %+v", got)
	}
	got = allowance{}
	e.cli.JSON(t, &got, "capacity", "capacity-allowance", "get-by-parent-and-capacity-block-group",
		"--parent-id", e.project, "--capacity-block-group-id", cbg)
	if got.Metadata.ID != id {
		t.Fatalf("get-by-parent-and-capacity-block-group = %+v, want %s", got, id)
	}
	var projectList struct {
		Items []allowance `json:"items"`
	}
	e.cli.JSON(t, &projectList, "capacity", "capacity-allowance", "list", "--parent-id", e.project)
	if len(projectList.Items) != 3 { // one per block group: the created one and two defaults
		t.Fatalf("allowance list = %d items, want 3", len(projectList.Items))
	}
	var createdSeen int
	for _, a := range projectList.Items {
		if a.Metadata.ID == id {
			createdSeen++
		}
	}
	if createdSeen != 1 {
		t.Fatalf("allowance list does not contain %s exactly once: %+v", id, projectList.Items)
	}
	byGroup.Items = nil
	e.cli.JSON(t, &byGroup, "capacity", "capacity-allowance", "list-by-capacity-block-group", "--capacity-block-group-id", cbg)
	if len(byGroup.Items) != 1 || byGroup.Items[0].Metadata.ID != id {
		t.Fatalf("list-by-capacity-block-group = %+v, want [%s]", byGroup, id)
	}

	// nebius capacity capacity-allowance update <id> --limit 40: above the group size, accepted.
	e.cli.JSON(t, &allowance{}, "capacity", "capacity-allowance", "update", id, "--limit", "40")
	got = allowance{}
	e.cli.JSON(t, &got, "capacity", "capacity-allowance", "get", id)
	if got.Spec.Limit == nil || *got.Spec.Limit != "40" {
		t.Fatalf("allowance after update = %+v, want limit 40", got)
	}

	// Clear the limit (unlimited) with a reset mask in patch mode.
	// The documented patch form: a reset mask clears the optional field.
	if _, err := e.cli.Run(t.Context(), "capacity", "capacity-allowance", "update", id, "--patch", "--clear-mask", "spec.limit"); err != nil {
		t.Fatalf("clearing the limit with --patch --clear-mask spec.limit: %v", err)
	}
	got = allowance{}
	e.cli.JSON(t, &got, "capacity", "capacity-allowance", "get", id)
	if got.Spec.Limit != nil || got.Spec.CapacityBlockGroupID != cbg {
		t.Fatalf("allowance after --clear-mask spec.limit = %+v, want unlimited in %s", got, cbg)
	}
	e.cli.JSON(t, &allowance{}, "capacity", "capacity-allowance", "update", id, "--limit", "40")
	// The full form: a file without limit replaces the allowance.
	file := filepath.Join(t.TempDir(), "allowance.yaml")
	body := "metadata:\n  id: " + id + "\n  parent_id: " + e.project + "\n  name: ml-limit\n  labels:\n    team: ml\nspec:\n  capacity_block_group_id: " + cbg + "\n"
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", file, err)
	}
	e.cli.JSON(t, &allowance{}, "capacity", "capacity-allowance", "update", "--file", file)
	got = allowance{}
	e.cli.JSON(t, &got, "capacity", "capacity-allowance", "get", id)
	if got.Spec.Limit != nil {
		t.Fatalf("allowance after clearing the limit = %+v, want unlimited", got)
	}
	if got.Metadata.Name != "ml-limit" || got.Metadata.Labels["team"] != "ml" {
		t.Fatalf("clearing the limit changed name/labels: %+v", got)
	}

	// nebius capacity capacity-allowance delete <id>
	e.cli.JSON(t, &struct{}{}, "capacity", "capacity-allowance", "delete", id)
	e.fails(t, "NotFound", "capacity", "capacity-allowance", "get", id)
	got = allowance{}
	e.cli.JSON(t, &got, "capacity", "capacity-allowance", "get-by-parent-and-capacity-block-group",
		"--parent-id", e.project, "--capacity-block-group-id", cbg)
	if got.Metadata.ID != "" || got.Spec.Limit != nil {
		t.Fatalf("allowance after delete = %+v, want the default one", got)
	}
}

// TestViewerCannotChangeLimits: a viewer reads capacity but cannot set limits.
func TestViewerCannotChangeLimits(t *testing.T) {
	e := start(t, config.DefaultViewerToken)
	var list struct {
		Items []blockGroup `json:"items"`
	}
	e.cli.JSON(t, &list, "capacity", "capacity-block-group", "list", "--parent-id", e.tenant)
	if len(list.Items) == 0 {
		t.Fatal("viewer sees no block groups")
	}
	e.fails(t, "PermissionDenied", "capacity", "capacity-allowance", "create", "--capacity-block-group-id", list.Items[0].Metadata.ID,
		"--parent-id", e.project, "--limit", "1")
}
