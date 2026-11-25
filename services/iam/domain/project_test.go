package domain_test

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/nebius/gosdk/proto/fieldmask/mask"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/services/iam/domain"
)

// fakeProjectRepo is a slice-backed domain.ProjectRepository (creation order,
// index tokens). children maps a project ID to the kinds of records under it.
type fakeProjectRepo struct {
	items    []*iampb.Container
	children map[string][]string
	deleted  []string
}

func (f *fakeProjectRepo) index(id string) int {
	for i, p := range f.items {
		if p.GetMetadata().GetId() == id {
			return i
		}
	}
	return -1
}

func (f *fakeProjectRepo) Get(_ context.Context, id string) (*iampb.Container, error) {
	i := f.index(id)
	if i < 0 {
		return nil, errs.NotFound(id)
	}
	return proto.CloneOf(f.items[i]), nil
}

func (f *fakeProjectRepo) Ensure(_ context.Context, c *iampb.Container) error {
	if f.index(c.GetMetadata().GetId()) < 0 {
		f.items = append(f.items, proto.CloneOf(c))
	}
	return nil
}

func (f *fakeProjectRepo) GetByName(_ context.Context, tenant, name string) (*iampb.Container, error) {
	for _, p := range f.items {
		if p.GetMetadata().GetParentId() == tenant && p.GetMetadata().GetName() == name {
			return proto.CloneOf(p), nil
		}
	}
	return nil, errs.NotFound(name)
}

func (f *fakeProjectRepo) List(_ context.Context, tenant string, size int, token string) (domain.ContainerPage, error) {
	start := 0
	if token != "" {
		n, err := strconv.Atoi(token)
		if err != nil {
			return domain.ContainerPage{}, errs.BadRequest(errs.Violation{Field: "page_token", Message: "bad"})
		}
		start = n
	}
	var page domain.ContainerPage
	for i := start; i < len(f.items); i++ {
		if f.items[i].GetMetadata().GetParentId() != tenant {
			continue
		}
		if len(page.Items) == size {
			page.NextPageToken = strconv.Itoa(i)
			break
		}
		page.Items = append(page.Items, proto.CloneOf(f.items[i]))
	}
	return page, nil
}

func (f *fakeProjectRepo) Create(ctx context.Context, p *iampb.Container) error {
	if existing, err := f.GetByName(ctx, p.GetMetadata().GetParentId(), p.GetMetadata().GetName()); err == nil {
		return errs.AlreadyExists(existing.GetMetadata().GetId(), "name taken")
	}
	f.items = append(f.items, proto.CloneOf(p))
	return nil
}

func (f *fakeProjectRepo) Update(_ context.Context, p *iampb.Container) error {
	i := f.index(p.GetMetadata().GetId())
	if i < 0 {
		return errs.NotFound(p.GetMetadata().GetId())
	}
	f.items[i] = proto.CloneOf(p)
	return nil
}

func (f *fakeProjectRepo) CheckEmpty(_ context.Context, id string, isResource func(string) bool) error {
	if f.index(id) < 0 {
		return errs.NotFound(id)
	}
	for _, kind := range f.children[id] {
		if isResource(kind) {
			return errs.BadState(id, "project "+id+" is not empty")
		}
	}
	return nil
}

func (f *fakeProjectRepo) DeleteIfEmpty(ctx context.Context, id string, isResource func(string) bool) error {
	if err := f.CheckEmpty(ctx, id, isResource); err != nil {
		return err
	}
	f.items = append(f.items[:f.index(id)], f.items[f.index(id)+1:]...)
	f.deleted = append(f.deleted, id)
	return nil
}

// fakeVisibility lets the caller see only the listed IDs; it records the
// permission it was asked about.
type fakeVisibility struct {
	visible map[string]bool
	perms   map[authz.Permission]bool
}

