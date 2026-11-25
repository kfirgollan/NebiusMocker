package dal

import (
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/testing/protocmp"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/compute/domain"
)

const otherParent = "project-e00mock0000000000other"

func instance(id, name string) *computepb.Instance {
	return &computepb.Instance{
		Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: parent, Name: name, Labels: map[string]string{"k": "v"}, ResourceVersion: 1},
		Spec: &computepb.InstanceSpec{
			Resources: &computepb.ResourcesSpec{Platform: "cpu-e2", Size: &computepb.ResourcesSpec_Preset{Preset: "2vcpu-8gb"}},
			Hostname:  "h-" + name,
		},
		Status: &computepb.InstanceStatus{
			State:           computepb.InstanceStatus_RUNNING,
			DiskAttachments: []*computepb.DiskAttachmentStatus{{Id: "computedisk-e00a", Name: "boot", IsManaged: true}},
		},
	}
}

func TestInstanceRepoRoundTripAndUniqueness(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := NewInstanceRepo(store.NewMemory())
	a := instance("computeinstance-e00a", "a")
	if err := r.Txn(ctx, func(tx domain.InstanceTx) error { return tx.CreateInstance(a) }); err != nil {
		t.Fatal(err)
	}
	for name, get := range map[string]func() (*computepb.Instance, error){
		"Get":       func() (*computepb.Instance, error) { return r.Get(ctx, a.GetMetadata().GetId()) },
		"GetByName": func() (*computepb.Instance, error) { return r.GetByName(ctx, parent, "a") },
	} {
		got, err := get()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if diff := cmp.Diff(a, got, protocmp.Transform()); diff != "" {
			t.Fatalf("%s differs (-want +got):\n%s", name, diff)
		}
	}
	if _, err := r.Get(ctx, "computeinstance-e00zz"); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("Get(missing) = %v, want NotFound", err)
	}
	if _, err := r.GetByName(ctx, parent, "zz"); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("GetByName(missing) = %v, want NotFound", err)
	}
	for name, inst := range map[string]*computepb.Instance{
		"same id":   instance("computeinstance-e00a", "other"),
		"same name": instance("computeinstance-e00b", "a"),
	} {
		err := r.Txn(ctx, func(tx domain.InstanceTx) error { return tx.CreateInstance(inst) })
		if !errs.IsKind(err, errs.KindAlreadyExists) {
			t.Errorf("create with %s = %v, want AlreadyExists", name, err)
		}
	}
	// The same name in another parent is fine.
	other := instance("computeinstance-e00c", "a")
	other.Metadata.ParentId = otherParent
	if err := r.Txn(ctx, func(tx domain.InstanceTx) error { return tx.CreateInstance(other) }); err != nil {
		t.Fatalf("same name in another project: %v", err)
	}
}

func TestInstanceRepoPutDeleteMissing(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := NewInstanceRepo(store.NewMemory())
	tests := map[string]func(tx domain.InstanceTx) error{
		"PutInstance":    func(tx domain.InstanceTx) error { return tx.PutInstance(instance("computeinstance-e00a", "a")) },
		"DeleteInstance": func(tx domain.InstanceTx) error { return tx.DeleteInstance("computeinstance-e00a") },
		"Instance":       func(tx domain.InstanceTx) error { _, err := tx.Instance("computeinstance-e00a"); return err },
		"PutDisk":        func(tx domain.InstanceTx) error { return tx.PutDisk(disk("computedisk-e00a", "a")) },
		"DeleteDisk":     func(tx domain.InstanceTx) error { return tx.DeleteDisk("computedisk-e00a") },
		"Disk":           func(tx domain.InstanceTx) error { _, err := tx.Disk("computedisk-e00a"); return err },
	}
	for name, fn := range tests {
		if err := r.Txn(ctx, fn); !errs.IsKind(err, errs.KindNotFound) {
			t.Errorf("%s(missing) = %v, want NotFound", name, err)
		}
	}
}

