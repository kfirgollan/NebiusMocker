package domain

import (
	"slices"
	"testing"
	"time"

	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

// nvlSpecGB200 is baseSpec on a gpu-gb200 preset in testNVLGroup.
func nvlSpecGB200(disk ...string) *computepb.InstanceSpec {
	s := baseSpec()
	if len(disk) > 0 {
		s.BootDisk = managedDisk(disk[0], 20)
	}
	s.Resources = &computepb.ResourcesSpec{Platform: "gpu-gb200", Size: presetSize("4gpu-128vcpu-1600gb")}
	s.NvlInstanceGroupId = testNVLGroup
	return s
}

func (f *instFixture) nvlMembers(t *testing.T) []string {
	t.Helper()
	g, err := f.nvl.Get(t.Context(), testNVLGroup)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(g.GetStatus().GetInstances()))
	for id := range g.GetStatus().GetInstances() {
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}

func TestInstanceCreateJoinsNVLGroup(t *testing.T) {
	t.Parallel()
	f := newInstFixture(t)
	inst := f.mustCreate(t, "vm", nvlSpecGB200())
	id := inst.GetMetadata().GetId()
	if got := f.nvlMembers(t); !slices.Equal(got, []string{id}) {
		t.Fatalf("group members = %v, want [%s]", got, id)
	}
	if inst.GetSpec().GetNvlInstanceGroupId() != testNVLGroup {
		t.Fatalf("spec = %v, want the group echoed", inst.GetSpec())
	}
}

func TestInstanceCreateNVLValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		edit      func(*computepb.InstanceSpec)
		wantKind  errs.Kind
		wantField string
	}{
		{"recovery ALWAYS with group", func(s *computepb.InstanceSpec) { s.RecoveryPolicy = computepb.InstanceRecoveryPolicy_ALWAYS }, errs.KindUnknown, ""},
		{"recovery ALWAYS without group", func(s *computepb.InstanceSpec) {
			s.RecoveryPolicy = computepb.InstanceRecoveryPolicy_ALWAYS
			s.NvlInstanceGroupId = ""
		}, errs.KindBadRequest, "spec.recovery_policy"},
		{"unknown group", func(s *computepb.InstanceSpec) { s.NvlInstanceGroupId = "computenvlinstancegroup-nope" }, errs.KindNotFound, ""},
		{"wrong platform type", func(s *computepb.InstanceSpec) {
			s.Resources = &computepb.ResourcesSpec{Platform: "gpu-gb300", Size: presetSize("4gpu-128vcpu-1600gb")}
		}, errs.KindBadRequest, "spec.nvl_instance_group_id"},
		{"cpu platform", func(s *computepb.InstanceSpec) { s.Resources = baseSpec().GetResources() }, errs.KindBadRequest, "spec.nvl_instance_group_id"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newInstFixture(t)
			_, err := f.create("vm", edited(nvlSpecGB200(), tt.edit))
			if got := errs.KindOf(err); got != tt.wantKind {
				t.Fatalf("kind = %v, want %v (%v)", got, tt.wantKind, err)
			}
			if tt.wantField != "" && !slices.Contains(violationFields(err), tt.wantField) {
				t.Fatalf("violations = %v, want %s", violationFields(err), tt.wantField)
			}
			if err != nil && (f.repo.count() != 0 || len(f.nvlMembers(t)) != 0 || len(f.net.boundNICs()) != 0) {
				t.Fatalf("rejected create left instances %d, members %v, NICs %v", f.repo.count(), f.nvlMembers(t), f.net.boundNICs())
			}
		})
	}
}

func TestInstanceCreateIntoFullNVLGroupLeavesNothing(t *testing.T) {
	t.Parallel()
	f := newInstFixture(t)
	f.mustCreate(t, "vm1", nvlSpecGB200("vm1"))
	f.mustCreate(t, "vm2", nvlSpecGB200("vm2"))
	nics := f.net.boundNICs()
	disks := f.diskCount()

	_, err := f.create("vm3", nvlSpecGB200("vm3"))
	if !errs.IsKind(err, errs.KindBadState) {
		t.Fatalf("third instance: %v, want BadState", err)
	}
	if f.repo.count() != 2 || len(f.nvlMembers(t)) != 2 {
		t.Fatalf("instances %d, members %v, want 2 each", f.repo.count(), f.nvlMembers(t))
	}
	if got := f.net.boundNICs(); !slices.Equal(got, nics) {
		t.Fatalf("NICs bound = %v, want %v: the failed create leaked", got, nics)
	}
	if f.diskCount() != disks {
		t.Fatalf("disks = %d, want %d: the failed create leaked", f.diskCount(), disks)
	}
}

