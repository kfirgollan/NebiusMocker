package domain

import (
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/nebius/gosdk/proto/fieldmask/mask"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	quotaspb "github.com/nebius/gosdk/proto/nebius/quotas/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/pkg/quota"
)

func TestNewServiceRequiresDeps(t *testing.T) {
	t.Parallel()
	if _, err := NewService(Deps{}); err == nil {
		t.Fatal("NewService with no deps succeeded")
	}
}

// listAll pages through List until the token is empty.
func listAll(t *testing.T, f *fixture, parent string, pageSize int64) []*quotaspb.QuotaAllowance {
	t.Helper()
	var out []*quotaspb.QuotaAllowance
	token := ""
	for range 100 {
		items, next, err := f.svc.List(t.Context(), parent, pageSize, token)
		if err != nil {
			t.Fatalf("List(%s): %v", parent, err)
		}
		if pageSize > 0 && int64(len(items)) > pageSize {
			t.Fatalf("page of %d items, page size %d", len(items), pageSize)
		}
		out = append(out, items...)
		if next == "" {
			return out
		}
		token = next
	}
	t.Fatal("paging does not terminate")
	return nil
}

func byName(as []*quotaspb.QuotaAllowance, region, name string) *quotaspb.QuotaAllowance {
	for _, a := range as {
		if a.GetMetadata().GetName() == name && a.GetSpec().GetRegion() == region {
			return a
		}
	}
	return nil
}

func TestListProject(t *testing.T) {
	t.Parallel()
	f := newFixture(t, map[string]uint64{quota.ComputeInstanceCount: 10})
	f.usage(peer.QuotaUsage{ContainerID: project1, Name: quota.ComputeInstanceCount, Amount: 5})
	entries := f.svc.catalog.Entries()

	got := listAll(t, f, project1, 0)
	if len(got) != len(entries) {
		t.Fatalf("listed %d allowances, want one per catalog entry (%d)", len(got), len(entries))
	}
	for _, a := range got {
		if a.GetSpec().GetRegion() != regionEU || a.GetMetadata().GetParentId() != project1 {
			t.Errorf("%s: region %q parent %q", a.GetMetadata().GetName(), a.GetSpec().GetRegion(), a.GetMetadata().GetParentId())
		}
		st := a.GetStatus()
		e, _ := f.svc.catalog.Lookup(a.GetMetadata().GetName())
		if st.GetState() != quotaspb.QuotaAllowanceStatus_STATE_ACTIVE || st.GetUnit() != e.Unit ||
			st.GetService() != e.Service || st.GetDescription() != e.Description || st.GetServiceDescription() != e.ServiceDescription {
			t.Errorf("%s: status %v", e.Name, st)
		}
		if a.GetSpec().GetLimit() != e.Default && e.Name != quota.ComputeInstanceCount {
			t.Errorf("%s: limit %d, want default %d", e.Name, a.GetSpec().GetLimit(), e.Default)
		}
	}

	inst := byName(got, regionEU, quota.ComputeInstanceCount)
	if inst.GetSpec().GetLimit() != 10 || inst.GetStatus().GetUsage() != 5 || inst.GetStatus().GetUsagePercentage() != "0.50" ||
		inst.GetStatus().GetUsageState() != quotaspb.QuotaAllowanceStatus_USAGE_STATE_USED {
		t.Errorf("instance count = %v", inst)
	}
	if d := byName(got, regionEU, quota.ComputeDiskCount); d.GetStatus().GetUsageState() != quotaspb.QuotaAllowanceStatus_USAGE_STATE_NOT_USED ||
		d.GetStatus().GetUsagePercentage() != "0.00" {
		t.Errorf("disk count (reported by compute, unused) = %v", d.GetStatus())
	}
	if n := byName(got, regionEU, quota.VPCNetworkCount); n.GetStatus().GetUsageState() != quotaspb.QuotaAllowanceStatus_USAGE_STATE_UNKNOWN {
		t.Errorf("vpc network count (no vpc source) usage state = %v, want UNKNOWN", n.GetStatus().GetUsageState())
	}

	// A sibling project and another region are not mixed in.
	if other := listAll(t, f, project2, 0); byName(other, regionEU, quota.ComputeInstanceCount).GetStatus().GetUsage() != 0 {
		t.Error("project2 shows project1 usage")
	}
	if us := listAll(t, f, projectUS, 0); len(us) != len(entries) || byName(us, regionUS, quota.ComputeInstanceCount) == nil {
		t.Errorf("us project lists %d allowances", len(us))
	}
}

