package dal_test

import (
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/testing/protocmp"

	capacitypb "github.com/nebius/gosdk/proto/nebius/capacity/v1"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/capacity/dal"
	"github.com/kfirgollan/NebiusMocker/services/capacity/domain"
)

const (
	project  = "project-e00mock000000000default"
	project2 = "project-e00mock000000000other"
	cbg1     = "capacityblockgroup-e00one"
	cbg2     = "capacityblockgroup-e00two"
)

func allowance(id, parent, name, cbg string, limit uint64) *capacitypb.CapacityAllowance {
	return &capacitypb.CapacityAllowance{
		Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: parent, Name: name, ResourceVersion: 1},
		Spec:     &capacitypb.CapacityAllowanceSpec{CapacityBlockGroupId: cbg, Limit: &limit},
		Status:   &capacitypb.CapacityAllowanceStatus{},
	}
}

func ids(as []*capacitypb.CapacityAllowance) []string {
	out := make([]string, 0, len(as))
	for _, a := range as {
		out = append(out, a.GetMetadata().GetId())
	}
	return out
}

func newRepo() *dal.AllowanceRepo { return dal.NewAllowanceRepo(store.NewMemory()) }

func TestAllowanceCRUD(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := newRepo()
	a := allowance("capacityallowance-1", project, "same", cbg1, 8)
	if err := r.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := r.Get(ctx, "capacityallowance-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if diff := cmp.Diff(a, got, protocmp.Transform()); diff != "" {
		t.Errorf("Get mismatch (-want +got):\n%s", diff)
	}
	upd := allowance("capacityallowance-1", project, "same", cbg1, 16)
	if err := r.Update(ctx, upd); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, err = r.Get(ctx, "capacityallowance-1")
	if err != nil {
		t.Fatalf("Get after Update: %v", err)
	}
	if diff := cmp.Diff(upd, got, protocmp.Transform()); diff != "" {
		t.Errorf("Update mismatch (-want +got):\n%s", diff)
	}
	if err := r.Delete(ctx, "capacityallowance-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := r.Get(ctx, "capacityallowance-1"); errs.KindOf(err) != errs.KindNotFound {
		t.Errorf("Get after Delete kind = %v, want NotFound", errs.KindOf(err))
	}
}

func TestAllowanceNamesNotUnique(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := newRepo()
	for _, a := range []*capacitypb.CapacityAllowance{
		allowance("capacityallowance-1", project, "dup", cbg1, 1),
		allowance("capacityallowance-2", project, "dup", cbg2, 1),
	} {
		if err := r.Create(ctx, a); err != nil {
			t.Fatalf("Create %s: %v", a.GetMetadata().GetId(), err)
		}
	}
}

func TestAllowanceNotFound(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := newRepo()
	tests := []struct {
		name string
		call func() error
	}{
		{"get", func() error { _, err := r.Get(ctx, "nope"); return err }},
		{"update", func() error { return r.Update(ctx, allowance("nope", project, "", cbg1, 1)) }},
		{"delete", func() error { return r.Delete(ctx, "nope") }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := errs.KindOf(tc.call()); got != errs.KindNotFound {
				t.Errorf("kind = %v, want NotFound", got)
			}
		})
	}
}

func TestAllowanceDuplicates(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := newRepo()
	for _, a := range []*capacitypb.CapacityAllowance{
		allowance("capacityallowance-1", project, "", cbg1, 1),
		allowance("capacityallowance-2", project, "", cbg2, 1),
	} {
		if err := r.Create(ctx, a); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}
	tests := []struct {
		name       string
		call       func() error
		wantExists string
	}{
		{"create same id", func() error {
			return r.Create(ctx, allowance("capacityallowance-1", project, "", cbg2, 1))
		}, "capacityallowance-1"},
		{"create same parent and group", func() error {
			return r.Create(ctx, allowance("capacityallowance-3", project, "", cbg1, 1))
		}, "capacityallowance-1"},
		{"update to taken group", func() error {
			return r.Update(ctx, allowance("capacityallowance-2", project, "", cbg1, 5))
		}, "capacityallowance-1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.call()
			if errs.KindOf(err) != errs.KindAlreadyExists {
				t.Fatalf("kind = %v, want AlreadyExists (err %v)", errs.KindOf(err), err)
			}
			if e, ok := errors.AsType[*errs.Error](err); !ok || e.ResourceID != tc.wantExists {
				t.Errorf("existing ID = %v, want %s", e, tc.wantExists)
			}
		})
	}
	// Updating an allowance keeping its own group is fine.
	if err := r.Update(ctx, allowance("capacityallowance-1", project, "", cbg1, 9)); err != nil {
		t.Errorf("Update own group: %v", err)
	}
	// The same group in another project is allowed.
	if err := r.Create(ctx, allowance("capacityallowance-4", project2, "", cbg1, 1)); err != nil {
		t.Errorf("Create same group in other project: %v", err)
	}
}

