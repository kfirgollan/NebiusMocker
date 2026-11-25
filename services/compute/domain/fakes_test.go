package domain

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"maps"
	"slices"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
)

type fakeImages struct {
	mu    sync.Mutex
	byID  map[string]*computepb.Image
	order []string
}

func newFakeImages() *fakeImages { return &fakeImages{byID: map[string]*computepb.Image{}} }

func (f *fakeImages) Get(_ context.Context, id string) (*computepb.Image, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	img, ok := f.byID[id]
	if !ok {
		return nil, errs.NotFound(id)
	}
	return proto.CloneOf(img), nil
}

func (f *fakeImages) GetByName(ctx context.Context, parentID, name string) (*computepb.Image, error) {
	all, _ := f.All(ctx, parentID)
	for _, img := range all {
		if img.GetMetadata().GetName() == name {
			return img, nil
		}
	}
	return nil, errs.NotFound(name)
}

func (f *fakeImages) List(ctx context.Context, parentID string, pageSize int, _ string) (ImagePage, error) {
	all, _ := f.All(ctx, parentID)
	return ImagePage{Items: all[:min(pageSize, len(all))]}, nil
}

func (f *fakeImages) All(_ context.Context, parentID string) ([]*computepb.Image, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*computepb.Image
	for _, id := range f.order {
		if img, ok := f.byID[id]; ok && img.GetMetadata().GetParentId() == parentID {
			out = append(out, proto.CloneOf(img))
		}
	}
	return out, nil
}

func (f *fakeImages) Create(_ context.Context, img *computepb.Image) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.byID[img.GetMetadata().GetId()]; ok {
		return errs.AlreadyExists(img.GetMetadata().GetId(), "exists")
	}
	f.byID[img.GetMetadata().GetId()] = proto.CloneOf(img)
	f.order = append(f.order, img.GetMetadata().GetId())
	return nil
}

func (f *fakeImages) Update(_ context.Context, img *computepb.Image) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.byID[img.GetMetadata().GetId()]; !ok {
		return errs.NotFound(img.GetMetadata().GetId())
	}
	f.byID[img.GetMetadata().GetId()] = proto.CloneOf(img)
	return nil
}

func (f *fakeImages) Delete(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.byID[id]; !ok {
		return errs.NotFound(id)
	}
	delete(f.byID, id)
	return nil
}

type fakeOpIndex struct {
	mu  sync.Mutex
	ids map[string][]string
}

func (f *fakeOpIndex) Add(_ context.Context, parentID, opID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ids == nil {
		f.ids = map[string][]string{}
	}
	f.ids[parentID] = append(f.ids[parentID], opID)
	return nil
}

func (f *fakeOpIndex) IDs(_ context.Context, parentID string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.ids[parentID]), nil
}

type blob struct {
	data []byte
	meta map[string]string
}

// fakeBlobs keeps image blobs and user-bucket objects in maps. mutate, when
// set, is called before each OpenSource (to simulate concurrent writers).
type fakeBlobs struct {
	mu      sync.Mutex
	images  map[string]blob
	sources map[string][]byte // "bucket/object" → bytes
	opens   int
	mutate  func(opens int, sources map[string][]byte)
	putHook func(imageID string) // called before PutImage stores, outside the lock
}

func newFakeBlobs() *fakeBlobs {
	return &fakeBlobs{images: map[string]blob{}, sources: map[string][]byte{}}
}

func (f *fakeBlobs) PutImage(_ context.Context, id string, r io.Reader, size int64, meta map[string]string) (BlobInfo, error) {
	if f.putHook != nil {
		f.putHook(id)
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return BlobInfo{}, err
	}
	if int64(len(data)) != size {
		return BlobInfo{}, fmt.Errorf("size %d, read %d", size, len(data))
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.images[id] = blob{data: data, meta: maps.Clone(meta)}
	return BlobInfo{Size: size, Metadata: maps.Clone(meta)}, nil
}

func (f *fakeBlobs) StatImage(_ context.Context, id string) (BlobInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.images[id]
	if !ok {
		return BlobInfo{}, errs.NotFound(id)
	}
	return BlobInfo{Size: int64(len(b.data)), Metadata: maps.Clone(b.meta)}, nil
}

func (f *fakeBlobs) DeleteImage(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.images, id)
	return nil
}

func (f *fakeBlobs) OpenSource(_ context.Context, bucket, object string) (io.ReadCloser, BlobInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opens++
	if f.mutate != nil {
		f.mutate(f.opens, f.sources)
	}
	data, ok := f.sources[bucket+"/"+object]
	if !ok {
		return nil, BlobInfo{}, errs.NotFound(bucket + "/" + object)
	}
	return io.NopCloser(bytes.NewReader(slices.Clone(data))), BlobInfo{Size: int64(len(data))}, nil
}

func (f *fakeBlobs) StatSource(_ context.Context, bucket, object string) (BlobInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.sources[bucket+"/"+object]
	if !ok {
		return BlobInfo{}, errs.NotFound(bucket + "/" + object)
	}
	return BlobInfo{Size: int64(len(data))}, nil
}

type fakeProjects map[string]bool

func (f fakeProjects) Parent(_ context.Context, id string) (string, error) {
	if !f[id] {
		return "", errs.NotFound(id)
	}
	return "tenant-e00mock0000000000default", nil
}

// fakeOps runs Prepare and Work inline, like the engine with no delay.
type fakeOps struct {
	mu  sync.Mutex
	n   int
	ops map[string]*operations.Operation
}

func (f *fakeOps) Submit(ctx context.Context, spec operations.Spec) (*operations.Operation, error) {
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
	if spec.Work != nil {
		op.Err = spec.Work(ctx)
	}
	op.FinishedAt = time.Unix(1, 0)
	return op, nil
}

func (f *fakeOps) Get(_ context.Context, id string) (*operations.Operation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	op, ok := f.ops[id]
	if !ok {
		return nil, errs.NotFound(id)
	}
	return op, nil
}

type fakeIDs struct {
	mu sync.Mutex
	n  int
}

func (f *fakeIDs) New(typ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	return fmt.Sprintf("%s-e00mock%013d", typ, f.n), nil
}

const testProject = "project-e00mock000000000default"

var testRegions = []Region{{Name: "eu-north1", RoutingCode: "e00"}, {Name: "us-central1", RoutingCode: "u00"}}

type fixture struct {
	svc    *ImageService
	images *fakeImages
	blobs  *fakeBlobs
	ops    *fakeOps
	clk    *clock.Manual
	disks  *stubDiskReader
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{
		images: newFakeImages(), blobs: newFakeBlobs(), ops: &fakeOps{}, disks: newStubDiskReader(),
		clk: clock.NewManual(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
	}
	svc, err := NewImageService(ImageDeps{
		Images: f.images, OpIndex: &fakeOpIndex{}, Blobs: f.blobs, Projects: fakeProjects{testProject: true}, Disks: f.disks, DiskLocks: f.disks,
		Ops: f.ops, IDs: &fakeIDs{}, Clock: f.clk, Regions: testRegions,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.svc = svc
	return f
}