func TestListTenantAndPaging(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	f.usage(
		peer.QuotaUsage{ContainerID: project1, Name: quota.ComputeInstanceCount, Amount: 5},
		peer.QuotaUsage{ContainerID: project2, Name: quota.ComputeInstanceCount, Amount: 3},
		peer.QuotaUsage{ContainerID: projectUS, Name: quota.ComputeInstanceCount, Amount: 2},
	)
	n := len(f.svc.catalog.Entries())

	got := listAll(t, f, tenantID, 7)
	if len(got) != 2*n {
		t.Fatalf("tenant lists %d allowances, want %d (every region)", len(got), 2*n)
	}
	ids := map[string]bool{}
	for _, a := range got {
		ids[a.GetMetadata().GetId()] = true
	}
	if len(ids) != len(got) {
		t.Errorf("paging repeated allowances: %d unique of %d", len(ids), len(got))
	}
	if eu := byName(got, regionEU, quota.ComputeInstanceCount); eu.GetStatus().GetUsage() != 8 {
		t.Errorf("tenant eu usage = %d, want 8 (both eu projects)", eu.GetStatus().GetUsage())
	}
	if us := byName(got, regionUS, quota.ComputeInstanceCount); us.GetStatus().GetUsage() != 2 {
		t.Errorf("tenant us usage = %d, want 2", us.GetStatus().GetUsage())
	}

	for name, tc := range map[string]struct {
		parent string
		size   int64
		kind   errs.Kind
	}{
		"negative size": {project1, -1, errs.KindBadRequest},
		"huge size":     {project1, 100000, errs.KindBadRequest},
		"bad parent":    {"bucket-e00x", 0, errs.KindBadRequest},
		"missing":       {"project-e00nope", 0, errs.KindNotFound},
	} {
		if _, _, err := f.svc.List(t.Context(), tc.parent, tc.size, ""); !errs.IsKind(err, tc.kind) {
			t.Errorf("%s: %v, want %v", name, err, tc.kind)
		}
	}
}

func TestGetByName(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	const name = quota.ComputeInstanceCount
	for _, tc := range []struct {
		desc, parent, name, region string
		kind                       errs.Kind // 0 means success
	}{
		{"project, empty region", project1, name, "", 0},
		{"project, own region", project1, name, regionEU, 0},
		{"tenant with region", tenantID, name, regionUS, 0},
		{"unknown quota", project1, "bogus.quota", "", errs.KindBadRequest},
		{"empty name", project1, "", "", errs.KindBadRequest},
		{"tenant without region", tenantID, name, "", errs.KindBadRequest},
		{"tenant unknown region", tenantID, name, "mars", errs.KindBadRequest},
		{"project wrong region", project1, name, regionUS, errs.KindBadRequest},
		{"bad parent kind", "bucket-e00x", name, "", errs.KindBadRequest},
		{"empty parent", "", name, "", errs.KindBadRequest},
		{"missing container", "project-e00nope", name, "", errs.KindNotFound},
	} {
		got, err := f.svc.GetByName(t.Context(), tc.parent, tc.name, tc.region)
		if tc.kind == 0 {
			if err != nil || got.GetMetadata().GetName() != tc.name || got.GetStatus() == nil {
				t.Errorf("%s: %v, %v", tc.desc, got, err)
			}
			continue
		}
		if !errs.IsKind(err, tc.kind) {
			t.Errorf("%s: %v, want %v", tc.desc, err, tc.kind)
		}
	}
}

func createCmd(parent, name, region string, limit *uint64, labels map[string]string) CreateCmd {
	return CreateCmd{
		Metadata: &commonpb.ResourceMetadata{ParentId: parent, Name: name, Labels: labels},
		Spec:     &quotaspb.QuotaAllowanceSpec{Limit: limit, Region: region},
	}
}

