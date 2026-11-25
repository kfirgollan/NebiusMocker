//go:build e2e && !e2e_images

package compute

import (
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-exec/tfexec"
	tfjson "github.com/hashicorp/terraform-json"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"
	computesvc "github.com/nebius/gosdk/services/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore/memory"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/test/internal/imagescenario"
)

const diskPlanFile = "tfplan"

// diskEnv is a mocker (enforce mode) with the official SDK clients for disks.
type diskEnv struct {
	*imagescenario.Env
	Disks     computesvc.DiskService
	Snapshots computesvc.DiskSnapshotService
}

func startDiskEnv(t *testing.T) *diskEnv {
	t.Helper()
	env := imagescenario.Start(t, memory.New(clock.Real{}), nil,
		testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce))
	sdk := env.Inst.SDK(t)
	return &diskEnv{Env: env, Disks: computesvc.NewDiskService(sdk), Snapshots: computesvc.NewDiskSnapshotService(sdk)}
}

func (e *diskEnv) disk(t *testing.T, id string) *computepb.Disk {
	t.Helper()
	d, err := e.Disks.Get(t.Context(), &computepb.GetDiskRequest{Id: id})
	if err != nil {
		t.Fatalf("SDK Get disk %s: %v", id, err)
	}
	return d
}

func (e *diskEnv) snapshot(t *testing.T, id string) *computepb.DiskSnapshot {
	t.Helper()
	s, err := e.Snapshots.Get(t.Context(), &computepb.GetDiskSnapshotRequest{Id: id})
	if err != nil {
		t.Fatalf("SDK Get snapshot %s: %v", id, err)
	}
	return s
}

func (e *diskEnv) assertDiskGone(t *testing.T, id string) {
	t.Helper()
	_, err := e.Disks.Get(t.Context(), &computepb.GetDiskRequest{Id: id})
	if got := status.Code(err); got != codes.NotFound {
		t.Fatalf("disk %s: SDK Get code = %v (err %v), want NotFound", id, got, err)
	}
}

func (e *diskEnv) assertSnapshotGone(t *testing.T, id string) {
	t.Helper()
	_, err := e.Snapshots.Get(t.Context(), &computepb.GetDiskSnapshotRequest{Id: id})
	if got := status.Code(err); got != codes.NotFound {
		t.Fatalf("snapshot %s: SDK Get code = %v (err %v), want NotFound", id, got, err)
	}
}

// planIs plans to a file and asserts the action of every managed resource
// (name -> "create", "update", "replace", "delete", "no-op").
func (r *tfRun) planIs(t *testing.T, want map[string]string) {
	t.Helper()
	if _, err := r.tf.Plan(t.Context(), tfexec.Out(diskPlanFile)); err != nil {
		t.Fatalf("terraform plan: %v", err)
	}
	plan, err := r.tf.ShowPlanFile(t.Context(), diskPlanFile)
	if err != nil {
		t.Fatalf("terraform show plan: %v", err)
	}
	got := map[string]string{}
	for _, rc := range plan.ResourceChanges {
		if rc.Mode == tfjson.ManagedResourceMode {
			got[rc.Name] = diskActionName(rc.Change.Actions)
		}
	}
	if !maps.Equal(got, want) {
		t.Fatalf("plan actions = %v, want %v", got, want)
	}
}

// applyPlan applies the plan file written by planIs.
func (r *tfRun) applyPlan(t *testing.T) {
	t.Helper()
	if err := r.tf.Apply(t.Context(), tfexec.DirOrPlan(diskPlanFile)); err != nil {
		t.Fatalf("terraform apply: %v", err)
	}
}

// step plans (asserting the actions), applies, then asserts the next plan is
// empty: the mock's Get round-trips through the provider schema without drift.
func (r *tfRun) step(t *testing.T, want map[string]string) {
	t.Helper()
	r.planIs(t, want)
	r.applyPlan(t)
	r.requireEmptyPlan(t)
}

func (r *tfRun) requireEmptyPlan(t *testing.T) {
	t.Helper()
	changed, err := r.tf.Plan(t.Context())
	if err != nil {
		t.Fatalf("terraform plan: %v", err)
	}
	if changed {
		t.Fatalf("plan after apply is not empty (drift between the mock's Get and the applied config); terraform output:\n%s", r.out.String())
	}
}

func diskActionName(a tfjson.Actions) string {
	switch {
	case a.NoOp():
		return "no-op"
	case a.Create():
		return "create"
	case a.Delete():
		return "delete"
	case a.Update():
		return "update"
	case a.Replace():
		return "replace"
	default:
		names := make([]string, 0, len(a))
		for _, x := range a {
			names = append(names, string(x))
		}
		sort.Strings(names)
		return strings.Join(names, "+")
	}
}

