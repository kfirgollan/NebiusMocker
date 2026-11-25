package contract

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/nebius/gosdk"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"
	iamv2pb "github.com/nebius/gosdk/proto/nebius/iam/v2"
	iamsvc "github.com/nebius/gosdk/services/nebius/iam/v1"
	iamv2svc "github.com/nebius/gosdk/services/nebius/iam/v2"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/iam"
)

// TestProjectGet covers ProjectService.Get on the seeded project, as
// `nebius iam project get` uses it (e.g. the Object Storage quick start).
func TestProjectGet(t *testing.T) {
	t.Parallel()
	inst := testkit.StartT(t, []mocker.Module{iam.New()}, testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce))
	projects := iamsvc.NewProjectService(inst.SDK(t))
	p, err := projects.Get(t.Context(), &iampb.GetProjectRequest{Id: inst.ProjectID})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if p.GetMetadata().GetId() != inst.ProjectID || p.GetMetadata().GetParentId() != inst.TenantID {
		t.Fatalf("Get = %v; want project %s in tenant %s", p.GetMetadata(), inst.ProjectID, inst.TenantID)
	}
	if _, err := projects.Get(t.Context(), &iampb.GetProjectRequest{Id: "project-e00doesnotexist0000"}); status.Code(err) != codes.NotFound {
		t.Fatalf("Get unknown: %v, want NotFound", err)
	}
}

const (
	projOutsiderToken = "project-outsider-token"
	projOutsiderID    = "tenantuseraccount-e00outsider0000000"
)

// projEnv is a mocker with enforced authz and several seeded projects, plus
// one SDK per caller.
type projEnv struct {
	inst                    *testkit.Instance
	admin, viewer, outsider *gosdk.SDK
}

func startProjEnv(t *testing.T) projEnv {
	t.Helper()
	inst := testkit.StartT(t, []mocker.Module{iam.New()},
		testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce),
		testkit.WithTokens(map[string]string{projOutsiderToken: projOutsiderID}),
		testkit.WithProjects(
			config.ProjectSeed{Name: "alpha"},
			config.ProjectSeed{Name: "beta", Region: "eu-west1"},
			config.ProjectSeed{Name: "gamma"},
		))
	if len(inst.Projects) < 4 {
		t.Fatalf("seeded projects = %v, want at least 4", inst.Projects)
	}
	return projEnv{
		inst:     inst,
		admin:    inst.SDK(t),
		viewer:   inst.SDKAs(t, config.DefaultViewerToken),
		outsider: inst.SDKAs(t, projOutsiderToken),
	}
}

// projAPI hides the v1/v2 difference for the shared List/Get/GetByName checks.
type projAPI struct {
	name    string
	list    func(ctx context.Context, parent string, size int64, token string) (ids, names []string, next string, err error)
	get     func(ctx context.Context, id string) (parent, name, region string, err error)
	getName func(ctx context.Context, parent, name string) (id string, err error)
	filter  func(ctx context.Context, parent string, size int64) ([]string, error)
}

