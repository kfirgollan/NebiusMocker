//go:build e2e

package iam

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-exec/tfexec"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	iamv2pb "github.com/nebius/gosdk/proto/nebius/iam/v2"
	vpcpb "github.com/nebius/gosdk/proto/nebius/vpc/v1"
	iamv2svc "github.com/nebius/gosdk/services/nebius/iam/v2"
	vpcsvc "github.com/nebius/gosdk/services/nebius/vpc/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/services/vpc"
)

// projectConfig is one nebius_iam_v2_project resource named "p".
func projectConfig(parent, name, labels, region string) string {
	regionLine := ""
	if region != "" {
		regionLine = fmt.Sprintf("region    = %q", region)
	}
	return fmt.Sprintf(`
resource "nebius_iam_v2_project" "p" {
  parent_id = %q
  name      = %q
  labels    = { %s }
  %s
}
`, parent, name, labels, regionLine)
}

// applyNow runs a plain `terraform apply` (plan and apply in one go).
func (r *tfRun) applyNow(t *testing.T) {
	t.Helper()
	if err := r.tf.Apply(t.Context()); err != nil {
		t.Fatalf("terraform apply: %v", err)
	}
}

// applyReplace runs `terraform apply -replace=addr`.
func (r *tfRun) applyReplace(t *testing.T, addr string) {
	t.Helper()
	if err := r.tf.Apply(t.Context(), tfexec.Replace(addr)); err != nil {
		t.Fatalf("terraform apply -replace=%s: %v", addr, err)
	}
}

func getProject(t *testing.T, svc iamv2svc.ProjectService, id string) *iamv2pb.Project {
	t.Helper()
	p, err := svc.Get(t.Context(), &iamv2pb.GetProjectRequest{Id: id})
	if err != nil {
		t.Fatalf("Get project %s: %v", id, err)
	}
	return p
}

func assertProjectGone(t *testing.T, svc iamv2svc.ProjectService, id string) {
	t.Helper()
	if _, err := svc.Get(t.Context(), &iamv2pb.GetProjectRequest{Id: id}); status.Code(err) != codes.NotFound {
		t.Fatalf("Get project %s: code %v, want NotFound (err: %v)", id, status.Code(err), err)
	}
}

func assertProject(t *testing.T, p *iamv2pb.Project, name, region string, labels map[string]string) {
	t.Helper()
	md := p.GetMetadata()
	if md.GetName() != name || p.GetSpec().GetRegion() != region || p.GetStatus().GetProjectState() != iamv2pb.ProjectStatus_ACTIVE {
		t.Fatalf("project = name %q region %q state %v, want %q %q ACTIVE", md.GetName(), p.GetSpec().GetRegion(), p.GetStatus().GetProjectState(), name, region)
	}
	for k, v := range labels {
		if md.GetLabels()[k] != v {
			t.Fatalf("label %s = %q, want %q (labels %v)", k, md.GetLabels()[k], v, md.GetLabels())
		}
	}
}

func startProjectMocker(t *testing.T, mods []mocker.Module, opts ...testkit.Option) *testkit.Instance {
	t.Helper()
	opts = append([]testkit.Option{testkit.WithOperationDelay(200 * time.Millisecond), testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce)}, opts...)
	return testkit.StartT(t, mods, mockerOptions(opts...)...)
}

