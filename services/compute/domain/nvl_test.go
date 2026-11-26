package domain

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

// fakeInstanceReader serves instances by ID; absent IDs are NotFound.
type fakeInstanceReader map[string]*computepb.Instance

func (f fakeInstanceReader) Get(_ context.Context, id string) (*computepb.Instance, error) {
	i, ok := f[id]
	if !ok {
		return nil, errs.NotFound(id)
	}
	return i, nil
}

func nvlInstance(id string, state computepb.InstanceStatus_InstanceState, reconciling bool) *computepb.Instance {
	return &computepb.Instance{
		Metadata: &commonpb.ResourceMetadata{Id: id},
		Status:   &computepb.InstanceStatus{State: state, Reconciling: reconciling},
	}
}

type nvlFixture struct {
	groups    *fakeNVLGroups
	instances fakeInstanceReader
	ops       *deferOps
	clk       *clock.Manual
	svc       *NVLInstanceGroupService
	members   *NVLMembers
}

func newNVLPlatforms(t *testing.T) *PlatformService {
	t.Helper()
	catalog, err := DefaultPlatformCatalog()
	if err != nil {
		t.Fatal(err)
	}
	p, err := NewPlatformService(PlatformDeps{
		Catalog: catalog, Regions: testRegions, Projects: fakeProjects{testProject: true, otherProject: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func newNVLFixture(t *testing.T) *nvlFixture {
	t.Helper()
	f := &nvlFixture{
		groups: &fakeNVLGroups{}, instances: fakeInstanceReader{}, ops: &deferOps{},
		clk: clock.NewManual(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
	}
	var err error
	f.svc, err = NewNVLInstanceGroupService(NVLInstanceGroupDeps{
		Groups: f.groups, Instances: f.instances, Projects: fakeProjects{testProject: true, otherProject: true},
		Ops: f.ops, IDs: &fakeIDs{}, Clock: f.clk,
	})
	if err != nil {
		t.Fatal(err)
	}
	if f.members, err = NewNVLMembers(f.groups, newNVLPlatforms(t)); err != nil {
		t.Fatal(err)
	}
	return f
}

func nvlSpec(typ computepb.NVLInstanceGroupSpec_NVLInstanceGroupType, size int64) *computepb.NVLInstanceGroupSpec {
	return &computepb.NVLInstanceGroupSpec{Type: typ, Size: size}
}

func (f *nvlFixture) create(t *testing.T, name string, size int64) string {
	t.Helper()
	op, err := f.svc.Create(t.Context(), CreateNVLInstanceGroupCmd{
		Metadata: &commonpb.ResourceMetadata{ParentId: testProject, Name: name},
		Spec:     nvlSpec(computepb.NVLInstanceGroupSpec_GB200, size),
	})
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	return op.ResourceID
}

func (f *nvlFixture) get(t *testing.T, id string) *computepb.NVLInstanceGroup {
	t.Helper()
	g, err := f.groups.Get(t.Context(), id)
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	return g
}

func TestNVLInstanceGroupCreateValidation(t *testing.T) {
	t.Parallel()
	gb200 := computepb.NVLInstanceGroupSpec_GB200
	tests := []struct {
		name      string
		md        *commonpb.ResourceMetadata
		spec      *computepb.NVLInstanceGroupSpec
		wantKind  errs.Kind
		wantField string
	}{
		{name: "ok", md: &commonpb.ResourceMetadata{ParentId: testProject, Name: "ok"}, spec: nvlSpec(gb200, 4)},
		{name: "parent missing", md: &commonpb.ResourceMetadata{}, spec: nvlSpec(gb200, 4), wantKind: errs.KindBadRequest, wantField: "metadata.parent_id"},
		{name: "id set", md: &commonpb.ResourceMetadata{Id: "x", ParentId: testProject}, spec: nvlSpec(gb200, 4), wantKind: errs.KindBadRequest, wantField: "metadata.id"},
		{name: "type unspecified", md: &commonpb.ResourceMetadata{ParentId: testProject}, spec: nvlSpec(computepb.NVLInstanceGroupSpec_UNSPECIFIED, 4), wantKind: errs.KindBadRequest, wantField: "spec.type"},
		{name: "type unknown", md: &commonpb.ResourceMetadata{ParentId: testProject}, spec: nvlSpec(99, 4), wantKind: errs.KindBadRequest, wantField: "spec.type"},
		{name: "size zero", md: &commonpb.ResourceMetadata{ParentId: testProject}, spec: nvlSpec(gb200, 0), wantKind: errs.KindBadRequest, wantField: "spec.size"},
		{name: "size negative", md: &commonpb.ResourceMetadata{ParentId: testProject}, spec: nvlSpec(gb200, -1), wantKind: errs.KindBadRequest, wantField: "spec.size"},
		{name: "spec nil", md: &commonpb.ResourceMetadata{ParentId: testProject}, wantKind: errs.KindBadRequest, wantField: "spec.type"},
		{name: "project unknown", md: &commonpb.ResourceMetadata{ParentId: "project-e00mock00000000unknown"}, spec: nvlSpec(gb200, 4), wantKind: errs.KindNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newNVLFixture(t)
			op, err := f.svc.Create(t.Context(), CreateNVLInstanceGroupCmd{Metadata: tt.md, Spec: tt.spec})
			if got := errs.KindOf(err); got != tt.wantKind {
				t.Fatalf("kind = %v, want %v (err: %v)", got, tt.wantKind, err)
			}
			if err != nil && op != nil {
				t.Fatalf("operation returned next to error %v", err)
			}
			if tt.wantField != "" && !slices.Contains(violationFields(err), tt.wantField) {
				t.Fatalf("violations = %v, want field %s", violationFields(err), tt.wantField)
			}
		})
	}
}

func TestNVLInstanceGroupCreateReconciles(t *testing.T) {
	t.Parallel()
	f := newNVLFixture(t)
	f.ops.deferred = true
	id := f.create(t, "g1", 3)
	want := nvlSpec(computepb.NVLInstanceGroupSpec_GB200, 3)
	g := f.get(t, id)
	if !g.GetStatus().GetReconciling() || g.GetMetadata().GetResourceVersion() != 1 || g.GetMetadata().GetParentId() != testProject ||
		!proto.Equal(g.GetSpec(), want) {
		t.Fatalf("group before op work = %v", g)
	}
	if !g.GetMetadata().GetCreatedAt().AsTime().Equal(f.clk.Now()) {
		t.Fatalf("created_at = %v", g.GetMetadata().GetCreatedAt())
	}
	f.ops.finish()
	g = f.get(t, id)
	if g.GetStatus().GetReconciling() || !proto.Equal(g.GetSpec(), want) {
		t.Fatalf("group after op work = %v", g)
	}
}

func TestNVLInstanceGroupReads(t *testing.T) {
	t.Parallel()
	f := newNVLFixture(t)
	for _, n := range []string{"a", "b", "c"} {
		f.create(t, n, 2)
	}
	id := f.create(t, "d", 2)
	ctx := t.Context()
	if got, err := f.svc.Get(ctx, id); err != nil || got.GetMetadata().GetName() != "d" {
		t.Fatalf("Get = %v, %v", got, err)
	}
	if got, err := f.svc.GetByName(ctx, testProject, "d"); err != nil || got.GetMetadata().GetId() != id {
		t.Fatalf("GetByName = %v, %v", got, err)
	}
	if _, err := f.svc.GetByName(ctx, testProject, "nope"); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("GetByName missing: %v", err)
	}
	if _, err := f.svc.GetByName(ctx, "", "d"); !errs.IsKind(err, errs.KindBadRequest) {
		t.Fatalf("GetByName without parent: %v", err)
	}
	if _, err := f.svc.Get(ctx, ""); !errs.IsKind(err, errs.KindBadRequest) {
		t.Fatalf("Get empty id: %v", err)
	}
	if _, err := f.svc.Get(ctx, "computenvlinstancegroup-missing"); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("Get missing: %v", err)
	}

	var names []string
	token := ""
	for range 5 {
		p, err := f.svc.List(ctx, testProject, 3, token)
		if err != nil {
			t.Fatal(err)
		}
		for _, g := range p.Items {
			names = append(names, g.GetMetadata().GetName())
		}
		if token = p.NextPageToken; token == "" {
			break
		}
	}
	if !slices.Equal(names, []string{"a", "b", "c", "d"}) {
		t.Fatalf("names = %v", names)
	}
	if _, err := f.svc.List(ctx, "", 0, ""); !errs.IsKind(err, errs.KindBadRequest) {
		t.Fatalf("List without parent: %v", err)
	}
	if p, err := f.svc.List(ctx, otherProject, 0, ""); err != nil || len(p.Items) != 0 {
		t.Fatalf("List other project = %v, %v", p, err)
	}
}

func TestNVLInstanceGroupUpdate(t *testing.T) {
	t.Parallel()
	f := newNVLFixture(t)
	id := f.create(t, "g1", 2)
	if err := f.members.Join(t.Context(), id, "computeinstance-1", testProject, "gpu-gb200"); err != nil {
		t.Fatal(err)
	}
	before := f.get(t, id)
	f.clk.Advance(time.Hour)
	gb200 := computepb.NVLInstanceGroupSpec_GB200

	update := func(md *commonpb.ResourceMetadata, spec *computepb.NVLInstanceGroupSpec) error {
		md.Id = id
		_, err := f.svc.Update(t.Context(), UpdateNVLInstanceGroupCmd{Metadata: md, Spec: spec})
		return err
	}

	tests := []struct {
		name      string
		spec      *computepb.NVLInstanceGroupSpec
		wantField string
	}{
		{name: "type change", spec: nvlSpec(computepb.NVLInstanceGroupSpec_GB300, 2), wantField: "spec.type"},
		{name: "size change", spec: nvlSpec(gb200, 3), wantField: "spec.size"},
	}
	for _, tt := range tests {
		err := update(&commonpb.ResourceMetadata{Name: "g1"}, tt.spec)
		if !errs.IsKind(err, errs.KindBadRequest) || !slices.Contains(violationFields(err), tt.wantField) || !strings.Contains(err.Error(), "field is immutable") {
			t.Fatalf("%s: %v", tt.name, err)
		}
		if !proto.Equal(before, f.get(t, id)) {
			t.Fatalf("%s: rejected update changed the group", tt.name)
		}
	}

	if err := update(&commonpb.ResourceMetadata{Name: "g1"}, nvlSpec(gb200, 2)); err != nil {
		t.Fatalf("no-op update: %v", err)
	}
	if !proto.Equal(before, f.get(t, id)) {
		t.Fatalf("no-op update changed the group: %v", f.get(t, id))
	}

	// Empty type and size keep the stored values; name and labels change.
	if err := update(&commonpb.ResourceMetadata{Name: "g2", Labels: map[string]string{"env": "dev"}}, &computepb.NVLInstanceGroupSpec{}); err != nil {
		t.Fatalf("update: %v", err)
	}
	got := f.get(t, id)
	if got.GetMetadata().GetName() != "g2" || got.GetMetadata().GetLabels()["env"] != "dev" ||
		got.GetMetadata().GetResourceVersion() != before.GetMetadata().GetResourceVersion()+1 ||
		!got.GetMetadata().GetUpdatedAt().AsTime().Equal(f.clk.Now()) {
		t.Fatalf("metadata after update = %v", got.GetMetadata())
	}
	if !proto.Equal(got.GetSpec(), before.GetSpec()) {
		t.Fatalf("spec = %v, want unchanged %v", got.GetSpec(), before.GetSpec())
	}
	if _, ok := got.GetStatus().GetInstances()["computeinstance-1"]; !ok {
		t.Fatalf("status = %v, want the member kept", got.GetStatus())
	}

	// resource_version guards the update.
	if err := update(&commonpb.ResourceMetadata{Name: "g3", ResourceVersion: 99}, nil); !errs.IsKind(err, errs.KindConflict) {
		t.Fatalf("stale resource_version: %v", err)
	}
	if err := update(&commonpb.ResourceMetadata{Name: "g3", ResourceVersion: got.GetMetadata().GetResourceVersion()}, nil); err != nil {
		t.Fatalf("matching resource_version: %v", err)
	}

	if _, err := f.svc.Update(t.Context(), UpdateNVLInstanceGroupCmd{Metadata: &commonpb.ResourceMetadata{}}); !errs.IsKind(err, errs.KindBadRequest) {
		t.Fatalf("update without id: %v", err)
	}
	if _, err := f.svc.Update(t.Context(), UpdateNVLInstanceGroupCmd{Metadata: &commonpb.ResourceMetadata{Id: "computenvlinstancegroup-missing"}}); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("update missing: %v", err)
	}
}

func TestNVLInstanceGroupDeleteRefusedWhileMembers(t *testing.T) {
	t.Parallel()
	f := newNVLFixture(t)
	id := f.create(t, "g1", 2)
	if err := f.members.Join(t.Context(), id, "computeinstance-1", testProject, "gpu-gb200"); err != nil {
		t.Fatal(err)
	}
	op, err := f.svc.Delete(t.Context(), id, nil)
	if !errs.IsKind(err, errs.KindBadState) || !strings.Contains(err.Error(), "computeinstance-1") {
		t.Fatalf("delete non-empty: op=%v err=%v, want BadState naming the instance", op, err)
	}
	if _, err := f.groups.Get(t.Context(), id); err != nil {
		t.Fatalf("group gone after refused delete: %v", err)
	}
	if err := f.members.Leave(t.Context(), id, "computeinstance-1"); err != nil {
		t.Fatal(err)
	}
	op, err = f.svc.Delete(t.Context(), id, nil)
	if err != nil || op.Err != nil {
		t.Fatalf("delete empty: %v / %v", err, op)
	}
	if _, err := f.groups.Get(t.Context(), id); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("group after delete: %v", err)
	}
	if _, err := f.svc.Delete(t.Context(), id, nil); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("delete twice: %v", err)
	}
	if _, err := f.svc.Delete(t.Context(), "", nil); !errs.IsKind(err, errs.KindBadRequest) {
		t.Fatalf("delete empty id: %v", err)
	}
}