func projAPIs(sdk *gosdk.SDK) []projAPI {
	v1 := iamsvc.NewProjectService(sdk)
	v2 := iamv2svc.NewProjectService(sdk)
	return []projAPI{
		{
			name: "v1",
			list: func(ctx context.Context, parent string, size int64, token string) ([]string, []string, string, error) {
				r, err := v1.List(ctx, &iampb.ListProjectsRequest{ParentId: parent, PageSize: size, PageToken: token})
				if err != nil {
					return nil, nil, "", err
				}
				var ids, names []string
				for _, p := range r.GetItems() {
					ids = append(ids, p.GetMetadata().GetId())
					names = append(names, p.GetMetadata().GetName())
				}
				return ids, names, r.GetNextPageToken(), nil
			},
			get: func(ctx context.Context, id string) (string, string, string, error) {
				p, err := v1.Get(ctx, &iampb.GetProjectRequest{Id: id})
				if err != nil {
					return "", "", "", err
				}
				if p.GetStatus().GetContainerState() != iampb.State_ACTIVE || p.GetStatus().GetRegion() == "" {
					return "", "", "", fmt.Errorf("v1 status = %v, want ACTIVE with region", p.GetStatus())
				}
				return p.GetMetadata().GetParentId(), p.GetMetadata().GetName(), p.GetSpec().GetRegion(), nil
			},
			getName: func(ctx context.Context, parent, name string) (string, error) {
				p, err := v1.GetByName(ctx, &iampb.GetProjectByNameRequest{ParentId: parent, Name: name})
				return p.GetMetadata().GetId(), err
			},
			filter: func(ctx context.Context, parent string, size int64) ([]string, error) {
				var ids []string
				for p, err := range v1.Filter(ctx, &iampb.ListProjectsRequest{ParentId: parent, PageSize: size}) {
					if err != nil {
						return ids, err
					}
					ids = append(ids, p.GetMetadata().GetId())
				}
				return ids, nil
			},
		},
		{
			name: "v2",
			list: func(ctx context.Context, parent string, size int64, token string) ([]string, []string, string, error) {
				r, err := v2.List(ctx, &iamv2pb.ListProjectsRequest{ParentId: parent, PageSize: size, PageToken: token})
				if err != nil {
					return nil, nil, "", err
				}
				var ids, names []string
				for _, p := range r.GetItems() {
					ids = append(ids, p.GetMetadata().GetId())
					names = append(names, p.GetMetadata().GetName())
				}
				return ids, names, r.GetNextPageToken(), nil
			},
			get: func(ctx context.Context, id string) (string, string, string, error) {
				p, err := v2.Get(ctx, &iamv2pb.GetProjectRequest{Id: id})
				if err != nil {
					return "", "", "", err
				}
				if p.GetStatus().GetProjectState() != iamv2pb.ProjectStatus_ACTIVE {
					return "", "", "", fmt.Errorf("v2 project_state = %v, want ACTIVE", p.GetStatus().GetProjectState())
				}
				return p.GetMetadata().GetParentId(), p.GetMetadata().GetName(), p.GetSpec().GetRegion(), nil
			},
			getName: func(ctx context.Context, parent, name string) (string, error) {
				p, err := v2.GetByName(ctx, &iamv2pb.GetProjectByNameRequest{ParentId: parent, Name: name})
				return p.GetMetadata().GetId(), err
			},
			filter: func(ctx context.Context, parent string, size int64) ([]string, error) {
				var ids []string
				for p, err := range v2.Filter(ctx, &iamv2pb.ListProjectsRequest{ParentId: parent, PageSize: size}) {
					if err != nil {
						return ids, err
					}
					ids = append(ids, p.GetMetadata().GetId())
				}
				return ids, nil
			},
		},
	}
}

// pageAll follows next_page_token until it is empty; every page must have items.
func projPageAll(t *testing.T, a projAPI, parent string, size int64) (ids, names []string) {
	t.Helper()
	token := ""
	for range 50 {
		pi, pn, next, err := a.list(t.Context(), parent, size, token)
		if err != nil {
			t.Fatalf("%s List(size=%d, token=%q): %v", a.name, size, token, err)
		}
		if len(pi) == 0 {
			t.Fatalf("%s List(size=%d, token=%q): empty page (next=%q)", a.name, size, token, next)
		}
		if size > 0 && int64(len(pi)) > size {
			t.Fatalf("%s List(size=%d): %d items on one page", a.name, size, len(pi))
		}
		ids, names = append(ids, pi...), append(names, pn...)
		if next == "" {
			return ids, names
		}
		token = next
	}
	t.Fatalf("%s List(size=%d): paging did not terminate", a.name, size)
	return nil, nil
}

func seededIDs(inst *testkit.Instance) []string {
	ids := make([]string, 0, len(inst.Projects))
	for _, p := range inst.Projects {
		ids = append(ids, p.ID)
	}
	return ids
}