// TestProjectLifecycle drives nebius_iam_v2_project through create, in-place
// update, a rejected region change (the server treats region as immutable
// although the provider plans an in-place update) and an explicit replace.
func TestProjectLifecycle(t *testing.T) {
	inst := startProjectMocker(t, []mocker.Module{iam.New()})
	projects := iamv2svc.NewProjectService(inst.SDK(t))
	tf := newTerraform(t, inst)

	tf.writeConfig(t, projectConfig(inst.TenantID, "team-a", `env = "dev"`, "eu-north1"))
	tf.init(t)
	tf.expectPlan(t, map[string]string{"p": "create"})
	tf.apply(t)
	id := tf.stateIDs(t)["p"]
	first := getProject(t, projects, id)
	assertProject(t, first, "team-a", "eu-north1", map[string]string{"env": "dev"})
	if first.GetMetadata().GetParentId() != inst.TenantID {
		t.Fatalf("parent = %q, want %q", first.GetMetadata().GetParentId(), inst.TenantID)
	}
	tf.expectNoChanges(t)

	// In-place update: rename + labels keep the ID and bump resource_version.
	tf.writeConfig(t, projectConfig(inst.TenantID, "team-b", `env = "prod", tier = "1"`, "eu-north1"))
	tf.expectPlan(t, map[string]string{"p": "update"})
	tf.apply(t)
	if got := tf.stateIDs(t)["p"]; got != id {
		t.Fatalf("project recreated by an in-place update: %s -> %s", id, got)
	}
	second := getProject(t, projects, id)
	assertProject(t, second, "team-b", "eu-north1", map[string]string{"env": "prod", "tier": "1"})
	if second.GetMetadata().GetResourceVersion() <= first.GetMetadata().GetResourceVersion() {
		t.Fatalf("resource_version %d -> %d, want growth", first.GetMetadata().GetResourceVersion(), second.GetMetadata().GetResourceVersion())
	}
	tf.expectNoChanges(t)

	// Region change: provider 0.6.91 plans an in-place update; the server refuses.
	tf.writeConfig(t, projectConfig(inst.TenantID, "team-b", `env = "prod", tier = "1"`, "eu-west1"))
	tf.expectPlan(t, map[string]string{"p": "update"})
	tf.expectApplyError(t, "spec.region is immutable")
	assertProject(t, getProject(t, projects, id), "team-b", "eu-north1", nil)

	// An explicit replace moves the project to the new region.
	tf.applyReplace(t, "nebius_iam_v2_project.p")
	newID := tf.stateIDs(t)["p"]
	if newID == id {
		t.Fatalf("replace kept the ID %s", id)
	}
	assertProjectGone(t, projects, id)
	assertProject(t, getProject(t, projects, newID), "team-b", "eu-west1", map[string]string{"env": "prod"})
	tf.expectNoChanges(t)

	tf.destroy(t)
	assertProjectGone(t, projects, newID)
}

// TestProjectOmittedRegion: spec.region is required on create, so a config
// without region fails and creates nothing.
func TestProjectOmittedRegion(t *testing.T) {
	inst := startProjectMocker(t, []mocker.Module{iam.New()})
	projects := iamv2svc.NewProjectService(inst.SDK(t))
	tf := newTerraform(t, inst)
	tf.writeConfig(t, projectConfig(inst.TenantID, "no-region", `a = "b"`, ""))
	tf.init(t)
	tf.expectApplyError(t, "spec.region is required")
	resp, err := projects.List(t.Context(), &iamv2pb.ListProjectsRequest{ParentId: inst.TenantID})
	if err != nil {
		t.Fatalf("List projects: %v", err)
	}
	if got, want := len(resp.GetItems()), len(inst.Projects); got != want {
		t.Fatalf("projects in tenant = %d, want only the %d seeded", got, want)
	}
}

// TestProjectAsParent builds a pool/network stack inside a Terraform-created
// project and proves a non-empty project cannot be deleted.
func TestProjectAsParent(t *testing.T) {
	inst := startProjectMocker(t, []mocker.Module{iam.New(), vpc.New()})
	sdk := inst.SDK(t)
	projects := iamv2svc.NewProjectService(sdk)
	networks := vpcsvc.NewNetworkService(sdk)
	tf := newTerraform(t, inst)

	tf.writeConfig(t, projectConfig(inst.TenantID, "stack", `a = "b"`, "eu-north1")+`
resource "nebius_vpc_v1_pool" "private" {
  parent_id  = nebius_iam_v2_project.p.id
  name       = "private-root"
  version    = "IPV4"
  visibility = "PRIVATE"
  cidrs      = [{ cidr = "10.40.0.0/16" }]
}

resource "nebius_vpc_v1_pool" "public" {
  parent_id  = nebius_iam_v2_project.p.id
  name       = "public-root"
  version    = "IPV4"
  visibility = "PUBLIC"
  cidrs      = [{ cidr = "198.51.100.0/24" }]
}

resource "nebius_vpc_v1_network" "n" {
  parent_id          = nebius_iam_v2_project.p.id
  name               = "net"
  ipv4_private_pools = { pools = [{ id = nebius_vpc_v1_pool.private.id }] }
  ipv4_public_pools  = { pools = [{ id = nebius_vpc_v1_pool.public.id }] }
}
`)
	tf.init(t)
	tf.applyNow(t)
	ids := tf.stateIDs(t)
	net, err := networks.Get(t.Context(), &vpcpb.GetNetworkRequest{Id: ids["n"]})
	if err != nil {
		t.Fatalf("Get network: %v", err)
	}
	if net.GetMetadata().GetParentId() != ids["p"] {
		t.Fatalf("network parent = %q, want the new project %q", net.GetMetadata().GetParentId(), ids["p"])
	}
	tf.expectNoChanges(t)

	op, err := projects.Delete(t.Context(), &iamv2pb.DeleteProjectRequest{Id: ids["p"]})
	if err == nil {
		_, err = op.Wait(t.Context())
	}
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("Delete of a non-empty project: code %v, want FailedPrecondition (err: %v)", status.Code(err), err)
	}
	getProject(t, projects, ids["p"])

	tf.destroy(t)
	assertProjectGone(t, projects, ids["p"])
}