func TestCreateIsUpsert(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	ctx := t.Context()
	const name = quota.ComputeInstanceCount
	before, err := f.svc.GetByName(ctx, project1, name, "")
	if err != nil {
		t.Fatalf("GetByName: %v", err)
	}
	if before.GetMetadata().GetResourceVersion() != 1 || before.GetSpec().GetRegion() != regionEU {
		t.Fatalf("fresh allowance = %v", before)
	}

	op, err := f.svc.Create(ctx, createCmd(project1, name, "", proto.Uint64(3), map[string]string{"k": "v"}))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if op.ResourceID != before.GetMetadata().GetId() {
		t.Errorf("Create id = %s, want existing %s", op.ResourceID, before.GetMetadata().GetId())
	}
	got, err := f.svc.GetByName(ctx, project1, name, "")
	if err != nil {
		t.Fatalf("GetByName: %v", err)
	}
	md := got.GetMetadata()
	if md.GetId() != before.GetMetadata().GetId() || got.GetSpec().GetLimit() != 3 || md.GetLabels()["k"] != "v" || md.GetResourceVersion() != 2 {
		t.Errorf("after Create = %v", got)
	}
	if got.GetSpec().GetRegion() != "" {
		t.Errorf("spec.region = %q, want the empty region as sent", got.GetSpec().GetRegion())
	}
	if byRegion, err := f.svc.GetByName(ctx, project1, name, regionEU); err != nil || byRegion.GetMetadata().GetId() != md.GetId() {
		t.Errorf("GetByName(%s) = %v, %v; want the same allowance", regionEU, byRegion, err)
	}

	// An identical Create changes nothing, a different one bumps the version.
	if _, err := f.svc.Create(ctx, createCmd(project1, name, "", proto.Uint64(3), map[string]string{"k": "v"})); err != nil {
		t.Fatalf("repeat Create: %v", err)
	}
	if again, _ := f.svc.GetByName(ctx, project1, name, ""); again.GetMetadata().GetResourceVersion() != 2 {
		t.Errorf("version after identical Create = %d, want 2", again.GetMetadata().GetResourceVersion())
	}
	if _, err := f.svc.Create(ctx, createCmd(project1, name, "", proto.Uint64(4), map[string]string{"k": "v"})); err != nil {
		t.Fatalf("changing Create: %v", err)
	}
	if again, _ := f.svc.GetByName(ctx, project1, name, ""); again.GetMetadata().GetResourceVersion() != 3 || again.GetSpec().GetLimit() != 4 {
		t.Errorf("after changing Create = %v", again)
	}
}

func TestCreateValidation(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	const name = quota.ComputeInstanceCount
	withID := createCmd(project1, name, "", nil, nil)
	withID.Metadata.Id = "quotaallowance-e00x"
	noSpec := createCmd(project1, name, "", nil, nil)
	noSpec.Spec = nil
	for desc, tc := range map[string]struct {
		cmd  CreateCmd
		kind errs.Kind
	}{
		"id set":          {withID, errs.KindBadRequest},
		"no spec":         {noSpec, errs.KindBadRequest},
		"unknown quota":   {createCmd(project1, "bogus", "", nil, nil), errs.KindBadRequest},
		"tenant no reg.":  {createCmd(tenantID, name, "", nil, nil), errs.KindBadRequest},
		"missing parent":  {createCmd("project-e00nope", name, "", nil, nil), errs.KindNotFound},
		"tenant bad reg.": {createCmd(tenantID, name, "mars", nil, nil), errs.KindBadRequest},
	} {
		if _, err := f.svc.Create(t.Context(), tc.cmd); !errs.IsKind(err, tc.kind) {
			t.Errorf("%s: %v, want %v", desc, err, tc.kind)
		}
	}
}

func updateCmd(id string, version int64, region string, limit *uint64, m *mask.Mask) UpdateCmd {
	return UpdateCmd{
		Metadata:  &commonpb.ResourceMetadata{Id: id, ResourceVersion: version},
		Spec:      &quotaspb.QuotaAllowanceSpec{Limit: limit, Region: region},
		ResetMask: m,
	}
}