func TestNVLInstanceGroupReadFillsMemberStates(t *testing.T) {
	t.Parallel()
	f := newNVLFixture(t)
	id := f.create(t, "g1", 4)
	for _, m := range []string{"computeinstance-run", "computeinstance-gone"} {
		if err := f.members.Join(t.Context(), id, m, testProject, "gpu-gb200"); err != nil {
			t.Fatal(err)
		}
	}
	f.instances["computeinstance-run"] = nvlInstance("computeinstance-run", computepb.InstanceStatus_RUNNING, false)

	g, err := f.svc.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	inst := g.GetStatus().GetInstances()
	if len(inst) != 1 || inst["computeinstance-run"].GetInstanceState() != computepb.InstanceStatus_RUNNING {
		t.Fatalf("status.instances = %v, want only the running member", inst)
	}
	if g.GetStatus().GetReconciling() {
		t.Fatal("reconciling set with all members settled")
	}
	if stored := f.get(t, id).GetStatus().GetInstances(); len(stored) != 2 {
		t.Fatalf("read changed the stored members: %v", stored)
	}

	f.instances["computeinstance-run"] = nvlInstance("computeinstance-run", computepb.InstanceStatus_STARTING, true)
	reads := map[string]func() (*computepb.NVLInstanceGroup, error){
		"Get":       func() (*computepb.NVLInstanceGroup, error) { return f.svc.Get(t.Context(), id) },
		"GetByName": func() (*computepb.NVLInstanceGroup, error) { return f.svc.GetByName(t.Context(), testProject, "g1") },
		"List": func() (*computepb.NVLInstanceGroup, error) {
			p, err := f.svc.List(t.Context(), testProject, 0, "")
			if err != nil || len(p.Items) != 1 {
				return nil, err
			}
			return p.Items[0], nil
		},
	}
	for name, read := range reads {
		g, err := read()
		if err != nil || g == nil {
			t.Fatalf("%s: %v, %v", name, g, err)
		}
		if !g.GetStatus().GetReconciling() || g.GetStatus().GetInstances()["computeinstance-run"].GetInstanceState() != computepb.InstanceStatus_STARTING {
			t.Fatalf("%s: status = %v, want reconciling with STARTING member", name, g.GetStatus())
		}
	}
}