func TestProjectListViewerSeesEverySeeded(t *testing.T) {
	t.Parallel()
	env := startProjEnv(t)
	want := seededIDs(env.inst)
	for _, a := range projAPIs(env.viewer) {
		for _, size := range []int64{0, 1, 2, 3, 100} {
			t.Run(fmt.Sprintf("%s/page_size_%d", a.name, size), func(t *testing.T) {
				t.Parallel()
				got, _ := projPageAll(t, a, env.inst.TenantID, size)
				if diff := cmp.Diff(want, got); diff != "" {
					t.Errorf("project IDs (-want +got):\n%s", diff)
				}
			})
		}
		t.Run(a.name+"/filter_iterator", func(t *testing.T) {
			t.Parallel()
			got, err := a.filter(t.Context(), env.inst.TenantID, 1)
			if err != nil {
				t.Fatalf("Filter: %v", err)
			}
			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("project IDs (-want +got):\n%s", diff)
			}
		})
	}
}

func TestProjectGetViewerSeeded(t *testing.T) {
	t.Parallel()
	env := startProjEnv(t)
	for _, a := range projAPIs(env.viewer) {
		for _, sp := range env.inst.Projects {
			t.Run(a.name+"/"+sp.Name, func(t *testing.T) {
				t.Parallel()
				parent, name, region, err := a.get(t.Context(), sp.ID)
				if err != nil {
					t.Fatalf("Get: %v", err)
				}
				if parent != env.inst.TenantID || name != sp.Name || region != sp.Region {
					t.Errorf("Get = (%s, %s, %s), want (%s, %s, %s)", parent, name, region, env.inst.TenantID, sp.Name, sp.Region)
				}
				id, err := a.getName(t.Context(), env.inst.TenantID, sp.Name)
				if err != nil || id != sp.ID {
					t.Errorf("GetByName(%s) = %q, %v; want %s", sp.Name, id, err, sp.ID)
				}
			})
		}
	}
}

func TestProjectOutsider(t *testing.T) {
	t.Parallel()
	env := startProjEnv(t)
	for _, a := range projAPIs(env.outsider) {
		t.Run(a.name, func(t *testing.T) {
			t.Parallel()
			ids, _, next, err := a.list(t.Context(), env.inst.TenantID, 0, "")
			if err != nil || len(ids) != 0 || next != "" {
				t.Errorf("List = %v, %q, %v; want empty and no error", ids, next, err)
			}
			_, _, _, err = a.get(t.Context(), env.inst.ProjectID)
			assertCode(t, err, codes.NotFound)
			_, err = a.getName(t.Context(), env.inst.TenantID, "default")
			assertCode(t, err, codes.NotFound)
		})
	}
	r1, err := iamsvc.NewTenantService(env.outsider).List(t.Context(), &iampb.ListTenantsRequest{})
	if err != nil || len(r1.GetItems()) != 0 {
		t.Errorf("v1 tenant List = %v, %v; want empty", r1.GetItems(), err)
	}
	r2, err := iamv2svc.NewTenantService(env.outsider).List(t.Context(), &iamv2pb.ListTenantsRequest{})
	if err != nil || len(r2.GetItems()) != 0 {
		t.Errorf("v2 tenant List = %v, %v; want empty", r2.GetItems(), err)
	}
}

// grantViewer lets principal read target through a new group, a viewer access
// permit and a membership, all created by the admin through the SDK.
func grantViewer(t *testing.T, admin *gosdk.SDK, tenantID, principal, target string) {
	t.Helper()
	g := createGroup(t, iamsvc.NewGroupService(admin), tenantID, "grant-"+target, nil)
	gid := g.GetMetadata().GetId()
	ctx := t.Context()
	op, err := iamsvc.NewGroupMembershipService(admin).Create(ctx, &iampb.CreateGroupMembershipRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: gid},
		Spec:     &iampb.GroupMembershipSpec{MemberId: principal},
	})
	if err != nil {
		t.Fatalf("create membership: %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("membership wait: %v", err)
	}
	op, err = iamsvc.NewAccessPermitService(admin).Create(ctx, &iampb.CreateAccessPermitRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: gid},
		Spec:     &iampb.AccessPermitSpec{ResourceId: target, Role: authz.RoleViewer},
	})
	if err != nil {
		t.Fatalf("create permit: %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("permit wait: %v", err)
	}
}