func TestUpdate(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	ctx := t.Context()
	const name = quota.ComputeInstanceCount
	a, err := f.svc.GetByName(ctx, project1, name, "")
	if err != nil {
		t.Fatalf("GetByName: %v", err)
	}
	id := a.GetMetadata().GetId()
	def := a.GetSpec().GetLimit()

	if _, err := f.svc.Update(ctx, updateCmd(id, 1, "", proto.Uint64(7), nil)); err != nil {
		t.Fatalf("Update limit: %v", err)
	}
	got, _ := f.svc.Get(ctx, id)
	if got.GetSpec().GetLimit() != 7 || got.GetMetadata().GetResourceVersion() != 2 || got.GetSpec().GetRegion() != regionEU {
		t.Errorf("after limit update = %v", got)
	}

	// No limit in the request and no reset mask keeps the limit.
	if _, err := f.svc.Update(ctx, updateCmd(id, 0, "", nil, nil)); err != nil {
		t.Fatalf("Update without limit: %v", err)
	}
	if got, _ := f.svc.Get(ctx, id); got.GetSpec().GetLimit() != 7 || got.GetMetadata().GetResourceVersion() != 2 {
		t.Errorf("empty update changed the allowance: %v", got)
	}

	if _, err := f.svc.Update(ctx, updateCmd(id, 0, regionUS, proto.Uint64(9), nil)); !errs.IsKind(err, errs.KindBadRequest) {
		t.Errorf("region change: %v, want BadRequest", err)
	}
	if _, err := f.svc.Update(ctx, updateCmd(id, 99, "", proto.Uint64(9), nil)); !errs.IsKind(err, errs.KindConflict) {
		t.Errorf("stale version: %v, want Conflict", err)
	}
	if _, err := f.svc.Update(ctx, updateCmd("", 0, "", proto.Uint64(9), nil)); !errs.IsKind(err, errs.KindBadRequest) {
		t.Errorf("no id: %v, want BadRequest", err)
	}
	if _, err := f.svc.Update(ctx, updateCmd("quotaallowance-e00nope", 0, "", proto.Uint64(9), nil)); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("unknown id: %v, want NotFound", err)
	}
	if got, _ := f.svc.Get(ctx, id); got.GetSpec().GetLimit() != 7 {
		t.Errorf("rejected updates changed the limit to %d", got.GetSpec().GetLimit())
	}

	if _, err := f.svc.Update(ctx, updateCmd(id, 0, "", nil, mask.ParseMust("spec.limit"))); err != nil {
		t.Fatalf("Update with reset mask: %v", err)
	}
	got, _ = f.svc.Get(ctx, id)
	if got.GetSpec().GetLimit() != def {
		t.Errorf("limit after spec.limit reset = %d, want default %d", got.GetSpec().GetLimit(), def)
	}
	if stored, _ := f.repo.Get(ctx, id); stored.Msg.GetSpec().Limit != nil {
		t.Errorf("stored limit after reset = %d, want unset", stored.Msg.GetSpec().GetLimit())
	}
}

func TestDeleteResetsToDefault(t *testing.T) {
	t.Parallel()
	f := newFixture(t, map[string]uint64{quota.ComputeInstanceCount: 50})
	ctx := t.Context()
	const name = quota.ComputeInstanceCount
	op, err := f.svc.Create(ctx, createCmd(project1, name, "", proto.Uint64(3), nil))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := f.svc.Delete(ctx, op.ResourceID, nil); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	got, err := f.svc.Get(ctx, op.ResourceID)
	if err != nil {
		t.Fatalf("Get after Delete: %v", err)
	}
	if got.GetSpec().GetLimit() != 50 {
		t.Errorf("limit after Delete = %d, want catalog default 50", got.GetSpec().GetLimit())
	}
	if _, err := f.svc.Delete(ctx, "quotaallowance-e00nope", nil); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("Delete unknown: %v, want NotFound", err)
	}
}

func TestPercentage(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		used, limit uint64
		want        string
	}{
		{5, 10, "0.50"}, {0, 10, "0.00"}, {1, 3, "0.33"}, {20, 10, "2.00"}, {1, 0, "1.00"}, {0, 0, "0.00"},
	} {
		if got := percentage(tc.used, tc.limit); got != tc.want {
			t.Errorf("percentage(%d, %d) = %q, want %q", tc.used, tc.limit, got, tc.want)
		}
	}
}