func TestNVLMembersJoin(t *testing.T) {
	t.Parallel()
	f := newNVLFixture(t)
	id := f.create(t, "g1", 1)
	tests := []struct {
		name                   string
		group, inst, proj, plt string
		wantKind               errs.Kind
		wantField              string
	}{
		{"missing group", "computenvlinstancegroup-nope", "i", testProject, "gpu-gb200", errs.KindNotFound, "spec.nvl_instance_group_id"},
		{"no group id", "", "i", testProject, "gpu-gb200", errs.KindBadRequest, "spec.nvl_instance_group_id"},
		{"no instance id", id, "", testProject, "gpu-gb200", errs.KindBadRequest, "metadata.id"},
		{"other project", id, "i", otherProject, "gpu-gb200", errs.KindBadRequest, "spec.nvl_instance_group_id"},
		{"gb300 into GB200", id, "i", testProject, "gpu-gb300", errs.KindBadRequest, "spec.nvl_instance_group_id"},
		{"cpu platform", id, "i", testProject, "cpu-e2", errs.KindBadRequest, "spec.nvl_instance_group_id"},
	}
	for _, tt := range tests {
		err := f.members.Join(t.Context(), tt.group, tt.inst, tt.proj, tt.plt)
		if errs.KindOf(err) != tt.wantKind {
			t.Errorf("%s: kind = %v, want %v (%v)", tt.name, errs.KindOf(err), tt.wantKind, err)
			continue
		}
		if !strings.Contains(err.Error(), tt.wantField) && !slices.Contains(violationFields(err), tt.wantField) {
			t.Errorf("%s: %v does not name %s", tt.name, err, tt.wantField)
		}
	}
	if len(f.get(t, id).GetStatus().GetInstances()) != 0 {
		t.Fatal("a rejected Join added a member")
	}

	if err := f.members.Join(t.Context(), id, "i1", testProject, "gpu-gb200"); err != nil {
		t.Fatal(err)
	}
	if err := f.members.Join(t.Context(), id, "i1", testProject, "gpu-gb200"); err != nil {
		t.Fatalf("idempotent re-join into a full group: %v", err)
	}
	if err := f.members.Join(t.Context(), id, "i2", testProject, "gpu-gb200"); !errs.IsKind(err, errs.KindBadState) {
		t.Fatalf("join full group: %v", err)
	}
	if got, err := f.members.Members(t.Context(), id); err != nil || !slices.Equal(got, []string{"i1"}) {
		t.Fatalf("Members = %v, %v", got, err)
	}
}