func TestAllowanceListings(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := newRepo()
	for _, a := range []*capacitypb.CapacityAllowance{
		allowance("capacityallowance-1", project, "", cbg1, 1),
		allowance("capacityallowance-2", project2, "", cbg1, 1),
		allowance("capacityallowance-3", project, "", cbg2, 1),
	} {
		if err := r.Create(ctx, a); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}
	tests := []struct {
		name string
		call func() ([]*capacitypb.CapacityAllowance, error)
		want []string
	}{
		{
			"by project", func() ([]*capacitypb.CapacityAllowance, error) { return r.ByProject(ctx, project) },
			[]string{"capacityallowance-1", "capacityallowance-3"},
		},
		{
			"by block group across projects", func() ([]*capacitypb.CapacityAllowance, error) { return r.ByBlockGroup(ctx, cbg1) },
			[]string{"capacityallowance-1", "capacityallowance-2"},
		},
		{
			"unknown block group", func() ([]*capacitypb.CapacityAllowance, error) { return r.ByBlockGroup(ctx, "none") },
			[]string{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := tc.call()
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tc.want, ids(got)); diff != "" {
				t.Errorf("ids (-want +got):\n%s", diff)
			}
		})
	}
}

func TestUsageRepo(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := dal.NewUsageRepo(store.NewMemory())
	u1 := domain.Usage{ConsumerID: "c1", ProjectID: project, BlockGroupID: cbg1, Quantity: 2}
	u2 := domain.Usage{ConsumerID: "c2", ProjectID: project, BlockGroupID: cbg1, Quantity: 3}
	u3 := domain.Usage{ConsumerID: "c3", ProjectID: project, BlockGroupID: cbg2, Quantity: 1}
	for _, u := range []domain.Usage{u1, u2, u3} {
		if err := r.Put(ctx, u); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	got, err := r.ByBlockGroup(ctx, cbg1)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]domain.Usage{u1, u2}, got); diff != "" {
		t.Errorf("ByBlockGroup (-want +got):\n%s", diff)
	}
	// Put replaces, and may move the consumer to another block group.
	u1b := domain.Usage{ConsumerID: "c1", ProjectID: project, BlockGroupID: cbg1, Quantity: 7}
	if err := r.Put(ctx, u1b); err != nil {
		t.Fatal(err)
	}
	got, err = r.ByBlockGroup(ctx, cbg1)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]domain.Usage{u1b, u2}, got); diff != "" {
		t.Errorf("after replace (-want +got):\n%s", diff)
	}
	u2m := domain.Usage{ConsumerID: "c2", ProjectID: project, BlockGroupID: cbg2, Quantity: 3}
	if err := r.Put(ctx, u2m); err != nil {
		t.Fatal(err)
	}
	got, err = r.ByBlockGroup(ctx, cbg1)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]domain.Usage{u1b}, got); diff != "" {
		t.Errorf("after move (-want +got):\n%s", diff)
	}
	if err := r.Delete(ctx, "c1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := r.Delete(ctx, "c1"); err != nil {
		t.Errorf("Delete missing = %v, want nil", err)
	}
	got, err = r.ByBlockGroup(ctx, cbg1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("ByBlockGroup after delete = %v, want empty", got)
	}
}
