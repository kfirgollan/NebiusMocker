package domain

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
)

// memRepo is an ordered in-memory resource store shared by the disk and
// snapshot fakes. Mutate is atomic and writes nothing when fn fails.
type memRepo[T interface {
	proto.Message
	GetMetadata() *commonpb.ResourceMetadata
}] struct {
	mu    sync.Mutex
	byID  map[string]T
	order []string
}

func (r *memRepo[T]) get(id string) (T, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.byID[id]
	if !ok {
		var zero T
		return zero, errs.NotFound(id)
	}
	return proto.CloneOf(v), nil
}

func (r *memRepo[T]) getByName(parentID, name string) (T, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, id := range r.order {
		if v, ok := r.byID[id]; ok && v.GetMetadata().GetParentId() == parentID && v.GetMetadata().GetName() == name {
			return proto.CloneOf(v), nil
		}
	}
	var zero T
	return zero, errs.NotFound(name)
}

// page returns the clones of the items matching keep, in creation order.
func (r *memRepo[T]) page(keep func(T) bool, size int, token string) (items []T, next string, err error) {
	offset, err := decodeOffset(token)
	if err != nil {
		return nil, "", err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var all []T
	for _, id := range r.order {
		if v, ok := r.byID[id]; ok && keep(v) {
			all = append(all, proto.CloneOf(v))
		}
	}
	offset = min(offset, len(all))
	end := min(offset+size, len(all))
	if end < len(all) {
		next = encodeOffset(end)
	}
	return all[offset:end], next, nil
}

func (r *memRepo[T]) create(v T) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	md := v.GetMetadata()
	if r.byID == nil {
		r.byID = map[string]T{}
	}
	if _, ok := r.byID[md.GetId()]; ok {
		return errs.AlreadyExists(md.GetId(), "id exists")
	}
	for _, other := range r.byID {
		if o := other.GetMetadata(); o.GetParentId() == md.GetParentId() && o.GetName() == md.GetName() {
			return errs.AlreadyExists(o.GetId(), "name exists")
		}
	}
	r.byID[md.GetId()] = proto.CloneOf(v)
	r.order = append(r.order, md.GetId())
	return nil
}

func (r *memRepo[T]) mutate(id string, fn func(T) error) (T, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur, ok := r.byID[id]
	if !ok {
		var zero T
		return zero, errs.NotFound(id)
	}
	work := proto.CloneOf(cur)
	if err := fn(work); err != nil {
		var zero T
		return zero, err
	}
	r.byID[id] = proto.CloneOf(work)
	return work, nil
}

func (r *memRepo[T]) remove(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.byID[id]; !ok {
		return errs.NotFound(id)
	}
	delete(r.byID, id)
	return nil
}

type fakeDisks struct{ repo memRepo[*computepb.Disk] }

func (f *fakeDisks) Get(_ context.Context, id string) (*computepb.Disk, error) { return f.repo.get(id) }

func (f *fakeDisks) GetByName(_ context.Context, parentID, name string) (*computepb.Disk, error) {
	return f.repo.getByName(parentID, name)
}

func (f *fakeDisks) List(_ context.Context, parentID string, size int, token string) (DiskPage, error) {
	items, next, err := f.repo.page(func(d *computepb.Disk) bool { return d.GetMetadata().GetParentId() == parentID }, size, token)
	return DiskPage{Items: items, NextPageToken: next}, err
}

func (f *fakeDisks) Create(_ context.Context, d *computepb.Disk) error { return f.repo.create(d) }

func (f *fakeDisks) Mutate(_ context.Context, id string, fn func(*computepb.Disk) error) (*computepb.Disk, error) {
	return f.repo.mutate(id, fn)
}

func (f *fakeDisks) Delete(_ context.Context, id string) error { return f.repo.remove(id) }

type fakeSnapshots struct {
	repo memRepo[*computepb.DiskSnapshot]
}

func (f *fakeSnapshots) Get(_ context.Context, id string) (*computepb.DiskSnapshot, error) {
	return f.repo.get(id)
}

func (f *fakeSnapshots) GetByName(_ context.Context, parentID, name string) (*computepb.DiskSnapshot, error) {
	return f.repo.getByName(parentID, name)
}

func (f *fakeSnapshots) List(_ context.Context, parentID string, size int, token string) (SnapshotPage, error) {
	items, next, err := f.repo.page(func(s *computepb.DiskSnapshot) bool { return s.GetMetadata().GetParentId() == parentID }, size, token)
	return SnapshotPage{Items: items, NextPageToken: next}, err
}

func (f *fakeSnapshots) ListByDisk(_ context.Context, parentID, diskID string, size int, token string) (SnapshotPage, error) {
	items, next, err := f.repo.page(func(s *computepb.DiskSnapshot) bool {
		return s.GetMetadata().GetParentId() == parentID && s.GetSpec().GetSourceDiskId() == diskID
	}, size, token)
	return SnapshotPage{Items: items, NextPageToken: next}, err
}

func (f *fakeSnapshots) Create(_ context.Context, s *computepb.DiskSnapshot) error {
	return f.repo.create(s)
}

func (f *fakeSnapshots) Mutate(_ context.Context, id string, fn func(*computepb.DiskSnapshot) error) (*computepb.DiskSnapshot, error) {
	return f.repo.mutate(id, fn)
}

func (f *fakeSnapshots) Delete(_ context.Context, id string) error { return f.repo.remove(id) }

// fakeImageLookup is an ImageLookup over a handful of seeded images.
type fakeImageLookup struct {
	mu     sync.Mutex
	images []*computepb.Image
	public map[string]bool
}

