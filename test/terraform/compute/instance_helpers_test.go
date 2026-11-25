//go:build e2e && !e2e_images

package compute

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"
	vpcpb "github.com/nebius/gosdk/proto/nebius/vpc/v1"
	computesvc "github.com/nebius/gosdk/services/nebius/compute/v1"
	vpcsvc "github.com/nebius/gosdk/services/nebius/vpc/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore/memory"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/vpc"
	"github.com/kfirgollan/NebiusMocker/test/internal/imagescenario"
)

const (
	instAddr  = "nebius_compute_v1_instance."
	allocAddr = "nebius_vpc_v1_allocation."
	// publicImagesProject owns the public image catalog in eu-north1 (e00).
	publicImagesProject = "project-e00public-images"
)

// instanceEnv is a mocker with iam + compute + vpc under authz enforce, with
// the official SDK clients (as the admin) to check what Terraform did.
// Terraform itself authenticates as the seeded editor.
type instanceEnv struct {
	*diskEnv
	Instances   computesvc.InstanceService
	Allocations vpcsvc.AllocationService
	SubnetID    string
	NetworkID   string
}

func startInstanceEnv(t *testing.T, opts ...testkit.Option) *instanceEnv {
	t.Helper()
	opts = append([]testkit.Option{testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce)}, opts...)
	env := imagescenario.Start(t, memory.New(clock.Real{}), []mocker.Module{vpc.New()}, opts...)
	sdk := env.Inst.SDK(t)
	subnet, err := vpcsvc.NewSubnetService(sdk).GetByName(t.Context(),
		&vpcpb.GetSubnetByNameRequest{ParentId: env.Inst.ProjectID, Name: "default-subnet"})
	if err != nil {
		t.Fatalf("SDK GetByName default-subnet: %v", err)
	}
	return &instanceEnv{
		diskEnv:     &diskEnv{Env: env, Disks: computesvc.NewDiskService(sdk), Snapshots: computesvc.NewDiskSnapshotService(sdk)},
		Instances:   computesvc.NewInstanceService(sdk),
		Allocations: vpcsvc.NewAllocationService(sdk),
		SubnetID:    subnet.GetMetadata().GetId(),
		NetworkID:   subnet.GetSpec().GetNetworkId(),
	}
}

// editorTerraform runs Terraform as the seeded editor (not the admin).
func (e *instanceEnv) editorTerraform(t *testing.T) *tfRun {
	t.Helper()
	return newTerraform(t, e.Inst.Addr, config.DefaultEditorToken)
}

func (e *instanceEnv) instance(t *testing.T, id string) *computepb.Instance {
	t.Helper()
	inst, err := e.Instances.Get(t.Context(), &computepb.GetInstanceRequest{Id: id})
	if err != nil {
		t.Fatalf("SDK Get instance %s: %v", id, err)
	}
	return inst
}

func (e *instanceEnv) assertInstanceGone(t *testing.T, id string) {
	t.Helper()
	_, err := e.Instances.Get(t.Context(), &computepb.GetInstanceRequest{Id: id})
	if got := status.Code(err); got != codes.NotFound {
		t.Fatalf("instance %s: SDK Get code = %v (err %v), want NotFound", id, got, err)
	}
}

func (e *instanceEnv) allocation(t *testing.T, id string) *vpcpb.Allocation {
	t.Helper()
	a, err := e.Allocations.Get(t.Context(), &vpcpb.GetAllocationRequest{Id: id})
	if err != nil {
		t.Fatalf("SDK Get allocation %s: %v", id, err)
	}
	return a
}

func (e *instanceEnv) assertAllocationGone(t *testing.T, id string) {
	t.Helper()
	_, err := e.Allocations.Get(t.Context(), &vpcpb.GetAllocationRequest{Id: id})
	if got := status.Code(err); got != codes.NotFound {
		t.Fatalf("allocation %s: SDK Get code = %v (err %v), want NotFound", id, got, err)
	}
}

// at walks nested Terraform values by map key (string) or list index (int)
// and renders the leaf like str ("" when absent).
func at(v any, path ...any) string {
	cur := v
	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, ok := cur.(map[string]any)
			if !ok {
				return ""
			}
			cur = m[k]
		case int:
			l, ok := cur.([]any)
			if !ok || k >= len(l) {
				return ""
			}
			cur = l[k]
		}
	}
	return str(map[string]any{"v": cur}, "v")
}

// count is the length of a nested list attribute (0 when absent).
func count(v any, path ...any) int {
	cur := v
	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, _ := cur.(map[string]any)
			cur = m[k]
		case int:
			l, _ := cur.([]any)
			if k >= len(l) {
				return 0
			}
			cur = l[k]
		}
	}
	l, _ := cur.([]any)
	return len(l)
}

