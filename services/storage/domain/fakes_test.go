package domain_test

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"sync"

	"google.golang.org/protobuf/proto"

	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/services/storage/domain"
)

const projectID = "project-e00test0000000000001"

// fakeBuckets is a slice-backed domain.BucketRepository with global names.
type fakeBuckets struct {
	mu    sync.Mutex
	items []*storagepb.Bucket
}

func (f *fakeBuckets) find(pred func(*storagepb.Bucket) bool, key string) (*storagepb.Bucket, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, b := range f.items {
		if pred(b) {
			return proto.CloneOf(b), nil
		}
	}
	return nil, errs.NotFound(key)
}

func (f *fakeBuckets) Get(_ context.Context, id string) (*storagepb.Bucket, error) {
	return f.find(func(b *storagepb.Bucket) bool { return b.GetMetadata().GetId() == id }, id)
}

func (f *fakeBuckets) GetByName(_ context.Context, parentID, name string) (*storagepb.Bucket, error) {
	return f.find(func(b *storagepb.Bucket) bool {
		return b.GetMetadata().GetParentId() == parentID && b.GetMetadata().GetName() == name
	}, name)
}

func (f *fakeBuckets) GetByBucketName(_ context.Context, name string) (*storagepb.Bucket, error) {
	return f.find(func(b *storagepb.Bucket) bool { return b.GetMetadata().GetName() == name }, name)
}

func (f *fakeBuckets) List(ctx context.Context, parentID string, _ int, _ string) (domain.BucketPage, error) {
	all, _ := f.ListAll(ctx)
	all = slices.DeleteFunc(all, func(b *storagepb.Bucket) bool { return b.GetMetadata().GetParentId() != parentID })
	return domain.BucketPage{Items: all}, nil
}

func (f *fakeBuckets) ListAll(context.Context) ([]*storagepb.Bucket, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*storagepb.Bucket, 0, len(f.items))
	for _, b := range f.items {
		out = append(out, proto.CloneOf(b))
	}
	return out, nil
}

func (f *fakeBuckets) Create(ctx context.Context, b *storagepb.Bucket) error {
	if existing, err := f.GetByBucketName(ctx, b.GetMetadata().GetName()); err == nil {
		return errs.AlreadyExists(existing.GetMetadata().GetId(), "name taken")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items = append(f.items, proto.CloneOf(b))
	return nil
}

func (f *fakeBuckets) Update(_ context.Context, b *storagepb.Bucket) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, e := range f.items {
		if e.GetMetadata().GetId() == b.GetMetadata().GetId() {
			f.items[i] = proto.CloneOf(b)
			return nil
		}
	}
	return errs.NotFound(b.GetMetadata().GetId())
}

func (f *fakeBuckets) Delete(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := len(f.items)
	f.items = slices.DeleteFunc(f.items, func(b *storagepb.Bucket) bool { return b.GetMetadata().GetId() == id })
	if len(f.items) == n {
		return errs.NotFound(id)
	}
	return nil
}

// fakeBackend records buckets, versioning and object counts; fail makes
// every call fail.
type fakeBackend struct {
	mu         sync.Mutex
	buckets    map[string]objstore.Versioning
	objects    map[string]int
	fail       error
	setCalls   int
	deleteHits []string
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{buckets: map[string]objstore.Versioning{}, objects: map[string]int{}}
}

func (f *fakeBackend) EnsureBucket(_ context.Context, b string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return f.fail
	}
	if _, ok := f.buckets[b]; !ok {
		f.buckets[b] = objstore.VersioningUnset
	}
	return nil
}

func (f *fakeBackend) DeleteBucket(_ context.Context, b string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return f.fail
	}
	delete(f.buckets, b)
	f.deleteHits = append(f.deleteHits, b)
	return nil
}

func (f *fakeBackend) BucketEmpty(_ context.Context, b string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.buckets[b]; !ok {
		return false, objstore.ErrBucketNotFound
	}
	return f.objects[b] == 0, nil
}

func (f *fakeBackend) SetVersioning(_ context.Context, b string, v objstore.Versioning) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return f.fail
	}
	f.setCalls++
	if v != objstore.VersioningUnset {
		f.buckets[b] = v
	}
	return nil
}

func (f *fakeBackend) versioning(b string) (objstore.Versioning, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.buckets[b]
	return v, ok
}

// fakeHierarchy knows projectID only.
type fakeHierarchy struct{}

func (fakeHierarchy) Parent(_ context.Context, id string) (string, error) {
	if id != projectID {
		return "", errs.NotFound(id)
	}
	return "tenant-e00test0000000000001", nil
}

// inlineOps runs Prepare and Work immediately, like a zero-delay engine.
type inlineOps struct{}

func (inlineOps) Submit(ctx context.Context, s operations.Spec) (*operations.Operation, error) {
	if s.Prepare != nil {
		if err := s.Prepare(ctx); err != nil {
			return nil, err
		}
	}
	op := &operations.Operation{ID: "operation-1", ResourceID: s.ResourceID}
	if s.Work != nil {
		op.Err = s.Work(ctx)
	}
	op.FinishedAt = t0
	return op, nil
}

func (inlineOps) Get(context.Context, string) (*operations.Operation, error) {
	return nil, errors.New("inlineOps: operations finish in Submit")
}

type seqIDs struct {
	mu sync.Mutex
	n  int
}

func (s *seqIDs) New(typ string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n++
	return typ + "-e00test" + strconv.Itoa(s.n), nil
}

// fakeChecker allows the permissions in allowed for every principal.
type fakeChecker struct{ allowed map[authz.Permission]bool }

func (f fakeChecker) Check(_ context.Context, _ string, perm authz.Permission, target string) (authz.Decision, error) {
	return authz.Decision{Allowed: f.allowed[perm], Permission: perm, Target: target, Reason: "fake"}, nil
}

// fakeKeys resolves one key.
type fakeKeys struct{ key peer.AccessKey }

func (f fakeKeys) ResolveAccessKey(_ context.Context, id string) (peer.AccessKey, error) {
	if id != f.key.AccessKeyID {
		return peer.AccessKey{}, errs.Unauthenticated("unknown key")
	}
	return f.key, nil
}

var errBackendDown = errors.New("backend down")
