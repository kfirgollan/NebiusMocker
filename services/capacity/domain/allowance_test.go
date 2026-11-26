package domain_test

import (
	"errors"
	"testing"
	"time"

	capacitypb "github.com/nebius/gosdk/proto/nebius/capacity/v1"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/services/capacity/domain"
)

func TestCreateAllowance(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		limit *uint64
	}{
		{"below the group size", u64(8)},
		{"above the group size", u64(1000)},
		{"zero", u64(0)},
		{"no limit", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, t0)
			id := e.newAllowance(t, projectID, gid(t, "a"), tt.limit)
			a := e.allowance(t, id)
			md := a.GetMetadata()
			if md.GetId() != id || md.GetParentId() != projectID || md.GetResourceVersion() != 1 {
				t.Errorf("metadata id, parent, version = %s, %s, %d; want %s, %s, 1", md.GetId(), md.GetParentId(), md.GetResourceVersion(), id, projectID)
			}
			if !md.GetCreatedAt().AsTime().Equal(t0) {
				t.Errorf("created_at = %s, want %s", md.GetCreatedAt().AsTime(), t0)
			}
			if a.GetSpec().GetCapacityBlockGroupId() != gid(t, "a") {
				t.Errorf("capacity_block_group_id = %s, want %s", a.GetSpec().GetCapacityBlockGroupId(), gid(t, "a"))
			}
			if (a.GetSpec().Limit == nil) != (tt.limit == nil) || (tt.limit != nil && a.GetSpec().GetLimit() != *tt.limit) {
				t.Errorf("limit = %v, want %v", a.GetSpec().Limit, tt.limit)
			}
			st := a.GetStatus()
			if st.GetState() != capacitypb.CapacityAllowanceStatus_STATE_ACTIVE || st.GetUnit() != "GPU" ||
				st.GetUsage() != 0 || st.GetUsageState() != capacitypb.CapacityAllowanceStatus_USAGE_STATE_NOT_USED {
				t.Errorf("status = %v, want ACTIVE, GPU, unused", st)
			}
		})
	}
}

func TestCreateAllowanceKeepsNameAndLabels(t *testing.T) {
	t.Parallel()
	e := newEnv(t, t0)
	id := done(t)(e.svc.CreateAllowance(t.Context(), domain.CreateAllowanceCmd{
		Metadata: &commonpb.ResourceMetadata{ParentId: projectID, Name: "gpus", Labels: map[string]string{"team": "ml"}},
		Spec:     &capacitypb.CapacityAllowanceSpec{CapacityBlockGroupId: gid(t, "a")},
	}))
	md := e.allowance(t, id).GetMetadata()
	if md.GetName() != "gpus" || md.GetLabels()["team"] != "ml" {
		t.Errorf("name, labels = %q, %v; want gpus, team=ml", md.GetName(), md.GetLabels())
	}
}

func TestCreateAllowanceErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		md   *commonpb.ResourceMetadata
		spec *capacitypb.CapacityAllowanceSpec
		want errs.Kind
	}{
		{
			"id set", &commonpb.ResourceMetadata{ParentId: projectID, Id: "capacityallowance-x"},
			&capacitypb.CapacityAllowanceSpec{CapacityBlockGroupId: gid(t, "a")}, errs.KindBadRequest,
		},
		{
			"resource_version set", &commonpb.ResourceMetadata{ParentId: projectID, ResourceVersion: 3},
			&capacitypb.CapacityAllowanceSpec{CapacityBlockGroupId: gid(t, "a")}, errs.KindBadRequest,
		},
		{
			"parent is not a project", &commonpb.ResourceMetadata{ParentId: tenantID},
			&capacitypb.CapacityAllowanceSpec{CapacityBlockGroupId: gid(t, "a")}, errs.KindBadRequest,
		},
		{
			"unknown project", &commonpb.ResourceMetadata{ParentId: "project-e00unknown"},
			&capacitypb.CapacityAllowanceSpec{CapacityBlockGroupId: gid(t, "a")}, errs.KindNotFound,
		},
		{
			"unknown block group", &commonpb.ResourceMetadata{ParentId: projectID},
			&capacitypb.CapacityAllowanceSpec{CapacityBlockGroupId: "capacityblockgroup-e00nope"}, errs.KindNotFound,
		},
		{"no block group", &commonpb.ResourceMetadata{ParentId: projectID}, &capacitypb.CapacityAllowanceSpec{}, errs.KindNotFound},
		{
			"block group of another tenant", &commonpb.ResourceMetadata{ParentId: projectID},
			&capacitypb.CapacityAllowanceSpec{CapacityBlockGroupId: gidIn(t, otherTenantID, "foreign")}, errs.KindNotFound,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, t0)
			_, err := e.svc.CreateAllowance(t.Context(), domain.CreateAllowanceCmd{Metadata: tt.md, Spec: tt.spec})
			wantKind(t, "CreateAllowance", err, tt.want)
			if len(e.allowances.items) != 0 {
				t.Errorf("a rejected create stored %d allowances", len(e.allowances.items))
			}
		})
	}
}

func TestCreateAllowanceForAnotherProjectOfTheTenant(t *testing.T) {
	t.Parallel()
	e := newEnv(t, t0)
	e.newAllowance(t, otherProjectID, gid(t, "a"), u64(2))
	// A project of another tenant cannot use this tenant's groups, but can
	// use its own.
	_, err := e.createAllowance(t.Context(), foreignProject, gid(t, "a"), u64(2))
	wantKind(t, "CreateAllowance(foreign project, default group)", err, errs.KindNotFound)
	e.newAllowance(t, foreignProject, gidIn(t, otherTenantID, "foreign"), u64(2))
}

func TestCreateAllowanceTwiceIsAlreadyExists(t *testing.T) {
	t.Parallel()
	e := newEnv(t, t0)
	first := e.newAllowance(t, projectID, gid(t, "a"), u64(8))
	_, err := e.createAllowance(t.Context(), projectID, gid(t, "a"), u64(4))
	wantKind(t, "second CreateAllowance", err, errs.KindAlreadyExists)
	if ee, ok := errors.AsType[*errs.Error](err); !ok || ee.ResourceID != first {
		t.Errorf("error = %v, want it to name the existing allowance %s", err, first)
	}
	// One per (project, group): another group or another project is fine.
	e.newAllowance(t, projectID, gid(t, "b"), u64(4))
	e.newAllowance(t, otherProjectID, gid(t, "a"), u64(4))
}

func TestGetAllowanceByGroup(t *testing.T) {
	t.Parallel()
	e := newEnv(t, t0)
	groupID := gid(t, "a")

	def, err := e.svc.GetAllowanceByGroup(t.Context(), projectID, groupID)
	if err != nil {
		t.Fatalf("GetAllowanceByGroup() error: %v", err)
	}
	if def.GetMetadata().GetId() != "" || def.GetMetadata().GetParentId() != projectID ||
		def.GetSpec().GetCapacityBlockGroupId() != groupID || def.GetSpec().Limit != nil {
		t.Errorf("default allowance = %v, want no ID, no limit, parent %s, group %s", def, projectID, groupID)
	}
	if def.GetStatus().GetState() != capacitypb.CapacityAllowanceStatus_STATE_ACTIVE {
		t.Errorf("default allowance state = %v, want ACTIVE", def.GetStatus().GetState())
	}

	id := e.newAllowance(t, projectID, groupID, u64(8))
	got, err := e.svc.GetAllowanceByGroup(t.Context(), projectID, groupID)
	if err != nil || got.GetMetadata().GetId() != id || got.GetSpec().GetLimit() != 8 {
		t.Errorf("GetAllowanceByGroup() = %v, %v; want allowance %s with limit 8", got, err, id)
	}

	tests := []struct {
		name            string
		project, group  string
		want            errs.Kind
		wantNoAllowance bool
	}{
		{"unknown group", projectID, "capacityblockgroup-e00nope", errs.KindNotFound, false},
		{"group of another tenant", projectID, gidIn(t, otherTenantID, "foreign"), errs.KindNotFound, false},
		{"unknown project", "project-e00unknown", groupID, errs.KindNotFound, false},
		{"not a project", tenantID, groupID, errs.KindBadRequest, false},
	}
	for _, tt := range tests {
		_, err := e.svc.GetAllowanceByGroup(t.Context(), tt.project, tt.group)
		wantKind(t, "GetAllowanceByGroup "+tt.name, err, tt.want)
	}
}

