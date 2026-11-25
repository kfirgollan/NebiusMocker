package domain

import (
	"fmt"
	"slices"
	"testing"

	"github.com/google/go-cmp/cmp"

	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

// mustCreateN creates n instances vm0..vm(n-1), each with its own boot disk and NIC name.
func (f *instFixture) mustCreateN(t *testing.T, n int) []*computepb.Instance {
	t.Helper()
	var out []*computepb.Instance
	for i := range n {
		spec := baseSpec()
		spec.BootDisk = managedDisk(fmt.Sprintf("boot%d", i), 20)
		out = append(out, f.mustCreate(t, fmt.Sprintf("vm%d", i), spec))
	}
	return out
}

func TestInstanceGetAndGetByName(t *testing.T) {
	t.Parallel()
	f := newInstFixture(t)
	inst := f.mustCreate(t, "vm", baseSpec())
	id := inst.GetMetadata().GetId()
	got, err := f.svc.Get(f.ctx, id)
	if err != nil || got.GetMetadata().GetId() != id {
		t.Fatalf("Get = %v, %v", got, err)
	}
	got, err = f.svc.GetByName(f.ctx, testProject, "vm")
	if err != nil || got.GetMetadata().GetId() != id {
		t.Fatalf("GetByName = %v, %v", got, err)
	}
	_, err = f.svc.Get(f.ctx, "")
	wantKind(t, err, errs.KindBadRequest, "id")
	_, err = f.svc.Get(f.ctx, "computeinstance-e00nope")
	wantKind(t, err, errs.KindNotFound, "")
	_, err = f.svc.GetByName(f.ctx, "", "vm")
	wantKind(t, err, errs.KindBadRequest, "parent_id")
	_, err = f.svc.GetByName(f.ctx, testProject, "nope")
	wantKind(t, err, errs.KindNotFound, "")
	_, err = f.svc.GetByName(f.ctx, otherProject, "vm")
	wantKind(t, err, errs.KindNotFound, "")
}

func TestInstanceListPaging(t *testing.T) {
	t.Parallel()
	f := newInstFixture(t)
	f.mustCreateN(t, 5)
	var names []string
	token, pages := "", 0
	for {
		page, err := f.svc.List(f.ctx, testProject, 2, token)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, i := range page.Items {
			names = append(names, i.GetMetadata().GetName())
		}
		if page.NextPageToken == "" {
			break
		}
		token = page.NextPageToken
	}
	if want := []string{"vm0", "vm1", "vm2", "vm3", "vm4"}; !slices.Equal(names, want) || pages != 3 {
		t.Fatalf("listed %v in %d pages, want %v in 3", names, pages, want)
	}
	if page, err := f.svc.List(f.ctx, otherProject, 0, ""); err != nil || len(page.Items) != 0 {
		t.Fatalf("List(other project) = %v, %v", page, err)
	}
	_, err := f.svc.List(f.ctx, "", 0, "")
	wantKind(t, err, errs.KindBadRequest, "parent_id")
	_, err = f.svc.List(f.ctx, testProject, -1, "")
	wantKind(t, err, errs.KindBadRequest, "page_size")
	_, err = f.svc.List(f.ctx, testProject, 1, "garbage!")
	wantKind(t, err, errs.KindBadRequest, "")
}

func TestInstanceBatchGet(t *testing.T) {
	t.Parallel()
	f := newInstFixture(t)
	insts := f.mustCreateN(t, 2)
	a, denied := insts[0].GetMetadata().GetId(), insts[1].GetMetadata().GetId()
	f.perms.deny[denied] = true
	const missing = "computeinstance-e00nope"

	got, err := f.svc.BatchGet(f.ctx, []string{a, missing, denied, a})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("%d results, want one per distinct id: %v", len(got), got)
	}
	if got[a].Err != nil || got[a].Instance.GetMetadata().GetId() != a {
		t.Fatalf("result for %s = %+v", a, got[a])
	}
	if got[missing].Instance != nil || !errs.IsKind(got[missing].Err, errs.KindNotFound) {
		t.Fatalf("result for the missing id = %+v, want NotFound", got[missing])
	}
	if got[denied].Instance != nil || !errs.IsKind(got[denied].Err, errs.KindPermissionDenied) {
		t.Fatalf("result for the denied id = %+v, want PermissionDenied", got[denied])
	}

	_, err = f.svc.BatchGet(f.ctx, nil)
	wantKind(t, err, errs.KindBadRequest, "instance_ids")
	many := make([]string, 1001)
	for i := range many {
		many[i] = fmt.Sprintf("computeinstance-e00%d", i)
	}
	_, err = f.svc.BatchGet(f.ctx, many)
	wantKind(t, err, errs.KindBadRequest, "instance_ids")
	if _, err := f.svc.BatchGet(f.ctx, many[:1000]); err != nil {
		t.Fatalf("1000 ids: %v", err)
	}
}