// stateOf returns the attributes of one managed resource in the Terraform
// state by address, e.g. "nebius_compute_v1_disk.main".
func (r *tfRun) stateOf(t *testing.T, addr string) map[string]any {
	t.Helper()
	st, err := r.tf.Show(t.Context())
	if err != nil {
		t.Fatalf("terraform show: %v", err)
	}
	if st.Values != nil && st.Values.RootModule != nil {
		for _, res := range st.Values.RootModule.Resources {
			if res.Address == addr {
				return res.AttributeValues
			}
		}
	}
	t.Fatalf("%s is not in the Terraform state", addr)
	return nil
}

// stateID is the id of a managed resource in the Terraform state.
func (r *tfRun) stateID(t *testing.T, addr string) string {
	t.Helper()
	return str(r.stateOf(t, addr), "id")
}

// labelsOf renders a "labels" attribute as a map.
func labelsOf(v map[string]any) map[string]string {
	out := map[string]string{}
	if l, ok := v["labels"].(map[string]any); ok {
		for k, x := range l {
			s, _ := x.(string)
			out[k] = s
		}
	}
	return out
}

// checkDisk compares Terraform's view of a disk (state attributes or a data
// source output) with the mock's, read with the SDK.
func checkDisk(t *testing.T, tfAttrs map[string]any, want *computepb.Disk) {
	t.Helper()
	md, st := want.GetMetadata(), want.GetStatus()
	checks := map[string][2]string{
		"id":                {str(tfAttrs, "id"), md.GetId()},
		"name":              {str(tfAttrs, "name"), md.GetName()},
		"parent_id":         {str(tfAttrs, "parent_id"), md.GetParentId()},
		"type":              {str(tfAttrs, "type"), want.GetSpec().GetType().String()},
		"status.state":      {str(tfAttrs, "status", "state"), st.GetState().String()},
		"status.size_bytes": {str(tfAttrs, "status", "size_bytes"), strconv.FormatInt(st.GetSizeBytes(), 10)},
		"status.block_size": {str(tfAttrs, "status", "block_size_bytes"), strconv.FormatInt(st.GetBlockSizeBytes(), 10)},
		"resource_version":  {str(tfAttrs, "resource_version"), strconv.FormatInt(md.GetResourceVersion(), 10)},
		"status.source_img": {str(tfAttrs, "status", "source_image_id"), st.GetSourceImageId()},
		// the provider renders a false (zero) bool as unset
		"status.reconciling": {strconv.FormatBool(str(tfAttrs, "status", "reconciling") == "true"), strconv.FormatBool(st.GetReconciling())},
	}
	for _, field := range slices.Sorted(maps.Keys(checks)) {
		c := checks[field]
		if c[0] != c[1] {
			t.Errorf("disk %s: terraform %s = %q, mock has %q", md.GetId(), field, c[0], c[1])
		}
	}
	if got := labelsOf(tfAttrs); !maps.Equal(got, md.GetLabels()) && (len(got) > 0 || len(md.GetLabels()) > 0) {
		t.Errorf("disk %s: terraform labels = %v, mock has %v", md.GetId(), got, md.GetLabels())
	}
}

// checkSnapshot is checkDisk for snapshots.
func checkSnapshot(t *testing.T, tfAttrs map[string]any, want *computepb.DiskSnapshot) {
	t.Helper()
	md, st := want.GetMetadata(), want.GetStatus()
	checks := map[string][2]string{
		"id":                        {str(tfAttrs, "id"), md.GetId()},
		"name":                      {str(tfAttrs, "name"), md.GetName()},
		"parent_id":                 {str(tfAttrs, "parent_id"), md.GetParentId()},
		"source_disk_id":            {str(tfAttrs, "source_disk_id"), want.GetSpec().GetSourceDiskId()},
		"description":               {str(tfAttrs, "description"), want.GetSpec().GetDescription()},
		"status.state":              {str(tfAttrs, "status", "state"), st.GetState().String()},
		"status.content_size_bytes": {str(tfAttrs, "status", "content_size_bytes"), strconv.FormatInt(st.GetContentSizeBytes(), 10)},
		"status.storage_size_bytes": {str(tfAttrs, "status", "storage_size_bytes"), strconv.FormatInt(st.GetStorageSizeBytes(), 10)},
		"resource_version":          {str(tfAttrs, "resource_version"), strconv.FormatInt(md.GetResourceVersion(), 10)},
	}
	for _, field := range slices.Sorted(maps.Keys(checks)) {
		c := checks[field]
		if c[0] != c[1] {
			t.Errorf("snapshot %s: terraform %s = %q, mock has %q", md.GetId(), field, c[0], c[1])
		}
	}
	if got := labelsOf(tfAttrs); !maps.Equal(got, md.GetLabels()) && (len(got) > 0 || len(md.GetLabels()) > 0) {
		t.Errorf("snapshot %s: terraform labels = %v, mock has %v", md.GetId(), got, md.GetLabels())
	}
}

// hclLabels renders a labels attribute.
func hclLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("  labels = {\n")
	for _, k := range slices.Sorted(maps.Keys(labels)) {
		fmt.Fprintf(&b, "    %s = %q\n", k, labels[k])
	}
	b.WriteString("  }\n")
	return b.String()
}