func TestGetAllowanceNotFound(t *testing.T) {
	t.Parallel()
	e := newEnv(t, t0)
	_, err := e.svc.GetAllowance(t.Context(), "capacityallowance-e00nope")
	wantKind(t, "GetAllowance", err, errs.KindNotFound)
}

func TestListAllowancesIncludesDefaults(t *testing.T) {
	t.Parallel()
	e := newEnv(t, t0)
	created := e.newAllowance(t, projectID, gid(t, "b"), u64(3))
	e.newAllowance(t, otherProjectID, gid(t, "c"), u64(3)) // another project's: not listed

	page, err := e.svc.ListAllowances(t.Context(), projectID, 0, "")
	if err != nil {
		t.Fatalf("ListAllowances() error: %v", err)
	}
	wantGroups := []string{gid(t, "a"), gid(t, "b"), gid(t, "c"), gid(t, "future"), gid(t, "gap")}
	if len(page.Items) != len(wantGroups) {
		t.Fatalf("ListAllowances() = %d items, want %d (one per block group)", len(page.Items), len(wantGroups))
	}
	for i, a := range page.Items {
		if a.GetSpec().GetCapacityBlockGroupId() != wantGroups[i] {
			t.Errorf("item %d block group = %s, want %s", i, a.GetSpec().GetCapacityBlockGroupId(), wantGroups[i])
		}
		isCreated := wantGroups[i] == gid(t, "b")
		if isCreated && a.GetMetadata().GetId() != created {
			t.Errorf("item %d ID = %q, want the created allowance %s", i, a.GetMetadata().GetId(), created)
		}
		if !isCreated && (a.GetMetadata().GetId() != "" || a.GetSpec().Limit != nil || a.GetMetadata().GetParentId() != projectID) {
			t.Errorf("item %d = %v, want a default allowance (no ID, no limit) of %s", i, a, projectID)
		}
	}

	if _, err := e.svc.ListAllowances(t.Context(), "project-e00unknown", 0, ""); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("ListAllowances(unknown project) error = %v, want not_found", err)
	}
	fp, err := e.svc.ListAllowances(t.Context(), foreignProject, 0, "")
	if err != nil || len(fp.Items) != 1 {
		t.Errorf("ListAllowances(foreign project) = %d items, %v; want the one group of its tenant", len(fp.Items), err)
	}
}

func TestListAllowancesByGroup(t *testing.T) {
	t.Parallel()
	e := newEnv(t, t0)
	a1 := e.newAllowance(t, projectID, gid(t, "a"), u64(8))
	a2 := e.newAllowance(t, otherProjectID, gid(t, "a"), nil)
	e.newAllowance(t, projectID, gid(t, "b"), u64(2))

	page, err := e.svc.ListAllowancesByGroup(t.Context(), gid(t, "a"), 0, "")
	if err != nil {
		t.Fatalf("ListAllowancesByGroup() error: %v", err)
	}
	var got []string
	for _, a := range page.Items {
		got = append(got, a.GetMetadata().GetId())
	}
	if len(got) != 2 || got[0] != a1 || got[1] != a2 {
		t.Errorf("ListAllowancesByGroup() = %v, want [%s %s] (created ones only, every project)", got, a1, a2)
	}
	empty, err := e.svc.ListAllowancesByGroup(t.Context(), gid(t, "c"), 0, "")
	if err != nil || len(empty.Items) != 0 {
		t.Errorf("ListAllowancesByGroup(no allowances) = %d items, %v; want none", len(empty.Items), err)
	}
	if _, err := e.svc.ListAllowancesByGroup(t.Context(), "capacityblockgroup-e00nope", 0, ""); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("ListAllowancesByGroup(unknown) error = %v, want not_found", err)
	}
}