func TestProjectSingleProjectGrantee(t *testing.T) {
	t.Parallel()
	env := startProjEnv(t)
	granted := env.inst.Projects[2] // beta, eu-west1
	grantViewer(t, env.admin, env.inst.TenantID, projOutsiderID, granted.ID)
	for _, a := range projAPIs(env.outsider) {
		t.Run(a.name, func(t *testing.T) {
			t.Parallel()
			for _, size := range []int64{0, 1} {
				ids, names := projPageAll(t, a, env.inst.TenantID, size)
				if diff := cmp.Diff([]string{granted.ID}, ids); diff != "" {
					t.Errorf("size %d: project IDs (-want +got):\n%s", size, diff)
				}
				if diff := cmp.Diff([]string{granted.Name}, names); diff != "" {
					t.Errorf("size %d: project names (-want +got):\n%s", size, diff)
				}
			}
			ids, err := a.filter(t.Context(), env.inst.TenantID, 1)
			if err != nil || !slices.Equal(ids, []string{granted.ID}) {
				t.Errorf("Filter = %v, %v; want [%s]", ids, err, granted.ID)
			}
			if _, name, region, err := a.get(t.Context(), granted.ID); err != nil || name != granted.Name || region != granted.Region {
				t.Errorf("Get granted = %q, %q, %v", name, region, err)
			}
			if id, err := a.getName(t.Context(), env.inst.TenantID, granted.Name); err != nil || id != granted.ID {
				t.Errorf("GetByName granted = %q, %v", id, err)
			}
			for _, other := range env.inst.Projects {
				if other.ID == granted.ID {
					continue
				}
				_, _, _, err := a.get(t.Context(), other.ID)
				assertCode(t, err, codes.NotFound)
				_, err = a.getName(t.Context(), env.inst.TenantID, other.Name)
				assertCode(t, err, codes.NotFound)
			}
		})
	}
}

func TestProjectViewerDenied(t *testing.T) {
	t.Parallel()
	env := startProjEnv(t)
	ctx := t.Context()
	target := env.inst.Projects[1].ID
	v1 := iamsvc.NewProjectService(env.viewer)
	v2 := iamv2svc.NewProjectService(env.viewer)
	md := func(id string) *commonpb.ResourceMetadata {
		return &commonpb.ResourceMetadata{Id: id, ParentId: env.inst.TenantID, Name: "denied"}
	}
	calls := map[string]func() error{
		"v1 Create": func() error {
			_, err := v1.Create(ctx, &iampb.CreateProjectRequest{Metadata: md("")})
			return err
		},
		"v2 Create": func() error {
			_, err := v2.Create(ctx, &iamv2pb.CreateProjectRequest{Metadata: md("")})
			return err
		},
		"v1 Update": func() error {
			_, err := v1.Update(ctx, &iampb.UpdateProjectRequest{Metadata: md(target)})
			return err
		},
		"v2 Update": func() error {
			_, err := v2.Update(ctx, &iamv2pb.UpdateProjectRequest{Metadata: md(target)})
			return err
		},
		"v2 Delete": func() error {
			_, err := v2.Delete(ctx, &iamv2pb.DeleteProjectRequest{Id: target})
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assertCode(t, call(), codes.PermissionDenied)
		})
	}
}

func TestProjectListArgumentErrors(t *testing.T) {
	t.Parallel()
	env := startProjEnv(t)
	for _, a := range projAPIs(env.viewer) {
		t.Run(a.name, func(t *testing.T) {
			t.Parallel()
			_, _, _, err := a.list(t.Context(), "", 0, "")
			assertCode(t, err, codes.InvalidArgument)
			_, _, _, err = a.list(t.Context(), "tenant-e00doesnotexist0000", 0, "")
			assertCode(t, err, codes.NotFound)
		})
	}
	ctx := t.Context()
	_, err := iamsvc.NewProjectService(env.viewer).List(ctx, &iampb.ListProjectsRequest{ParentId: env.inst.TenantID, Filter: "name=x"})
	assertCode(t, err, codes.InvalidArgument)
	_, err = iamv2svc.NewProjectService(env.viewer).List(ctx, &iamv2pb.ListProjectsRequest{ParentId: env.inst.TenantID, Filter: "name=x"})
	assertCode(t, err, codes.InvalidArgument)
	_, err = iamsvc.NewTenantService(env.viewer).List(ctx, &iampb.ListTenantsRequest{Filter: "name=x"})
	assertCode(t, err, codes.InvalidArgument)
	_, err = iamv2svc.NewTenantService(env.viewer).List(ctx, &iamv2pb.ListTenantsRequest{Filter: "name=x"})
	assertCode(t, err, codes.InvalidArgument)
}