func TestInstanceDeleteLeavesNVLGroup(t *testing.T) {
	t.Parallel()
	f := newInstFixture(t)
	a := f.mustCreate(t, "vm1", nvlSpecGB200("vm1")).GetMetadata().GetId()
	b := f.mustCreate(t, "vm2", nvlSpecGB200("vm2")).GetMetadata().GetId()
	op, err := f.svc.Delete(t.Context(), a, nil)
	if err != nil || op.Err != nil {
		t.Fatalf("delete: %v / %v", err, op)
	}
	if got := f.nvlMembers(t); !slices.Equal(got, []string{b}) {
		t.Fatalf("members after delete = %v, want [%s]", got, b)
	}
	// The freed slot can be reused.
	f.mustCreate(t, "vm3", nvlSpecGB200("vm3"))
	if got := f.nvlMembers(t); len(got) != 2 {
		t.Fatalf("members = %v", got)
	}
}

func TestInstanceUpdateNVLGroupImmutable(t *testing.T) {
	t.Parallel()
	f := newInstFixture(t)
	id := f.mustCreate(t, "vm", nvlSpecGB200()).GetMetadata().GetId()
	for name, to := range map[string]string{"cleared": "", "changed": "computenvlinstancegroup-other"} {
		err := f.update(t, id, func(s *computepb.InstanceSpec) { s.NvlInstanceGroupId = to }, nil)
		if !errs.IsKind(err, errs.KindBadRequest) || !slices.Contains(violationFields(err), "spec.nvl_instance_group_id") {
			t.Fatalf("%s: %v, want BadRequest on spec.nvl_instance_group_id", name, err)
		}
	}
	if got := f.get(t, id).GetSpec().GetNvlInstanceGroupId(); got != testNVLGroup {
		t.Fatalf("group = %q after rejected updates", got)
	}
	if got := f.nvlMembers(t); !slices.Equal(got, []string{id}) {
		t.Fatalf("members = %v", got)
	}
	if err := f.update(t, id, nil, map[string]string{"env": "dev"}); err != nil {
		t.Fatalf("labels update in a group: %v", err)
	}
}

func TestListByNVLInstanceGroup(t *testing.T) {
	t.Parallel()
	f := newInstFixture(t)
	var want []string
	for _, n := range []string{"vm1", "vm2"} {
		want = append(want, f.mustCreate(t, n, nvlSpecGB200(n)).GetMetadata().GetId())
		f.clk.Advance(time.Minute)
	}
	// An instance outside the group must not show up.
	f.mustCreate(t, "other", baseSpec())
	ctx := t.Context()

	page, err := f.svc.ListByNVLInstanceGroup(ctx, testNVLGroup, 0, "")
	if err != nil || page.NextPageToken != "" {
		t.Fatalf("list = %v, %v", page, err)
	}
	var got []string
	for _, i := range page.Items {
		got = append(got, i.GetMetadata().GetId())
	}
	if !slices.Equal(got, want) {
		t.Fatalf("ids = %v, want creation order %v", got, want)
	}

	got = nil
	token := ""
	for range 4 {
		p, err := f.svc.ListByNVLInstanceGroup(ctx, testNVLGroup, 1, token)
		if err != nil || len(p.Items) != 1 {
			t.Fatalf("page = %v, %v", p, err)
		}
		got = append(got, p.Items[0].GetMetadata().GetId())
		if token = p.NextPageToken; token == "" {
			break
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("paged ids = %v, want %v", got, want)
	}

	if _, err := f.svc.ListByNVLInstanceGroup(ctx, "", 0, ""); !errs.IsKind(err, errs.KindBadRequest) ||
		!slices.Contains(violationFields(err), "nvl_instance_group_id") {
		t.Fatalf("empty id: %v", err)
	}
	if _, err := f.svc.ListByNVLInstanceGroup(ctx, "computenvlinstancegroup-nope", 0, ""); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("unknown group: %v", err)
	}
	if _, err := f.svc.ListByNVLInstanceGroup(ctx, testNVLGroup, 1, "garbage!"); !errs.IsKind(err, errs.KindBadRequest) {
		t.Fatalf("bad token: %v", err)
	}
}