func TestUpdateAllowance(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		initial     *uint64
		md          func(id string) *commonpb.ResourceMetadata
		spec        *capacitypb.CapacityAllowanceSpec
		resetMask   string
		wantVersion int64
		check       func(t *testing.T, a *capacitypb.CapacityAllowance)
	}{
		{
			name: "limit changes", initial: u64(8),
			md:   func(id string) *commonpb.ResourceMetadata { return &commonpb.ResourceMetadata{Id: id} },
			spec: &capacitypb.CapacityAllowanceSpec{Limit: u64(12)}, wantVersion: 2,
			check: func(t *testing.T, a *capacitypb.CapacityAllowance) {
				t.Helper()
				if a.GetSpec().GetLimit() != 12 || a.GetSpec().GetCapacityBlockGroupId() != gid(t, "a") {
					t.Errorf("spec = %v, want limit 12 on group a", a.GetSpec())
				}
			},
		},
		{
			name: "limit above the group size", initial: u64(8),
			md:   func(id string) *commonpb.ResourceMetadata { return &commonpb.ResourceMetadata{Id: id} },
			spec: &capacitypb.CapacityAllowanceSpec{Limit: u64(500)}, wantVersion: 2,
			check: func(t *testing.T, a *capacitypb.CapacityAllowance) {
				t.Helper()
				if a.GetSpec().GetLimit() != 500 {
					t.Errorf("limit = %d, want 500", a.GetSpec().GetLimit())
				}
			},
		},
		{
			name: "same limit is not a change", initial: u64(8),
			md:   func(id string) *commonpb.ResourceMetadata { return &commonpb.ResourceMetadata{Id: id} },
			spec: &capacitypb.CapacityAllowanceSpec{Limit: u64(8)}, wantVersion: 1,
		},
		{
			name: "empty update is not a change", initial: u64(8),
			md:   func(id string) *commonpb.ResourceMetadata { return &commonpb.ResourceMetadata{Id: id} },
			spec: &capacitypb.CapacityAllowanceSpec{}, wantVersion: 1,
		},
		{
			name: "reset mask clears the limit", initial: u64(8),
			md:   func(id string) *commonpb.ResourceMetadata { return &commonpb.ResourceMetadata{Id: id} },
			spec: &capacitypb.CapacityAllowanceSpec{}, resetMask: "spec.limit", wantVersion: 2,
			check: func(t *testing.T, a *capacitypb.CapacityAllowance) {
				t.Helper()
				if a.GetSpec().Limit != nil {
					t.Errorf("limit = %d, want unset (unlimited)", a.GetSpec().GetLimit())
				}
				if a.GetSpec().GetCapacityBlockGroupId() != gid(t, "a") {
					t.Errorf("block group = %s, want it kept", a.GetSpec().GetCapacityBlockGroupId())
				}
			},
		},
		{
			// nebius CLI: update --patch --clear-mask spec.limit sends no spec.
			name: "reset mask without a spec clears only the limit", initial: u64(8),
			md:   func(id string) *commonpb.ResourceMetadata { return &commonpb.ResourceMetadata{Id: id} },
			spec: nil, resetMask: "spec.limit", wantVersion: 2,
			check: func(t *testing.T, a *capacitypb.CapacityAllowance) {
				t.Helper()
				if a.GetSpec().Limit != nil || a.GetSpec().GetCapacityBlockGroupId() != gid(t, "a") {
					t.Errorf("spec = %v, want the block group kept and no limit", a.GetSpec())
				}
			},
		},
		{
			name: "reset mask on an unset limit is not a change", initial: nil,
			md:   func(id string) *commonpb.ResourceMetadata { return &commonpb.ResourceMetadata{Id: id} },
			spec: &capacitypb.CapacityAllowanceSpec{}, resetMask: "spec.limit", wantVersion: 1,
		},
		{
			name: "name and labels", initial: u64(8),
			md: func(id string) *commonpb.ResourceMetadata {
				return &commonpb.ResourceMetadata{Id: id, Name: "renamed", Labels: map[string]string{"k": "v"}}
			},
			spec: &capacitypb.CapacityAllowanceSpec{}, wantVersion: 2,
			check: func(t *testing.T, a *capacitypb.CapacityAllowance) {
				t.Helper()
				if a.GetMetadata().GetName() != "renamed" || a.GetMetadata().GetLabels()["k"] != "v" || a.GetSpec().GetLimit() != 8 {
					t.Errorf("allowance = %v, want name renamed, label k=v, limit 8", a)
				}
			},
		},
		{
			name: "matching resource version", initial: u64(8),
			md: func(id string) *commonpb.ResourceMetadata {
				return &commonpb.ResourceMetadata{Id: id, ResourceVersion: 1}
			},
			spec: &capacitypb.CapacityAllowanceSpec{Limit: u64(9)}, wantVersion: 2,
		},
		{
			name: "same parent", initial: u64(8),
			md: func(id string) *commonpb.ResourceMetadata {
				return &commonpb.ResourceMetadata{Id: id, ParentId: projectID}
			},
			spec: &capacitypb.CapacityAllowanceSpec{Limit: u64(9)}, wantVersion: 2,
		},
		{
			name: "move to another group", initial: u64(8),
			md:   func(id string) *commonpb.ResourceMetadata { return &commonpb.ResourceMetadata{Id: id} },
			spec: &capacitypb.CapacityAllowanceSpec{CapacityBlockGroupId: gid(t, "b")}, wantVersion: 2,
			check: func(t *testing.T, a *capacitypb.CapacityAllowance) {
				t.Helper()
				if a.GetSpec().GetCapacityBlockGroupId() != gid(t, "b") || a.GetSpec().GetLimit() != 8 {
					t.Errorf("spec = %v, want group b with limit 8", a.GetSpec())
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, t0)
			id := e.newAllowance(t, projectID, gid(t, "a"), tt.initial)
			e.clk.Advance(3 * time.Hour)
			done(t)(e.updateAllowance(t.Context(), tt.md(id), tt.spec, tt.resetMask))
			got := e.allowance(t, id)
			if got.GetMetadata().GetResourceVersion() != tt.wantVersion {
				t.Errorf("resource_version = %d, want %d", got.GetMetadata().GetResourceVersion(), tt.wantVersion)
			}
			wantUpdated := t0
			if tt.wantVersion > 1 {
				wantUpdated = e.clk.Now()
			}
			if !got.GetMetadata().GetUpdatedAt().AsTime().Equal(wantUpdated) {
				t.Errorf("updated_at = %s, want %s", got.GetMetadata().GetUpdatedAt().AsTime(), wantUpdated)
			}
			if !got.GetMetadata().GetCreatedAt().AsTime().Equal(t0) {
				t.Errorf("created_at = %s, want it unchanged (%s)", got.GetMetadata().GetCreatedAt().AsTime(), t0)
			}
			if tt.check != nil {
				tt.check(t, got)
			}
		})
	}
}

func TestUpdateAllowanceErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		md   func(id string) *commonpb.ResourceMetadata
		spec *capacitypb.CapacityAllowanceSpec
		want errs.Kind
	}{
		{
			"no ID", func(string) *commonpb.ResourceMetadata { return &commonpb.ResourceMetadata{} },
			&capacitypb.CapacityAllowanceSpec{Limit: u64(1)}, errs.KindBadRequest,
		},
		{
			"unknown allowance", func(string) *commonpb.ResourceMetadata {
				return &commonpb.ResourceMetadata{Id: "capacityallowance-e00nope"}
			},
			&capacitypb.CapacityAllowanceSpec{Limit: u64(1)}, errs.KindNotFound,
		},
		{"version mismatch", func(id string) *commonpb.ResourceMetadata {
			return &commonpb.ResourceMetadata{Id: id, ResourceVersion: 7}
		}, &capacitypb.CapacityAllowanceSpec{Limit: u64(1)}, errs.KindBadState},
		{"parent change", func(id string) *commonpb.ResourceMetadata {
			return &commonpb.ResourceMetadata{Id: id, ParentId: otherProjectID}
		}, &capacitypb.CapacityAllowanceSpec{Limit: u64(1)}, errs.KindBadRequest},
		{
			"move to an unknown group", func(id string) *commonpb.ResourceMetadata { return &commonpb.ResourceMetadata{Id: id} },
			&capacitypb.CapacityAllowanceSpec{CapacityBlockGroupId: "capacityblockgroup-e00nope"}, errs.KindNotFound,
		},
		{
			"move to a group of another tenant", func(id string) *commonpb.ResourceMetadata { return &commonpb.ResourceMetadata{Id: id} },
			&capacitypb.CapacityAllowanceSpec{CapacityBlockGroupId: gidIn(t, otherTenantID, "foreign")}, errs.KindNotFound,
		},
		{
			"move to a group that has an allowance", func(id string) *commonpb.ResourceMetadata { return &commonpb.ResourceMetadata{Id: id} },
			&capacitypb.CapacityAllowanceSpec{CapacityBlockGroupId: gid(t, "c")}, errs.KindAlreadyExists,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, t0)
			id := e.newAllowance(t, projectID, gid(t, "a"), u64(8))
			e.newAllowance(t, projectID, gid(t, "c"), u64(8))
			_, err := e.updateAllowance(t.Context(), tt.md(id), tt.spec, "")
			wantKind(t, "UpdateAllowance", err, tt.want)
			if got := e.allowance(t, id); got.GetMetadata().GetResourceVersion() != 1 || got.GetSpec().GetLimit() != 8 {
				t.Errorf("a rejected update changed the allowance: %v", got)
			}
		})
	}
}