// checkInstance compares Terraform's view of an instance (state attributes
// or a data source output) with the mock's, read with the SDK.
func checkInstance(t *testing.T, tfAttrs map[string]any, want *computepb.Instance) {
	t.Helper()
	md, spec, st := want.GetMetadata(), want.GetSpec(), want.GetStatus()
	checks := map[string][2]string{
		"id":                 {at(tfAttrs, "id"), md.GetId()},
		"name":               {at(tfAttrs, "name"), md.GetName()},
		"parent_id":          {at(tfAttrs, "parent_id"), md.GetParentId()},
		"resource_version":   {at(tfAttrs, "resource_version"), strconv.FormatInt(md.GetResourceVersion(), 10)},
		"resources.platform": {at(tfAttrs, "resources", "platform"), spec.GetResources().GetPlatform()},
		"resources.preset":   {at(tfAttrs, "resources", "preset"), spec.GetResources().GetPreset()},
		"hostname":           {at(tfAttrs, "hostname"), spec.GetHostname()},
		"status.state":       {at(tfAttrs, "status", "state"), st.GetState().String()},
		// the provider renders a false (zero) bool as unset
		"stopped":                    {strconv.FormatBool(at(tfAttrs, "stopped") == "true"), strconv.FormatBool(spec.GetStopped())},
		"status.reconciling":         {strconv.FormatBool(at(tfAttrs, "status", "reconciling") == "true"), strconv.FormatBool(st.GetReconciling())},
		"status.network_interfaces#": {strconv.Itoa(count(tfAttrs, "status", "network_interfaces")), strconv.Itoa(len(st.GetNetworkInterfaces()))},
		"status.disk_attachments#":   {strconv.Itoa(count(tfAttrs, "status", "disk_attachments")), strconv.Itoa(len(st.GetDiskAttachments()))},
		"secondary_disks#":           {strconv.Itoa(count(tfAttrs, "secondary_disks")), strconv.Itoa(len(spec.GetSecondaryDisks()))},
	}
	for i, nic := range st.GetNetworkInterfaces() {
		p := fmt.Sprintf("status.network_interfaces[%d].", i)
		checks[p+"name"] = [2]string{at(tfAttrs, "status", "network_interfaces", i, "name"), nic.GetName()}
		checks[p+"ip_address.address"] = [2]string{at(tfAttrs, "status", "network_interfaces", i, "ip_address", "address"), nic.GetIpAddress().GetAddress()}
		checks[p+"ip_address.allocation_id"] = [2]string{at(tfAttrs, "status", "network_interfaces", i, "ip_address", "allocation_id"), nic.GetIpAddress().GetAllocationId()}
		checks[p+"public_ip_address.address"] = [2]string{at(tfAttrs, "status", "network_interfaces", i, "public_ip_address", "address"), nic.GetPublicIpAddress().GetAddress()}
		checks[p+"mac_address"] = [2]string{at(tfAttrs, "status", "network_interfaces", i, "mac_address"), nic.GetMacAddress()}
		checks[p+"fqdn"] = [2]string{at(tfAttrs, "status", "network_interfaces", i, "fqdn"), nic.GetFqdn()}
	}
	for i, d := range st.GetDiskAttachments() {
		p := fmt.Sprintf("status.disk_attachments[%d].", i)
		checks[p+"id"] = [2]string{at(tfAttrs, "status", "disk_attachments", i, "id"), d.GetId()}
		checks[p+"name"] = [2]string{at(tfAttrs, "status", "disk_attachments", i, "name"), d.GetName()}
	}
	for _, field := range slices.Sorted(maps.Keys(checks)) {
		c := checks[field]
		if c[0] != c[1] {
			t.Errorf("instance %s: terraform %s = %q, mock has %q", md.GetId(), field, c[0], c[1])
		}
	}
	if got := labelsOf(tfAttrs); !maps.Equal(got, md.GetLabels()) && (len(got) > 0 || len(md.GetLabels()) > 0) {
		t.Errorf("instance %s: terraform labels = %v, mock has %v", md.GetId(), got, md.GetLabels())
	}
}

// attachedDisks maps the instance's status.disk_attachments by disk id.
func attachedDisks(inst *computepb.Instance) map[string]*computepb.DiskAttachmentStatus {
	out := map[string]*computepb.DiskAttachmentStatus{}
	for _, d := range inst.GetStatus().GetDiskAttachments() {
		out[d.GetId()] = d
	}
	return out
}
