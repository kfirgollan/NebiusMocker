package dal

import (
	"testing"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/store"
)

func nvlGroup(id, name string) *computepb.NVLInstanceGroup {
	return &computepb.NVLInstanceGroup{
		Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: parent, Name: name},
		Spec:     &computepb.NVLInstanceGroupSpec{Type: computepb.NVLInstanceGroupSpec_GB200, Size: 2},
		Status:   &computepb.NVLInstanceGroupStatus{},
	}
}

func nvlEmptyCheck(g *computepb.NVLInstanceGroup) error {
	if len(g.GetStatus().GetInstances()) > 0 {
		return errs.BadState(g.GetMetadata().GetId(), "not empty")
	}
	return nil
}

func TestNVLInstanceGroupRepoRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := NewNVLInstanceGroupRepo(store.NewMemory())
	if err := r.Create(ctx, nvlGroup("computenvlinstancegroup-a", "a")); err != nil {
		t.Fatal(err)
	}
	if err := r.Create(ctx, nvlGroup("computenvlinstancegroup-b", "a")); !errs.IsKind(err, errs.KindAlreadyExists) {
		t.Fatalf("duplicate name: %v", err)
	}
	if err := r.Create(ctx, nvlGroup("computenvlinstancegroup-b", "b")); err != nil {
		t.Fatal(err)
	}
	got, err := r.Get(ctx, "computenvlinstancegroup-a")
	if err != nil || got.GetSpec().GetType() != computepb.NVLInstanceGroupSpec_GB200 || got.GetSpec().GetSize() != 2 {
		t.Fatalf("Get = %v, %v", got, err)
	}
	if _, err := r.Get(ctx, "computenvlinstancegroup-missing"); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("Get missing: %v", err)
	}
	byName, err := r.GetByName(ctx, parent, "b")
	if err != nil || byName.GetMetadata().GetId() != "computenvlinstancegroup-b" {
		t.Fatalf("GetByName = %v, %v", byName, err)
	}
	if _, err := r.GetByName(ctx, parent, "zzz"); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("GetByName missing: %v", err)
	}
	page, err := r.List(ctx, parent, 1, "")
	if err != nil || len(page.Items) != 1 || page.Items[0].GetMetadata().GetName() != "a" || page.NextPageToken == "" {
		t.Fatalf("first page = %v, %v", page, err)
	}
	page, err = r.List(ctx, parent, 1, page.NextPageToken)
	if err != nil || len(page.Items) != 1 || page.Items[0].GetMetadata().GetName() != "b" || page.NextPageToken != "" {
		t.Fatalf("second page = %v, %v", page, err)
	}
	if page, err = r.List(ctx, "project-e00other", 0, ""); err != nil || len(page.Items) != 0 {
		t.Fatalf("other parent = %v, %v", page, err)
	}
}

func TestNVLInstanceGroupRepoMutateAndDeleteIf(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := NewNVLInstanceGroupRepo(store.NewMemory())
	for _, n := range []string{"a", "b"} {
		if err := r.Create(ctx, nvlGroup("computenvlinstancegroup-"+n, n)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.Mutate(ctx, "computenvlinstancegroup-a", func(g *computepb.NVLInstanceGroup) error {
		g.Status.Instances = map[string]*computepb.NVLInstanceGroupStatus_InstanceInfo{"computeinstance-x": {}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if g, err := r.Get(ctx, "computenvlinstancegroup-a"); err != nil || len(g.GetStatus().GetInstances()) != 1 {
		t.Fatalf("after Mutate = %v, %v", g, err)
	}
	if _, err := r.Mutate(ctx, "computenvlinstancegroup-missing", func(*computepb.NVLInstanceGroup) error { return nil }); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("Mutate missing: %v", err)
	}
	if _, err := r.Mutate(ctx, "computenvlinstancegroup-a", func(g *computepb.NVLInstanceGroup) error {
		g.Status.Instances = nil
		return errs.BadState("x", "abort")
	}); !errs.IsKind(err, errs.KindBadState) {
		t.Fatalf("Mutate abort: %v", err)
	}
	if g, _ := r.Get(ctx, "computenvlinstancegroup-a"); len(g.GetStatus().GetInstances()) != 1 {
		t.Fatalf("aborted Mutate was stored: %v", g)
	}

	if err := r.DeleteIf(ctx, "computenvlinstancegroup-a", nvlEmptyCheck); !errs.IsKind(err, errs.KindBadState) {
		t.Fatalf("DeleteIf on a non-empty group: %v", err)
	}
	if _, err := r.Get(ctx, "computenvlinstancegroup-a"); err != nil {
		t.Fatalf("aborted DeleteIf removed the group: %v", err)
	}
	if err := r.DeleteIf(ctx, "computenvlinstancegroup-b", nvlEmptyCheck); err != nil {
		t.Fatal(err)
	}
	if err := r.DeleteIf(ctx, "computenvlinstancegroup-b", nvlEmptyCheck); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("DeleteIf of a missing group: %v", err)
	}
}
