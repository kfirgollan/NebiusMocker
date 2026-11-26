package domain

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
)

type fakeFilesystems struct {
	repo memRepo[*computepb.Filesystem]

	mu      sync.Mutex
	exposed map[string][]string // filesystem ID -> bucket IDs
}

func (f *fakeFilesystems) Get(_ context.Context, id string) (*computepb.Filesystem, error) {
	return f.repo.get(id)
}

func (f *fakeFilesystems) GetByName(_ context.Context, parentID, name string) (*computepb.Filesystem, error) {
	return f.repo.getByName(parentID, name)
}

func (f *fakeFilesystems) List(_ context.Context, parentID string, size int, token string) (FilesystemPage, error) {
	items, next, err := f.repo.page(func(v *computepb.Filesystem) bool { return v.GetMetadata().GetParentId() == parentID }, size, token)
	return FilesystemPage{Items: items, NextPageToken: next}, err
}

func (f *fakeFilesystems) Create(_ context.Context, fs *computepb.Filesystem) error {
	return f.repo.create(fs)
}

func (f *fakeFilesystems) Mutate(_ context.Context, id string, fn func(*computepb.Filesystem) error) (*computepb.Filesystem, error) {
	return f.repo.mutate(id, fn)
}

func (f *fakeFilesystems) Delete(_ context.Context, id string) error { return f.repo.remove(id) }

func (f *fakeFilesystems) Expose(_ context.Context, fsID, bucketID string, check func(*computepb.Filesystem) error) error {
	fs, err := f.repo.get(fsID)
	if err != nil {
		return err
	}
	if err := check(fs); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.exposed == nil {
		f.exposed = map[string][]string{}
	}
	f.exposed[fsID] = append(f.exposed[fsID], bucketID)
	return nil
}

func (f *fakeFilesystems) Unexpose(_ context.Context, fsID, bucketID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.exposed[fsID] = slices.DeleteFunc(f.exposed[fsID], func(b string) bool { return b == bucketID })
	return nil
}

func (f *fakeFilesystems) MutateExposed(_ context.Context, id string, fn func(*computepb.Filesystem, []string) error) (*computepb.Filesystem, error) {
	f.mu.Lock()
	buckets := slices.Clone(f.exposed[id])
	f.mu.Unlock()
	return f.repo.mutate(id, func(fs *computepb.Filesystem) error { return fn(fs, buckets) })
}

// fakeFsData records Provision and Purge calls.
type fakeFsData struct {
	mu           sync.Mutex
	provisioned  []string
	purged       []string
	provisionErr error
}

func (f *fakeFsData) Bucket(id string) string { return "bucket-" + id }

func (f *fakeFsData) Provision(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.provisionErr != nil {
		return f.provisionErr
	}
	f.provisioned = append(f.provisioned, id)
	return nil
}

func (f *fakeFsData) Purge(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.purged = append(f.purged, id)
	return nil
}

type fsFixture struct {
	repo *fakeFilesystems
	data *fakeFsData
	ops  *deferOps
	clk  *clock.Manual
	gate *fakeQuotaGate
	svc  *FilesystemService
}

func newFsFixture(t *testing.T) *fsFixture {
	t.Helper()
	f := &fsFixture{
		repo: &fakeFilesystems{}, data: &fakeFsData{}, ops: &deferOps{}, gate: &fakeQuotaGate{},
		clk: clock.NewManual(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
	}
	svc, err := NewFilesystemService(FilesystemDeps{
		Filesystems: f.repo, OpIndex: &fakeOpIndex{}, Data: f.data,
		Projects: fakeProjects{testProject: true}, Ops: f.ops, IDs: &fakeIDs{}, Clock: f.clk, Quota: f.gate,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.svc = svc
	return f
}

func filesystemSpecGiB(gib int64) *computepb.FilesystemSpec {
	return &computepb.FilesystemSpec{
		Type: computepb.FilesystemSpec_NETWORK_SSD,
		Size: &computepb.FilesystemSpec_SizeGibibytes{SizeGibibytes: gib},
	}
}

// seed writes a READY 10 GiB filesystem straight into the fake repo.
func (f *fsFixture) seed(t *testing.T, id string, edit func(*computepb.Filesystem)) {
	t.Helper()
	fs := &computepb.Filesystem{
		Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: testProject, Name: id, ResourceVersion: 1},
		Spec:     filesystemSpecGiB(10),
		Status:   &computepb.FilesystemStatus{State: computepb.FilesystemStatus_READY, SizeBytes: 10 * gib, BlockSizeBytes: DefaultBlockSizeBytes},
	}
	if edit != nil {
		edit(fs)
	}
	if err := f.repo.Create(t.Context(), fs); err != nil {
		t.Fatal(err)
	}
}

func (f *fsFixture) get(t *testing.T, id string) *computepb.Filesystem {
	t.Helper()
	fs, err := f.repo.Get(t.Context(), id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	return fs
}

var errProvision = errors.New("bucket backend down")