func (f *fakeImageLookup) add(img *computepb.Image) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.images = append(f.images, proto.CloneOf(img))
}

func (f *fakeImageLookup) Get(_ context.Context, id string) (*computepb.Image, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, img := range f.images {
		if img.GetMetadata().GetId() == id {
			return proto.CloneOf(img), nil
		}
	}
	return nil, errs.NotFound(id)
}

func (f *fakeImageLookup) GetLatestByFamily(_ context.Context, family, parentID string) (*computepb.Image, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var latest *computepb.Image
	for _, img := range f.images {
		if img.GetSpec().GetImageFamily() == family && (parentID == "" || img.GetMetadata().GetParentId() == parentID) {
			latest = img
		}
	}
	if latest == nil {
		return nil, errs.NotFound(family)
	}
	return proto.CloneOf(latest), nil
}

func (f *fakeImageLookup) IsPublicProject(id string) bool { return f.public[id] }

// deferOps is an Operations whose Work runs inline, or, once deferred is set,
// waits for finish. Prepare always runs inline, like the real engine.
type deferOps struct {
	fakeOps
	deferred bool
	pending  []func()
}

func (f *deferOps) Submit(ctx context.Context, spec operations.Spec) (*operations.Operation, error) {
	if !f.deferred {
		return f.fakeOps.Submit(ctx, spec)
	}
	if spec.Prepare != nil {
		if err := spec.Prepare(ctx); err != nil {
			return nil, err
		}
	}
	f.mu.Lock()
	f.n++
	op := &operations.Operation{ID: fmt.Sprintf("operation-e00mock%013d", f.n), Description: spec.Description, ResourceID: spec.ResourceID}
	if f.ops == nil {
		f.ops = map[string]*operations.Operation{}
	}
	f.ops[op.ID] = op
	f.mu.Unlock()
	f.pending = append(f.pending, func() {
		if spec.Work != nil {
			op.Err = spec.Work(ctx)
		}
		op.FinishedAt = time.Unix(1, 0)
	})
	return op, nil
}

// finish runs the deferred Work of every submitted operation.
func (f *deferOps) finish() {
	for _, w := range f.pending {
		w()
	}
	f.pending = nil
}

const otherProject = "project-e00mock0000000000other"

type diskFixture struct {
	disks  *fakeDisks
	snaps  *fakeSnapshots
	images *fakeImageLookup
	ops    *deferOps
	idx    *fakeOpIndex
	clk    *clock.Manual
	ds     *DiskService
	ss     *SnapshotService
}

func newDiskFixture(t *testing.T) *diskFixture {
	t.Helper()
	f := &diskFixture{
		disks: &fakeDisks{}, snaps: &fakeSnapshots{}, ops: &deferOps{}, idx: &fakeOpIndex{},
		images: &fakeImageLookup{public: map[string]bool{PublicProjectID("e00"): true}},
		clk:    clock.NewManual(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
	}
	deps := DiskDeps{
		Disks: f.disks, Snapshots: f.snaps, Images: f.images, OpIndex: f.idx,
		Projects: fakeProjects{testProject: true, otherProject: true},
		Ops:      f.ops, IDs: &fakeIDs{}, Clock: f.clk,
	}
	var err error
	if f.ds, err = NewDiskService(deps); err != nil {
		t.Fatal(err)
	}
	if f.ss, err = NewSnapshotService(deps); err != nil {
		t.Fatal(err)
	}
	return f
}

// seedDisk writes a READY 10 GiB NETWORK_SSD disk straight into the repo; edit
// may adjust it first.
func (f *diskFixture) seedDisk(t *testing.T, id string, edit func(*computepb.Disk)) {
	t.Helper()
	d := &computepb.Disk{
		Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: testProject, Name: id, ResourceVersion: 1},
		Spec: &computepb.DiskSpec{
			Size: &computepb.DiskSpec_SizeGibibytes{SizeGibibytes: 10}, Type: computepb.DiskSpec_NETWORK_SSD,
		},
		Status: &computepb.DiskStatus{State: computepb.DiskStatus_READY, SizeBytes: 10 * gib, BlockSizeBytes: DefaultBlockSizeBytes},
	}
	if edit != nil {
		edit(d)
	}
	if err := f.disks.Create(t.Context(), d); err != nil {
		t.Fatal(err)
	}
}

// seedSnapshot writes a READY 10 GiB snapshot straight into the repo.
func (f *diskFixture) seedSnapshot(t *testing.T, id, parent, diskID string, edit func(*computepb.DiskSnapshot)) {
	t.Helper()
	s := &computepb.DiskSnapshot{
		Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: parent, Name: id, ResourceVersion: 1},
		Spec:     &computepb.DiskSnapshotSpec{SourceDiskId: diskID},
		Status:   &computepb.DiskSnapshotStatus{State: computepb.DiskSnapshotStatus_READY, ContentSizeBytes: 10 * gib, StorageSizeBytes: 10 * gib},
	}
	if edit != nil {
		edit(s)
	}
	if err := f.snaps.Create(t.Context(), s); err != nil {
		t.Fatal(err)
	}
}

// seedImage registers an ARM64 image in the lookup.
func (f *diskFixture) seedImage(id, parent, family string, state computepb.ImageStatus_State, minSize int64) {
	f.images.add(&computepb.Image{
		Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: parent, Name: id},
		Spec:     &computepb.ImageSpec{ImageFamily: family, CpuArchitecture: computepb.ImageSpec_ARM64},
		Status:   &computepb.ImageStatus{State: state, MinDiskSizeBytes: minSize},
	})
}