func TestProjectAnonymousList(t *testing.T) {
	t.Parallel()
	env := startProjEnv(t)
	anon := env.inst.SDKAs(t, "")
	_, err := iamsvc.NewProjectService(anon).List(t.Context(), &iampb.ListProjectsRequest{ParentId: env.inst.TenantID})
	assertCode(t, err, codes.Unauthenticated)
	_, err = iamv2svc.NewProjectService(anon).List(t.Context(), &iamv2pb.ListProjectsRequest{ParentId: env.inst.TenantID})
	assertCode(t, err, codes.Unauthenticated)
	_, err = iamsvc.NewTenantService(anon).List(t.Context(), &iampb.ListTenantsRequest{})
	assertCode(t, err, codes.Unauthenticated)
	_, err = iamv2svc.NewTenantService(anon).List(t.Context(), &iamv2pb.ListTenantsRequest{})
	assertCode(t, err, codes.Unauthenticated)
}

// createProjectV2 creates a project through v2 and returns its ID.
func createProjectV2(t *testing.T, svc iamv2svc.ProjectService, tenantID, name, region string) string {
	t.Helper()
	op, err := svc.Create(t.Context(), &iamv2pb.CreateProjectRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: tenantID, Name: name},
		Spec:     &iamv2pb.ProjectSpec{Region: region},
	})
	if err != nil {
		t.Fatalf("v2 Create(%s): %v", name, err)
	}
	if _, err := op.Wait(t.Context()); err != nil {
		t.Fatalf("v2 Create(%s) wait: %v", name, err)
	}
	if op.ResourceID() == "" {
		t.Fatalf("v2 Create(%s): empty operation resource id", name)
	}
	return op.ResourceID()
}