func TestNVLMembersLeaveAndMembers(t *testing.T) {
	t.Parallel()
	f := newNVLFixture(t)
	id := f.create(t, "g1", 3)
	for _, m := range []string{"i3", "i1", "i2"} {
		if err := f.members.Join(t.Context(), id, m, testProject, "gpu-gb200"); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := f.members.Members(t.Context(), id); err != nil || !slices.Equal(got, []string{"i1", "i2", "i3"}) {
		t.Fatalf("Members = %v, %v, want sorted", got, err)
	}
	if err := f.members.Leave(t.Context(), id, "i2"); err != nil {
		t.Fatal(err)
	}
	if err := f.members.Leave(t.Context(), id, "i2"); err != nil {
		t.Fatalf("second Leave: %v", err)
	}
	if err := f.members.Leave(t.Context(), "computenvlinstancegroup-missing", "i1"); err != nil {
		t.Fatalf("Leave of a missing group: %v", err)
	}
	if err := f.members.Leave(t.Context(), "", "i1"); err != nil {
		t.Fatalf("Leave without a group: %v", err)
	}
	if got, err := f.members.Members(t.Context(), id); err != nil || !slices.Equal(got, []string{"i1", "i3"}) {
		t.Fatalf("Members after leave = %v, %v", got, err)
	}
	if _, err := f.members.Members(t.Context(), "computenvlinstancegroup-missing"); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("Members of a missing group: %v", err)
	}
}