func TestUpdateAllowanceCannotResetTheBlockGroup(t *testing.T) {
	t.Parallel()
	e := newEnv(t, t0)
	id := e.newAllowance(t, projectID, gid(t, "a"), u64(8))
	_, err := e.updateAllowance(t.Context(), &commonpb.ResourceMetadata{Id: id}, &capacitypb.CapacityAllowanceSpec{}, "spec.capacity_block_group_id")
	wantKind(t, "UpdateAllowance(reset the block group)", err, errs.KindNotFound)
	if got := e.allowance(t, id); got.GetSpec().GetCapacityBlockGroupId() != gid(t, "a") {
		t.Errorf("block group = %s, want it kept", got.GetSpec().GetCapacityBlockGroupId())
	}
}

func TestUpdateAllowanceChangesTheEffectiveLimit(t *testing.T) {
	t.Parallel()
	e := newEnv(t, t0)
	groupID := gid(t, "a")
	id := e.newAllowance(t, projectID, groupID, u64(2))
	reserve := func(consumer string, n uint64) error {
		_, err := e.svc.Reserve(t.Context(), peer.CapacityRequest{
			ConsumerID: consumer, ProjectID: projectID, Region: "eu-north1", Platform: "gpu-h100-sxm", Fabric: "fabric-2", Quantity: n,
		})
		return err
	}
	wantKind(t, "Reserve(4) under limit 2", reserve("c1", 4), errs.KindQuotaExceeded)
	done(t)(e.updateAllowance(t.Context(), &commonpb.ResourceMetadata{Id: id}, &capacitypb.CapacityAllowanceSpec{Limit: u64(4)}, ""))
	if err := reserve("c1", 4); err != nil {
		t.Fatalf("Reserve(4) under limit 4: %v", err)
	}
	wantKind(t, "Reserve(5) under limit 4", reserve("c2", 5), errs.KindQuotaExceeded)
	done(t)(e.updateAllowance(t.Context(), &commonpb.ResourceMetadata{Id: id}, &capacitypb.CapacityAllowanceSpec{}, "spec.limit"))
	if err := reserve("c2", 12); err != nil {
		t.Fatalf("Reserve(12) without a limit: %v", err)
	}
}