func TestProjectCreate(t *testing.T) {
	t.Parallel()
	env := startProjEnv(t)
	ctx := t.Context()
	v1 := iamsvc.NewProjectService(env.admin)
	v2 := iamv2svc.NewProjectService(env.admin)
	tenant := env.inst.TenantID

	id := createProjectV2(t, v2, tenant, "made-v2", "eu-north1")
	got, err := v2.Get(ctx, &iamv2pb.GetProjectRequest{Id: id})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.GetSpec().GetRegion() != "eu-north1" || got.GetMetadata().GetParentId() != tenant || got.GetMetadata().GetName() != "made-v2" {
		t.Errorf("created project = %v; want eu-north1 default region in %s", got, tenant)
	}
	byName, err := v2.GetByName(ctx, &iamv2pb.GetProjectByNameRequest{ParentId: tenant, Name: "made-v2"})
	if err != nil || byName.GetMetadata().GetId() != id {
		t.Errorf("GetByName = %v, %v; want %s", byName.GetMetadata(), err, id)
	}
	west := createProjectV2(t, v2, tenant, "made-west", "eu-west1")
	if p, err := v2.Get(ctx, &iamv2pb.GetProjectRequest{Id: west}); err != nil || p.GetSpec().GetRegion() != "eu-west1" {
		t.Errorf("explicit region project = %v, %v; want eu-west1", p, err)
	}

	// v1 Create, read back through v1.
	op, err := v1.Create(ctx, &iampb.CreateProjectRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: tenant, Name: "made-v1"},
	})
	if err != nil {
		t.Fatalf("v1 Create: %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("v1 Create wait: %v", err)
	}
	p1, err := v1.Get(ctx, &iampb.GetProjectRequest{Id: op.ResourceID()})
	if err != nil || p1.GetSpec().GetRegion() != "eu-north1" || p1.GetMetadata().GetName() != "made-v1" {
		t.Errorf("v1 created project = %v, %v", p1, err)
	}

	// The list shows the seeded and the three new projects.
	want := append(seededIDs(env.inst), id, west, op.ResourceID())
	for _, a := range projAPIs(env.admin) {
		ids, _ := projPageAll(t, a, tenant, 2)
		if diff := cmp.Diff(want, ids); diff != "" {
			t.Errorf("%s List after create (-want +got):\n%s", a.name, diff)
		}
	}

	tests := []struct {
		name string
		md   *commonpb.ResourceMetadata
		spec *iamv2pb.ProjectSpec
		want codes.Code
	}{
		{"v2 empty region", &commonpb.ResourceMetadata{ParentId: tenant, Name: "no-region"}, nil, codes.InvalidArgument},
		{"unknown region", &commonpb.ResourceMetadata{ParentId: tenant, Name: "bad-region"}, &iamv2pb.ProjectSpec{Region: "mars-north1"}, codes.InvalidArgument},
		{"duplicate name", &commonpb.ResourceMetadata{ParentId: tenant, Name: "made-v2"}, nil, codes.AlreadyExists},
		{"parent is not a tenant", &commonpb.ResourceMetadata{ParentId: env.inst.ProjectID, Name: "nested"}, nil, codes.InvalidArgument},
		{"unknown tenant", &commonpb.ResourceMetadata{ParentId: "tenant-e00doesnotexist0000", Name: "orphan"}, nil, codes.NotFound},
		{"missing name", &commonpb.ResourceMetadata{ParentId: tenant}, nil, codes.InvalidArgument},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			spec := tt.spec
			if spec == nil && tt.name != "v2 empty region" {
				spec = &iamv2pb.ProjectSpec{Region: "eu-north1"}
			}
			_, err := v2.Create(ctx, &iamv2pb.CreateProjectRequest{Metadata: tt.md, Spec: spec})
			assertCode(t, err, tt.want)
		})
	}
}

func TestProjectUpdate(t *testing.T) {
	t.Parallel()
	env := startProjEnv(t)
	ctx := t.Context()
	tenant := env.inst.TenantID
	v1 := iamsvc.NewProjectService(env.admin)
	v2 := iamv2svc.NewProjectService(env.admin)
	id := createProjectV2(t, v2, tenant, "upd", "eu-north1")
	before, err := v2.Get(ctx, &iamv2pb.GetProjectRequest{Id: id})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	// v2: rename and set labels.
	op, err := v2.Update(ctx, &iamv2pb.UpdateProjectRequest{
		Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: tenant, Name: "upd-renamed", Labels: map[string]string{"team": "core"}},
		Spec:     &iamv2pb.ProjectSpec{Region: "eu-north1"},
	})
	if err != nil {
		t.Fatalf("v2 Update: %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("v2 Update wait: %v", err)
	}
	after, err := v2.Get(ctx, &iamv2pb.GetProjectRequest{Id: id})
	if err != nil {
		t.Fatalf("Get after update: %v", err)
	}
	if after.GetMetadata().GetName() != "upd-renamed" || after.GetMetadata().GetLabels()["team"] != "core" {
		t.Errorf("after update = %v", after.GetMetadata())
	}
	if after.GetMetadata().GetResourceVersion() <= before.GetMetadata().GetResourceVersion() {
		t.Errorf("resource_version %d -> %d, want an increase", before.GetMetadata().GetResourceVersion(), after.GetMetadata().GetResourceVersion())
	}
	if _, err := v2.GetByName(ctx, &iamv2pb.GetProjectByNameRequest{ParentId: tenant, Name: "upd-renamed"}); err != nil {
		t.Errorf("GetByName new name: %v", err)
	}
	_, err = v2.GetByName(ctx, &iamv2pb.GetProjectByNameRequest{ParentId: tenant, Name: "upd"})
	assertCode(t, err, codes.NotFound)

	// v1: rename again.
	cur, err := v1.Get(ctx, &iampb.GetProjectRequest{Id: id})
	if err != nil {
		t.Fatalf("v1 Get: %v", err)
	}
	md := cur.GetMetadata()
	md.Name = "upd-v1"
	op1, err := v1.Update(ctx, &iampb.UpdateProjectRequest{Metadata: md, Spec: cur.GetSpec()})
	if err != nil {
		t.Fatalf("v1 Update: %v", err)
	}
	if _, err := op1.Wait(ctx); err != nil {
		t.Fatalf("v1 Update wait: %v", err)
	}
	if p, err := v1.Get(ctx, &iampb.GetProjectRequest{Id: id}); err != nil || p.GetMetadata().GetName() != "upd-v1" {
		t.Errorf("v1 after update = %v, %v", p.GetMetadata(), err)
	}

	// Region is immutable on both versions, and the message names the field.
	_, err = v2.Update(ctx, &iamv2pb.UpdateProjectRequest{
		Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: tenant, Name: "upd-v1"},
		Spec:     &iamv2pb.ProjectSpec{Region: "eu-west1"},
	})
	assertImmutableRegion(t, "v2", err)
	_, err = v1.Update(ctx, &iampb.UpdateProjectRequest{
		Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: tenant, Name: "upd-v1"},
		Spec:     &iampb.ContainerSpec{Region: "eu-west1"},
	})
	assertImmutableRegion(t, "v1", err)
	if p, err := v2.Get(ctx, &iamv2pb.GetProjectRequest{Id: id}); err != nil || p.GetSpec().GetRegion() != "eu-north1" {
		t.Errorf("region after rejected updates = %v, %v", p.GetSpec(), err)
	}
}