func TestInstanceListByNVLInstanceGroup(t *testing.T) {
	t.Parallel()
	f := newInstFixture(t)
	f.mustCreate(t, "vm", baseSpec())
	_, err := f.svc.ListByNVLInstanceGroup(f.ctx, "nvlinstancegroup-e00any", 0, "")
	wantKind(t, err, errs.KindNotFound, "")
	_, err = f.svc.ListByNVLInstanceGroup(f.ctx, "", 0, "")
	wantKind(t, err, errs.KindBadRequest, "nvl_instance_group_id")
	_, err = f.svc.ListByNVLInstanceGroup(f.ctx, "nvlinstancegroup-e00any", -1, "")
	wantKind(t, err, errs.KindBadRequest, "page_size")
}

func TestInstanceListOperationsByParent(t *testing.T) {
	t.Parallel()
	f := newInstFixture(t)
	insts := f.mustCreateN(t, 2)
	a, b := insts[0].GetMetadata().GetId(), insts[1].GetMetadata().GetId()
	if err := f.stop(a); err != nil {
		t.Fatal(err)
	}
	if err := f.remove(b); err != nil {
		t.Fatal(err)
	}
	type entry struct{ Desc, Resource string }
	list := func(size int64, token string) ([]entry, string) {
		t.Helper()
		page, err := f.svc.ListOperationsByParent(f.ctx, testProject, size, token)
		if err != nil {
			t.Fatal(err)
		}
		var out []entry
		for _, op := range page.Items {
			out = append(out, entry{op.Description, op.ResourceID})
		}
		return out, page.NextPageToken
	}
	want := []entry{{"Delete instance", b}, {"Stop instance", a}, {"Create instance", b}, {"Create instance", a}}
	all, next := list(0, "")
	if diff := cmp.Diff(want, all); diff != "" || next != "" {
		t.Fatalf("operations (-want +got):\n%s (next %q)", diff, next)
	}
	first, next := list(3, "")
	if diff := cmp.Diff(want[:3], first); diff != "" || next == "" {
		t.Fatalf("first page (-want +got):\n%s (next %q)", diff, next)
	}
	second, next := list(3, next)
	if diff := cmp.Diff(want[3:], second); diff != "" || next != "" {
		t.Fatalf("second page (-want +got):\n%s (next %q)", diff, next)
	}
	if page, err := f.svc.ListOperationsByParent(f.ctx, otherProject, 0, ""); err != nil || len(page.Items) != 0 {
		t.Fatalf("operations of another project = %v, %v", page, err)
	}
	_, err := f.svc.ListOperationsByParent(f.ctx, "", 0, "")
	wantKind(t, err, errs.KindBadRequest, "parent_id")
}

func TestInstanceRejectedRequestsAreNotIndexed(t *testing.T) {
	t.Parallel()
	f := newInstFixture(t)
	_, err := f.create("vm", edited(baseSpec(), func(s *computepb.InstanceSpec) { s.Resources.Platform = "nope" }))
	wantKind(t, err, errs.KindBadRequest, "spec.resources.platform")
	page, err := f.svc.ListOperationsByParent(f.ctx, testProject, 0, "")
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("operations after a rejected create = %v, %v", page, err)
	}
}