func TestNVLPlatformTypes(t *testing.T) {
	t.Parallel()
	p := newNVLPlatforms(t)
	tests := []struct {
		platform string
		want     computepb.NVLInstanceGroupSpec_NVLInstanceGroupType
	}{
		{"gpu-gb200", computepb.NVLInstanceGroupSpec_GB200},
		{"gpu-gb300", computepb.NVLInstanceGroupSpec_GB300},
		{"cpu-e2", computepb.NVLInstanceGroupSpec_UNSPECIFIED},
		{"unknown", computepb.NVLInstanceGroupSpec_UNSPECIFIED},
	}
	for _, tt := range tests {
		if got := p.NVLInstanceGroupType(tt.platform); got != tt.want {
			t.Errorf("NVLInstanceGroupType(%q) = %v, want %v", tt.platform, got, tt.want)
		}
	}
}

func TestParsePlatformCatalogNVLType(t *testing.T) {
	t.Parallel()
	doc := func(typ string) []byte {
		return []byte("platforms: [{name: a, nvl_instance_group_type: " + typ + ", presets: [{name: p, vcpu_count: 1, memory_gibibytes: 1}]}]")
	}
	if _, err := ParsePlatformCatalog(doc("GB200")); err != nil {
		t.Fatalf("GB200: %v", err)
	}
	for _, typ := range []string{"BOGUS", "UNSPECIFIED"} {
		if _, err := ParsePlatformCatalog(doc(typ)); err == nil {
			t.Errorf("nvl_instance_group_type %s accepted", typ)
		}
	}
}

func TestNewNVLServicesRequireDependencies(t *testing.T) {
	t.Parallel()
	if _, err := NewNVLInstanceGroupService(NVLInstanceGroupDeps{}); err == nil {
		t.Fatal("accepted empty dependencies")
	}
	if _, err := NewNVLMembers(nil, nil); err == nil {
		t.Fatal("NewNVLMembers accepted nil dependencies")
	}
}