// TestProjectDataSourcesAsViewer reads every seeded project through the v1 and
// v2 data sources, by id and by name, as a read-only principal.
func TestProjectDataSourcesAsViewer(t *testing.T) {
	inst := startProjectMocker(t, []mocker.Module{iam.New()}, testkit.WithProjects(
		config.ProjectSeed{Name: "alpha"}, config.ProjectSeed{Name: "beta", Region: "eu-west1"}, config.ProjectSeed{Name: "gamma"}))
	if len(inst.Projects) != 4 {
		t.Fatalf("seeded projects = %d, want 4", len(inst.Projects))
	}
	tf := newTerraformAs(t, inst, config.DefaultViewerToken)

	var b strings.Builder
	for i, p := range inst.Projects {
		fmt.Fprintf(&b, `
data "nebius_iam_v2_project" "v2_id_%[1]d"   { id = %[2]q }
data "nebius_iam_v2_project" "v2_name_%[1]d" {
  name      = %[3]q
  parent_id = %[4]q
}
data "nebius_iam_v1_project" "v1_id_%[1]d"   { id = %[2]q }
data "nebius_iam_v1_project" "v1_name_%[1]d" {
  name      = %[3]q
  parent_id = %[4]q
}
output "v2_id_%[1]d"   { value = [data.nebius_iam_v2_project.v2_id_%[1]d.id, data.nebius_iam_v2_project.v2_id_%[1]d.name, data.nebius_iam_v2_project.v2_id_%[1]d.region] }
output "v2_name_%[1]d" { value = [data.nebius_iam_v2_project.v2_name_%[1]d.id, data.nebius_iam_v2_project.v2_name_%[1]d.name, data.nebius_iam_v2_project.v2_name_%[1]d.region] }
output "v1_id_%[1]d"   { value = [data.nebius_iam_v1_project.v1_id_%[1]d.id, data.nebius_iam_v1_project.v1_id_%[1]d.name, data.nebius_iam_v1_project.v1_id_%[1]d.region] }
output "v1_name_%[1]d" { value = [data.nebius_iam_v1_project.v1_name_%[1]d.id, data.nebius_iam_v1_project.v1_name_%[1]d.name, data.nebius_iam_v1_project.v1_name_%[1]d.region] }
`, i, p.ID, p.Name, inst.TenantID)
	}
	tf.writeConfig(t, b.String())
	tf.init(t)
	tf.applyNow(t)
	outs, err := tf.tf.Output(t.Context())
	if err != nil {
		t.Fatalf("terraform output: %v", err)
	}
	for i, p := range inst.Projects {
		for _, kind := range []string{"v2_id", "v2_name", "v1_id", "v1_name"} {
			name := fmt.Sprintf("%s_%d", kind, i)
			o, ok := outs[name]
			if !ok {
				t.Fatalf("output %s missing", name)
			}
			want := fmt.Sprintf(`[%q,%q,%q]`, p.ID, p.Name, p.Region)
			if got := strings.Join(strings.Fields(string(o.Value)), ""); got != want {
				t.Errorf("%s = %s, want %s", name, got, want)
			}
		}
	}
	tf.expectNoChanges(t)

	// The viewer may read but not create projects.
	tf.writeConfig(t, projectConfig(inst.TenantID, "viewer-made", `a = "b"`, "eu-north1"))
	tf.expectApplyError(t, "PermissionDenied")
}