func TestInstanceRepoPutAndDelete(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := NewInstanceRepo(store.NewMemory())
	a := instance("computeinstance-e00a", "a")
	if err := r.Txn(ctx, func(tx domain.InstanceTx) error { return tx.CreateInstance(a) }); err != nil {
		t.Fatal(err)
	}
	a.Status.State = computepb.InstanceStatus_STOPPED
	a.Metadata.ResourceVersion = 2
	if err := r.Txn(ctx, func(tx domain.InstanceTx) error { return tx.PutInstance(a) }); err != nil {
		t.Fatal(err)
	}
	got, err := r.Get(ctx, "computeinstance-e00a")
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(a, got, protocmp.Transform()); diff != "" {
		t.Fatalf("after put (-want +got):\n%s", diff)
	}
	if err := r.Txn(ctx, func(tx domain.InstanceTx) error { return tx.DeleteInstance("computeinstance-e00a") }); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Get(ctx, "computeinstance-e00a"); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("Get after delete = %v, want NotFound", err)
	}
}

func TestInstanceRepoTxnReachesDisks(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	st := store.NewMemory()
	r := NewInstanceRepo(st)
	err := r.Txn(ctx, func(tx domain.InstanceTx) error {
		if err := tx.CreateDisk(disk("computedisk-e00a", "boot")); err != nil {
			return err
		}
		if err := tx.CreateDisk(disk("computedisk-e00b", "boot")); !errs.IsKind(err, errs.KindAlreadyExists) {
			return fmt.Errorf("CreateDisk(duplicate name) = %w, want AlreadyExists", err)
		}
		got, err := tx.Disk("computedisk-e00a")
		if err != nil {
			return err
		}
		got.Status.ReadWriteAttachment = "computeinstance-e00a"
		return tx.PutDisk(got)
	})
	if err != nil {
		t.Fatal(err)
	}
	// The disk is visible through the DiskRepo: same store kind.
	got, err := NewDiskRepo(st).Get(ctx, "computedisk-e00a")
	if err != nil {
		t.Fatal(err)
	}
	if got.GetStatus().GetReadWriteAttachment() != "computeinstance-e00a" {
		t.Fatalf("disk status = %v, want the attachment written in the transaction", got.GetStatus())
	}
	if err := r.Txn(ctx, func(tx domain.InstanceTx) error { return tx.DeleteDisk("computedisk-e00a") }); err != nil {
		t.Fatal(err)
	}
	if _, err := NewDiskRepo(st).Get(ctx, "computedisk-e00a"); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("disk after DeleteDisk = %v, want NotFound", err)
	}
}

func TestInstanceRepoTxnRollsBackOnError(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	st := store.NewMemory()
	r := NewInstanceRepo(st)
	keep := instance("computeinstance-e00a", "keep")
	if err := r.Txn(ctx, func(tx domain.InstanceTx) error {
		if err := tx.CreateInstance(keep); err != nil {
			return err
		}
		return tx.CreateDisk(disk("computedisk-e00keep", "keep"))
	}); err != nil {
		t.Fatal(err)
	}

	boom := errors.New("boom")
	err := r.Txn(ctx, func(tx domain.InstanceTx) error {
		if err := tx.CreateInstance(instance("computeinstance-e00b", "new")); err != nil {
			return err
		}
		if err := tx.CreateDisk(disk("computedisk-e00new", "new")); err != nil {
			return err
		}
		upd := instance("computeinstance-e00a", "keep")
		upd.Status.State = computepb.InstanceStatus_STOPPED
		if err := tx.PutInstance(upd); err != nil {
			return err
		}
		if err := tx.DeleteDisk("computedisk-e00keep"); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Txn = %v, want boom", err)
	}
	if _, err := r.Get(ctx, "computeinstance-e00b"); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("created instance survived rollback: %v", err)
	}
	if _, err := NewDiskRepo(st).Get(ctx, "computedisk-e00new"); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("created disk survived rollback: %v", err)
	}
	if _, err := NewDiskRepo(st).Get(ctx, "computedisk-e00keep"); err != nil {
		t.Errorf("deleted disk was not restored by rollback: %v", err)
	}
	got, err := r.Get(ctx, "computeinstance-e00a")
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(keep, got, protocmp.Transform()); diff != "" {
		t.Errorf("instance changed despite rollback (-want +got):\n%s", diff)
	}
}