func (v *fakeVisibility) CanSee(_ context.Context, perm authz.Permission, id string) (bool, error) {
	v.perms[perm] = true
	return v.visible[id], nil
}

type fakeKinds map[string]bool

func (k fakeKinds) HasKind(kind string) bool { return k[kind] }

type projectFixture struct {
	svc  *domain.ProjectService
	repo *fakeProjectRepo
	vis  *fakeVisibility
	clk  *clock.Manual
}

func newProjectFixture(t *testing.T) *projectFixture {
	t.Helper()
	f := &projectFixture{
		repo: &fakeProjectRepo{children: map[string][]string{}},
		vis:  &fakeVisibility{visible: map[string]bool{}, perms: map[authz.Permission]bool{}},
		clk:  clock.NewManual(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
	}
	svc, err := domain.NewProjectService(domain.ProjectDeps{
		Projects: f.repo, Tenants: fakeTenants{}, Visibility: f.vis, Kinds: fakeKinds{"vpcnetwork": true},
		Ops: inlineOps{}, IDs: &seqIDs{}, Clock: f.clk, Regions: []string{"eu-north1", "eu-west1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	f.svc = svc
	return f
}

func (f *projectFixture) create(t *testing.T, name, region string, labels map[string]string) *iampb.Container {
	t.Helper()
	op, err := f.svc.Create(t.Context(), domain.CreateProjectCmd{
		Metadata: &commonpb.ResourceMetadata{ParentId: tenantID, Name: name, Labels: labels}, Region: region,
	})
	if err != nil {
		t.Fatalf("Create(%q): %v", name, err)
	}
	p, err := f.svc.Get(t.Context(), op.ResourceID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	return p
}

func TestNewProjectServiceValidation(t *testing.T) {
	t.Parallel()
	f := newProjectFixture(t)
	if _, err := domain.NewProjectService(domain.ProjectDeps{Projects: f.repo}); err == nil {
		t.Error("missing dependencies: want error")
	}
	if _, err := domain.NewProjectService(domain.ProjectDeps{
		Projects: f.repo, Tenants: fakeTenants{}, Visibility: f.vis, Kinds: fakeKinds{},
		Ops: inlineOps{}, IDs: &seqIDs{}, Clock: f.clk,
	}); err == nil {
		t.Error("no regions: want error")
	}
}

func TestProjectListFiltersByVisibility(t *testing.T) {
	t.Parallel()
	f := newProjectFixture(t)
	var ids []string
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		ids = append(ids, f.create(t, n, "", nil).GetMetadata().GetId())
	}
	f.vis.visible[ids[1]] = true
	f.vis.visible[ids[3]] = true

	var got []string
	token := ""
	for range 10 {
		page, err := f.svc.List(t.Context(), tenantID, 1, token, "")
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(page.Items) != 1 {
			t.Fatalf("page has %d items, want 1", len(page.Items))
		}
		got = append(got, page.Items[0].GetMetadata().GetId())
		if token = page.NextPageToken; token == "" {
			break
		}
	}
	if len(got) != 2 || got[0] != ids[1] || got[1] != ids[3] {
		t.Fatalf("visible projects = %v, want [%s %s]", got, ids[1], ids[3])
	}
	if !f.vis.perms[authz.Perm("iam", "projects", authz.VerbGet)] {
		t.Errorf("visibility asked about %v, want iam.projects.get", f.vis.perms)
	}
}

func TestProjectListNothingVisible(t *testing.T) {
	t.Parallel()
	f := newProjectFixture(t)
	f.create(t, "a", "", nil)
	page, err := f.svc.List(t.Context(), tenantID, 1, "", "")
	if err != nil || len(page.Items) != 0 || page.NextPageToken != "" {
		t.Fatalf("page = %+v, %v; want empty", page, err)
	}
}

func TestProjectListRejections(t *testing.T) {
	t.Parallel()
	f := newProjectFixture(t)
	tests := []struct {
		name     string
		tenant   string
		size     int64
		filter   string
		want     errs.Kind
		contains string
	}{
		{"filter", tenantID, 0, "name=x", errs.KindBadRequest, "filter"},
		{"missing parent", "", 0, "", errs.KindBadRequest, "parent_id"},
		{"unknown tenant", "tenant-e00test0000000000999", 0, "", errs.KindNotFound, ""},
		{"negative page size", tenantID, -1, "", errs.KindBadRequest, "page_size"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := f.svc.List(t.Context(), tt.tenant, tt.size, "", tt.filter)
			if !errs.IsKind(err, tt.want) {
				t.Fatalf("err = %v, want kind %v", err, tt.want)
			}
			if tt.contains != "" && !strings.Contains(err.Error(), tt.contains) {
				t.Errorf("err = %q, want it to mention %q", err, tt.contains)
			}
		})
	}
}

func TestProjectCreate(t *testing.T) {
	t.Parallel()
	f := newProjectFixture(t)

	p := f.create(t, "alpha", "", map[string]string{"k": "v"})
	if p.GetSpec().GetRegion() != "eu-north1" || p.GetStatus().GetRegion() != "eu-north1" {
		t.Errorf("default region = %q/%q, want eu-north1", p.GetSpec().GetRegion(), p.GetStatus().GetRegion())
	}
	md := p.GetMetadata()
	if md.GetParentId() != tenantID || md.GetResourceVersion() != 1 || md.GetLabels()["k"] != "v" || !strings.HasPrefix(md.GetId(), "project-") {
		t.Errorf("metadata = %v", md)
	}
	if p.GetStatus().GetContainerState() != iampb.State_ACTIVE {
		t.Errorf("state = %v, want ACTIVE", p.GetStatus().GetContainerState())
	}
	if west := f.create(t, "beta", "eu-west1", nil); west.GetSpec().GetRegion() != "eu-west1" {
		t.Errorf("region = %q, want eu-west1", west.GetSpec().GetRegion())
	}
}

// TestProjectCreateRequireRegion: the v2 API has no default region, v1 does.
func TestProjectCreateRequireRegion(t *testing.T) {
	t.Parallel()
	f := newProjectFixture(t)
	md := &commonpb.ResourceMetadata{ParentId: tenantID, Name: "v2-no-region"}
	_, err := f.svc.Create(t.Context(), domain.CreateProjectCmd{Metadata: md, RequireRegion: true})
	if !errs.IsKind(err, errs.KindBadRequest) || !strings.Contains(err.Error(), "spec.region is required") {
		t.Fatalf("v2 create without region: err = %v, want BadRequest naming spec.region", err)
	}
	if _, err := f.svc.Create(t.Context(), domain.CreateProjectCmd{Metadata: md, Region: "eu-west1", RequireRegion: true}); err != nil {
		t.Fatalf("v2 create with region: %v", err)
	}
}

func TestProjectCreateRejections(t *testing.T) {
	t.Parallel()
	f := newProjectFixture(t)
	f.create(t, "taken", "", nil)
	tests := []struct {
		name     string
		md       *commonpb.ResourceMetadata
		region   string
		want     errs.Kind
		contains string
	}{
		{"id set", &commonpb.ResourceMetadata{Id: "project-x", ParentId: tenantID, Name: "n"}, "", errs.KindBadRequest, "metadata.id"},
		{"parent missing", &commonpb.ResourceMetadata{Name: "n"}, "", errs.KindBadRequest, "metadata.parent_id"},
		{"parent not a tenant", &commonpb.ResourceMetadata{ParentId: "project-e00test1", Name: "n"}, "", errs.KindBadRequest, "tenant"},
		{"unknown tenant", &commonpb.ResourceMetadata{ParentId: "tenant-e00test0000000000999", Name: "n"}, "", errs.KindNotFound, ""},
		{"name missing", &commonpb.ResourceMetadata{ParentId: tenantID}, "", errs.KindBadRequest, "metadata.name"},
		{"unknown region", &commonpb.ResourceMetadata{ParentId: tenantID, Name: "n"}, "mars1", errs.KindBadRequest, "spec.region"},
		{"duplicate name", &commonpb.ResourceMetadata{ParentId: tenantID, Name: "taken"}, "", errs.KindAlreadyExists, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := f.svc.Create(t.Context(), domain.CreateProjectCmd{Metadata: tt.md, Region: tt.region})
			if !errs.IsKind(err, tt.want) {
				t.Fatalf("err = %v, want kind %v", err, tt.want)
			}
			if tt.contains != "" && !strings.Contains(err.Error(), tt.contains) {
				t.Errorf("err = %q, want it to mention %q", err, tt.contains)
			}
		})
	}
}

func TestProjectUpdateRenameLabelsAndVersion(t *testing.T) {
	t.Parallel()
	f := newProjectFixture(t)
	p := f.create(t, "a", "", map[string]string{"k": "v"})
	id := p.GetMetadata().GetId()
	f.clk.Advance(time.Minute)

	update := func(cmd domain.UpdateProjectCmd) {
		t.Helper()
		if _, err := f.svc.Update(t.Context(), cmd); err != nil {
			t.Fatalf("Update: %v", err)
		}
	}
	version := func() int64 {
		t.Helper()
		got, err := f.svc.Get(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		return got.GetMetadata().GetResourceVersion()
	}

	// Rename keeps labels (not in the mask) and bumps the version.
	update(domain.UpdateProjectCmd{Metadata: &commonpb.ResourceMetadata{Id: id, Name: "b"}})
	got, _ := f.svc.Get(t.Context(), id)
	if got.GetMetadata().GetName() != "b" || got.GetMetadata().GetLabels()["k"] != "v" || version() != 2 {
		t.Fatalf("after rename: %v", got.GetMetadata())
	}
	if !got.GetMetadata().GetUpdatedAt().AsTime().Equal(f.clk.Now()) {
		t.Errorf("updated_at = %v, want %v", got.GetMetadata().GetUpdatedAt().AsTime(), f.clk.Now())
	}

	// Labels in the reset mask and absent from the request are cleared.
	update(domain.UpdateProjectCmd{
		Metadata:  &commonpb.ResourceMetadata{Id: id, Name: "b"},
		ResetMask: mask.ParseMust("metadata.labels"),
	})
	got, _ = f.svc.Get(t.Context(), id)
	if len(got.GetMetadata().GetLabels()) != 0 || version() != 3 {
		t.Fatalf("after reset: %v", got.GetMetadata())
	}

	// Same values: no version bump, even with the stored region repeated.
	update(domain.UpdateProjectCmd{
		Metadata: &commonpb.ResourceMetadata{Id: id, Name: "b"},
		Spec:     &iampb.ContainerSpec{Region: "eu-north1"},
	})
	if version() != 3 {
		t.Errorf("no-op update bumped version to %d", version())
	}
}

func TestProjectUpdateRegion(t *testing.T) {
	t.Parallel()
	f := newProjectFixture(t)
	id := f.create(t, "a", "", nil).GetMetadata().GetId()
	md := &commonpb.ResourceMetadata{Id: id, Name: "a"}

	_, err := f.svc.Update(t.Context(), domain.UpdateProjectCmd{Metadata: md, Spec: &iampb.ContainerSpec{Region: "eu-west1"}})
	if !errs.IsKind(err, errs.KindBadRequest) || !strings.Contains(err.Error(), "spec.region") {
		t.Fatalf("changing region err = %v, want BadRequest naming spec.region", err)
	}

	// An empty region keeps the stored one, with or without a reset mask.
	for _, rm := range []*mask.Mask{nil, mask.ParseMust("spec.region")} {
		if _, err := f.svc.Update(t.Context(), domain.UpdateProjectCmd{Metadata: md, ResetMask: rm}); err != nil {
			t.Fatalf("Update with empty region: %v", err)
		}
		got, _ := f.svc.Get(t.Context(), id)
		if got.GetSpec().GetRegion() != "eu-north1" || got.GetMetadata().GetResourceVersion() != 1 {
			t.Fatalf("after empty-region update: spec %v version %d", got.GetSpec(), got.GetMetadata().GetResourceVersion())
		}
	}
}

func TestProjectUpdateRejections(t *testing.T) {
	t.Parallel()
	f := newProjectFixture(t)
	p := f.create(t, "a", "", nil)
	id := p.GetMetadata().GetId()
	tests := []struct {
		name string
		md   *commonpb.ResourceMetadata
		want errs.Kind
	}{
		{"missing id", &commonpb.ResourceMetadata{Name: "a"}, errs.KindBadRequest},
		{"unknown id", &commonpb.ResourceMetadata{Id: "project-nope", Name: "a"}, errs.KindNotFound},
		{"name required", &commonpb.ResourceMetadata{Id: id}, errs.KindBadRequest},
		{"parent change", &commonpb.ResourceMetadata{Id: id, ParentId: "tenant-other", Name: "a"}, errs.KindBadRequest},
		{"stale resource_version", &commonpb.ResourceMetadata{Id: id, Name: "a", ResourceVersion: 7}, errs.KindConflict},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cmd := domain.UpdateProjectCmd{Metadata: tt.md}
			if tt.name == "name required" {
				// A reset mask on the name empties it.
				cmd.ResetMask = mask.ParseMust("metadata.name")
			}
			_, err := f.svc.Update(t.Context(), cmd)
			if !errs.IsKind(err, tt.want) {
				t.Fatalf("err = %v, want kind %v", err, tt.want)
			}
		})
	}
}

func TestProjectDelete(t *testing.T) {
	t.Parallel()
	f := newProjectFixture(t)
	empty := f.create(t, "empty", "", nil).GetMetadata().GetId()
	busy := f.create(t, "busy", "", nil).GetMetadata().GetId()
	// An internal (non-resource) record does not keep a project alive.
	f.repo.children[empty] = []string{"accesskeyindex"}
	f.repo.children[busy] = []string{"accesskeyindex", "vpcnetwork"}

	if _, err := f.svc.Delete(t.Context(), domain.DeleteProjectCmd{ID: "", DryRun: false}); !errs.IsKind(err, errs.KindBadRequest) {
		t.Errorf("empty id err = %v, want BadRequest", err)
	}
	if _, err := f.svc.Delete(t.Context(), domain.DeleteProjectCmd{ID: "project-nope", DryRun: false}); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("unknown id err = %v, want NotFound", err)
	}
	if _, err := f.svc.Delete(t.Context(), domain.DeleteProjectCmd{ID: busy, DryRun: false}); !errs.IsKind(err, errs.KindBadState) {
		t.Fatalf("non-empty err = %v, want BadState", err)
	}
	if _, err := f.svc.Get(t.Context(), busy); err != nil {
		t.Errorf("refused delete removed the project: %v", err)
	}

	if _, err := f.svc.Delete(t.Context(), domain.DeleteProjectCmd{ID: empty, DryRun: true}); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if _, err := f.svc.Get(t.Context(), empty); err != nil {
		t.Fatalf("dry run deleted the project: %v", err)
	}
	if _, err := f.svc.Delete(t.Context(), domain.DeleteProjectCmd{ID: busy, DryRun: true}); !errs.IsKind(err, errs.KindBadState) {
		t.Errorf("dry run on non-empty err = %v, want BadState", err)
	}

	if _, err := f.svc.Delete(t.Context(), domain.DeleteProjectCmd{ID: empty, DryRun: false}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := f.svc.Get(t.Context(), empty); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("Get after delete err = %v, want NotFound", err)
	}
}
