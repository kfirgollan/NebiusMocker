package dal

import (
	"errors"
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/testing/protocmp"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/store"
)

func disk(id, name string) *computepb.Disk {
	return &computepb.Disk{
		Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: parent, Name: name, Labels: map[string]string{"k": "v"}},
		Spec: &computepb.DiskSpec{
			Type: computepb.DiskSpec_NETWORK_SSD,
			Size: &computepb.DiskSpec_SizeGibibytes{SizeGibibytes: 1},
		},
		Status: &computepb.DiskStatus{State: computepb.DiskStatus_READY, SizeBytes: 1 << 30, BlockSizeBytes: 4096},
	}
}

func snapshot(id, name, diskID string) *computepb.DiskSnapshot {
	return &computepb.DiskSnapshot{
		Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: parent, Name: name},
		Spec:     &computepb.DiskSnapshotSpec{SourceDiskId: diskID},
		Status:   &computepb.DiskSnapshotStatus{State: computepb.DiskSnapshotStatus_READY, ContentSizeBytes: 1 << 30},
	}
}

func TestDiskRepoRoundTripUniquenessAndDelete(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := NewDiskRepo(store.NewMemory())
	a := disk("computedisk-e00a", "a")
	if err := r.Create(ctx, a); err != nil {
		t.Fatal(err)
	}
	for _, get := range []func() (*computepb.Disk, error){
		func() (*computepb.Disk, error) { return r.Get(ctx, "computedisk-e00a") },
		func() (*computepb.Disk, error) { return r.GetByName(ctx, parent, "a") },
	} {
		got, err := get()
		if err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff(a, got, protocmp.Transform()); diff != "" {
			t.Fatalf("read differs (-want +got):\n%s", diff)
		}
	}
	if err := r.Create(ctx, a); !errs.IsKind(err, errs.KindAlreadyExists) {
		t.Fatalf("duplicate id: got %v, want AlreadyExists", err)
	}
	if err := r.Create(ctx, disk("computedisk-e00b", "a")); !errs.IsKind(err, errs.KindAlreadyExists) {
		t.Fatalf("duplicate name: got %v, want AlreadyExists", err)
	}
	if err := r.Delete(ctx, "computedisk-e00a"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Get(ctx, "computedisk-e00a"); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("after delete: got %v, want NotFound", err)
	}
	if err := r.Delete(ctx, "computedisk-e00a"); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("second delete: got %v, want NotFound", err)
	}
}

func TestDiskRepoMutateIsAtomicAndAbortable(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := NewDiskRepo(store.NewMemory())
	if err := r.Create(ctx, disk("computedisk-e00a", "a")); err != nil {
		t.Fatal(err)
	}
	got, err := r.Mutate(ctx, "computedisk-e00a", func(d *computepb.Disk) error {
		d.Metadata.Name = "renamed"
		d.Status.SizeBytes = 2 << 30
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetStatus().GetSizeBytes() != 2<<30 {
		t.Fatalf("Mutate returned size %d", got.GetStatus().GetSizeBytes())
	}
	if _, err := r.GetByName(ctx, parent, "renamed"); err != nil {
		t.Fatalf("rename not indexed: %v", err)
	}
	abort := errs.BadState("computedisk-e00a", "nope")
	if _, err := r.Mutate(ctx, "computedisk-e00a", func(d *computepb.Disk) error {
		d.Status.SizeBytes = 1
		return abort
	}); !errors.Is(err, abort) {
		t.Fatalf("abort: got %v, want the callback error", err)
	}
	stored, err := r.Get(ctx, "computedisk-e00a")
	if err != nil {
		t.Fatal(err)
	}
	if stored.GetStatus().GetSizeBytes() != 2<<30 {
		t.Fatalf("aborted Mutate wrote: size %d", stored.GetStatus().GetSizeBytes())
	}
	if _, err := r.Mutate(ctx, "computedisk-missing", func(*computepb.Disk) error { return nil }); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("missing: got %v, want NotFound", err)
	}
}

func TestDiskRepoListPages(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := NewDiskRepo(store.NewMemory())
	for i := range 5 {
		if err := r.Create(ctx, disk(fmt.Sprintf("computedisk-e00%d", i), fmt.Sprintf("d%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	var names []string
	token := ""
	for pages := 0; ; pages++ {
		page, err := r.List(ctx, parent, 2, token)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range page.Items {
			names = append(names, d.GetMetadata().GetName())
		}
		if token = page.NextPageToken; token == "" {
			if pages != 2 {
				t.Fatalf("got %d extra pages, want 2", pages)
			}
			break
		}
	}
	if diff := cmp.Diff([]string{"d0", "d1", "d2", "d3", "d4"}, names); diff != "" {
		t.Fatalf("listing (-want +got):\n%s", diff)
	}
	if _, err := r.List(ctx, parent, 2, "!!"); !errs.IsKind(err, errs.KindBadRequest) {
		t.Fatalf("bad token: got %v, want InvalidArgument", err)
	}
}

func TestSnapshotRepoListByDiskFiltersAndPages(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := NewSnapshotRepo(store.NewMemory())
	for i, src := range []string{"computedisk-a", "computedisk-b", "computedisk-a", "computedisk-a", "computedisk-b"} {
		if err := r.Create(ctx, snapshot(fmt.Sprintf("computedisksnapshot-e00%d", i), fmt.Sprintf("s%d", i), src)); err != nil {
			t.Fatal(err)
		}
	}
	first, err := r.ListByDisk(ctx, parent, "computedisk-a", 2, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Items) != 2 || first.NextPageToken == "" {
		t.Fatalf("first page: %d items, token %q", len(first.Items), first.NextPageToken)
	}
	second, err := r.ListByDisk(ctx, parent, "computedisk-a", 2, first.NextPageToken)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range append(first.Items, second.Items...) {
		names = append(names, s.GetMetadata().GetName())
	}
	if diff := cmp.Diff([]string{"s0", "s2", "s3"}, names); diff != "" {
		t.Fatalf("ListByDisk (-want +got):\n%s", diff)
	}
	if second.NextPageToken != "" {
		t.Fatalf("last page has token %q", second.NextPageToken)
	}

	reader := DiskReader{snapshots: r, disks: NewDiskRepo(store.NewMemory())}
	if _, err := reader.Snapshot(ctx, "computedisksnapshot-e000"); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Disk(ctx, "computedisk-a"); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("reader missing disk: got %v, want NotFound", err)
	}
}