func TestInstanceRepoListPaging(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := NewInstanceRepo(store.NewMemory())
	for i := range 5 {
		inst := instance(fmt.Sprintf("computeinstance-e00%d", i), fmt.Sprintf("i%d", i))
		if err := r.Txn(ctx, func(tx domain.InstanceTx) error { return tx.CreateInstance(inst) }); err != nil {
			t.Fatal(err)
		}
	}
	other := instance("computeinstance-e00x", "x")
	other.Metadata.ParentId = otherParent
	if err := r.Txn(ctx, func(tx domain.InstanceTx) error { return tx.CreateInstance(other) }); err != nil {
		t.Fatal(err)
	}
	var names []string
	token, pages := "", 0
	for {
		page, err := r.List(ctx, parent, 2, token)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, it := range page.Items {
			names = append(names, it.GetMetadata().GetName())
		}
		if page.NextPageToken == "" {
			break
		}
		token = page.NextPageToken
	}
	if diff := cmp.Diff([]string{"i0", "i1", "i2", "i3", "i4"}, names); diff != "" || pages != 3 {
		t.Fatalf("listed %v in %d pages, want i0..i4 in 3 pages (diff:\n%s)", names, pages, diff)
	}
}

func TestInstanceOpIndex(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	idx := NewInstanceOpIndex(store.NewMemory())
	if ids, err := idx.IDs(ctx, parent); err != nil || len(ids) != 0 {
		t.Fatalf("IDs(empty) = %v, %v", ids, err)
	}
	for _, op := range []string{"operation-e00a", "operation-e00b", "operation-e00c"} {
		if err := idx.Add(ctx, parent, op); err != nil {
			t.Fatal(err)
		}
	}
	if err := idx.Add(ctx, otherParent, "operation-e00z"); err != nil {
		t.Fatal(err)
	}
	ids, err := idx.IDs(ctx, parent)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]string{"operation-e00a", "operation-e00b", "operation-e00c"}, ids); diff != "" {
		t.Fatalf("IDs = %v, want the project's operations in submission order (-want +got):\n%s", ids, diff)
	}
}

func TestInstanceRepoStopTimes(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := NewInstanceRepo(store.NewMemory())
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 123, time.UTC)
	err := r.Txn(ctx, func(tx domain.InstanceTx) error {
		return errors.Join(tx.SetStoppedAt("computeinstance-e00a", t0), tx.SetStoppedAt("computeinstance-e00b", t0.Add(time.Hour)))
	})
	if err != nil {
		t.Fatal(err)
	}
	err = r.Txn(ctx, func(tx domain.InstanceTx) error {
		at, ok, err := tx.StoppedAt("computeinstance-e00a")
		if err != nil || !ok || !at.Equal(t0) {
			return fmt.Errorf("StoppedAt = %v, %v; want %v: %w", at, ok, t0, err)
		}
		if _, ok, err := tx.StoppedAt("computeinstance-e00none"); err != nil || ok {
			return fmt.Errorf("StoppedAt(missing) = %v: %w", ok, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if ids, err := r.StoppedBefore(ctx, t0.Add(time.Minute)); err != nil || !slices.Equal(ids, []string{"computeinstance-e00a"}) {
		t.Fatalf("StoppedBefore = %v, %v; want only a", ids, err)
	}
	err = r.Txn(ctx, func(tx domain.InstanceTx) error {
		return errors.Join(tx.ClearStoppedAt("computeinstance-e00a"), tx.ClearStoppedAt("computeinstance-e00none"))
	})
	if err != nil {
		t.Fatalf("ClearStoppedAt (idempotent): %v", err)
	}
	if ids, err := r.StoppedBefore(ctx, t0.Add(2*time.Hour)); err != nil || !slices.Equal(ids, []string{"computeinstance-e00b"}) {
		t.Fatalf("StoppedBefore after clear = %v, %v; want only b", ids, err)
	}
}