func TestDeleteAllowance(t *testing.T) {
	t.Parallel()
	e := newEnv(t, t0)
	id := e.newAllowance(t, projectID, gid(t, "a"), u64(1))
	done(t)(e.svc.DeleteAllowance(t.Context(), id, nil))
	_, err := e.svc.GetAllowance(t.Context(), id)
	wantKind(t, "GetAllowance after delete", err, errs.KindNotFound)
	_, err = e.svc.DeleteAllowance(t.Context(), id, nil)
	wantKind(t, "second DeleteAllowance", err, errs.KindNotFound)

	// The group is unlimited again for the project, and the allowance can be
	// created anew.
	def, err := e.svc.GetAllowanceByGroup(t.Context(), projectID, gid(t, "a"))
	if err != nil || def.GetMetadata().GetId() != "" || def.GetSpec().Limit != nil {
		t.Errorf("GetAllowanceByGroup after delete = %v, %v; want the default allowance", def, err)
	}
	e.newAllowance(t, projectID, gid(t, "a"), u64(5))
}

func TestAllowanceStatusReflectsUsage(t *testing.T) {
	t.Parallel()
	e := newEnv(t, t0)
	groupID := gid(t, "a")
	id := e.newAllowance(t, projectID, groupID, u64(8))
	if _, err := e.svc.Reserve(t.Context(), peer.CapacityRequest{
		ConsumerID: "computeinstance-1", ProjectID: projectID, Region: "eu-north1", Platform: "gpu-h100-sxm", Fabric: "fabric-2", Quantity: 3,
	}); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	// Another project's usage does not count towards this allowance.
	if _, err := e.svc.Reserve(t.Context(), peer.CapacityRequest{
		ConsumerID: "computeinstance-2", ProjectID: otherProjectID, Region: "eu-north1", Platform: "gpu-h100-sxm", Fabric: "fabric-2", Quantity: 2,
	}); err != nil {
		t.Fatalf("Reserve(other project): %v", err)
	}
	st := e.allowance(t, id).GetStatus()
	if st.GetUsage() != 3 || st.GetUsagePercentage() != "37.5%" || st.GetUsageState() != capacitypb.CapacityAllowanceStatus_USAGE_STATE_USED {
		t.Errorf("status usage, percentage, state = %d, %q, %v; want 3, 37.5%%, USED", st.GetUsage(), st.GetUsagePercentage(), st.GetUsageState())
	}
	// Without an allowance, usage is a share of the whole group (16).
	def, err := e.svc.GetAllowanceByGroup(t.Context(), otherProjectID, groupID)
	if err != nil {
		t.Fatalf("GetAllowanceByGroup: %v", err)
	}
	if def.GetStatus().GetUsage() != 2 || def.GetStatus().GetUsagePercentage() != "12.5%" {
		t.Errorf("default allowance usage, percentage = %d, %q; want 2, 12.5%%", def.GetStatus().GetUsage(), def.GetStatus().GetUsagePercentage())
	}
}