func assertImmutableRegion(t *testing.T, ver string, err error) {
	t.Helper()
	if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "spec.region") {
		t.Errorf("%s region change = %v; want InvalidArgument mentioning spec.region", ver, err)
	}
}

func TestProjectDelete(t *testing.T) {
	t.Parallel()
	env := startProjEnv(t)
	ctx := t.Context()
	tenant := env.inst.TenantID
	v2 := iamv2svc.NewProjectService(env.admin)
	id := createProjectV2(t, v2, tenant, "doomed", "eu-north1")

	// dry_run changes nothing.
	if err := deleteProjectWait(t, v2, id, true); err != nil {
		t.Fatalf("dry-run Delete: %v", err)
	}
	if _, err := v2.Get(ctx, &iamv2pb.GetProjectRequest{Id: id}); err != nil {
		t.Fatalf("project gone after dry run: %v", err)
	}

	// A project with a service account is refused, naming the account.
	sas := iamsvc.NewServiceAccountService(env.admin)
	saID := createServiceAccount(t, sas, id, "inside").GetMetadata().GetId()
	err := deleteProjectWait(t, v2, id, false)
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), saID) {
		t.Fatalf("Delete non-empty = %v; want FailedPrecondition naming %s", err, saID)
	}
	if err := deleteProjectWait(t, v2, id, true); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("dry-run Delete non-empty = %v; want FailedPrecondition", err)
	}

	op, err := sas.Delete(ctx, &iampb.DeleteServiceAccountRequest{Id: saID})
	if err != nil {
		t.Fatalf("delete service account: %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("delete service account wait: %v", err)
	}
	if err := deleteProjectWait(t, v2, id, false); err != nil {
		t.Fatalf("Delete empty: %v", err)
	}
	_, err = v2.Get(ctx, &iamv2pb.GetProjectRequest{Id: id})
	assertCode(t, err, codes.NotFound)
	for _, a := range projAPIs(env.admin) {
		ids, _ := projPageAll(t, a, tenant, 1)
		if slices.Contains(ids, id) {
			t.Errorf("%s List still shows deleted project %s", a.name, id)
		}
	}
	assertCode(t, deleteProjectWait(t, v2, id, false), codes.NotFound)
}

// deleteProjectWait issues Delete and waits; the error may come from either.
func deleteProjectWait(t *testing.T, svc iamv2svc.ProjectService, id string, dryRun bool) error {
	t.Helper()
	op, err := svc.Delete(t.Context(), &iamv2pb.DeleteProjectRequest{Id: id, DryRun: dryRun})
	if err != nil {
		return err
	}
	_, err = op.Wait(t.Context())
	return err
}
