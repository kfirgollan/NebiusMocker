package domain

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	registrypb "github.com/nebius/gosdk/proto/nebius/registry/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
)

type fakeRegistries struct {
	mu    sync.Mutex
	items []*registrypb.Registry
}

func (f *fakeRegistries) find(id string) int {
	return slices.IndexFunc(f.items, func(r *registrypb.Registry) bool { return r.GetMetadata().GetId() == id })
}

func (f *fakeRegistries) Get(_ context.Context, id string) (*registrypb.Registry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i := f.find(id); i >= 0 {
		return proto.CloneOf(f.items[i]), nil
	}
	return nil, errs.NotFound(id)
}

func (f *fakeRegistries) GetByName(_ context.Context, parentID, name string) (*registrypb.Registry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.items {
		if r.GetMetadata().GetParentId() == parentID && r.GetMetadata().GetName() == name {
			return proto.CloneOf(r), nil
		}
	}
	return nil, errs.NotFound(name)
}

func (f *fakeRegistries) List(_ context.Context, parentID string, _ int, _ string) (RegistryPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var p RegistryPage
	for _, r := range f.items {
		if r.GetMetadata().GetParentId() == parentID {
			p.Items = append(p.Items, proto.CloneOf(r))
		}
	}
	return p, nil
}

func (f *fakeRegistries) Create(_ context.Context, r *registrypb.Registry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, x := range f.items {
		if n := r.GetMetadata().GetName(); n != "" && x.GetMetadata().GetParentId() == r.GetMetadata().GetParentId() && x.GetMetadata().GetName() == n {
			return errs.AlreadyExists(x.GetMetadata().GetId(), "name taken")
		}
	}
	f.items = append(f.items, proto.CloneOf(r))
	return nil
}

func (f *fakeRegistries) Update(_ context.Context, r *registrypb.Registry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.find(r.GetMetadata().GetId())
	if i < 0 {
		return errs.NotFound(r.GetMetadata().GetId())
	}
	f.items[i] = proto.CloneOf(r)
	return nil
}

func (f *fakeRegistries) Delete(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.find(id)
	if i < 0 {
		return errs.NotFound(id)
	}
	f.items = slices.Delete(f.items, i, i+1)
	return nil
}

type fakeIndex struct {
	mu   sync.Mutex
	refs map[string]ArtifactRef
}

func newFakeIndex() *fakeIndex { return &fakeIndex{refs: map[string]ArtifactRef{}} }

func (f *fakeIndex) Lookup(_ context.Context, ref ArtifactRef) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, r := range f.refs {
		if r == ref {
			return id, nil
		}
	}
	return "", errs.NotFound(ref.Repository)
}

func (f *fakeIndex) Assign(_ context.Context, id string, ref ArtifactRef) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for x, r := range f.refs {
		if r == ref {
			return errs.AlreadyExists(x, "assigned")
		}
	}
	f.refs[id] = ref
	return nil
}

func (f *fakeIndex) Get(_ context.Context, id string) (ArtifactRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.refs[id]
	if !ok {
		return ArtifactRef{}, errs.NotFound(id)
	}
	return r, nil
}

func (f *fakeIndex) Delete(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.refs[id]; !ok {
		return errs.NotFound(id)
	}
	delete(f.refs, id)
	return nil
}

func (f *fakeIndex) DeleteRegistry(_ context.Context, registryID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, r := range f.refs {
		if r.RegistryID == registryID {
			delete(f.refs, id)
		}
	}
	return nil
}

type fakeBackend struct {
	mu   sync.Mutex
	ns   map[string][]Image
	fail error // returned by every call when set
}

func newFakeBackend() *fakeBackend { return &fakeBackend{ns: map[string][]Image{}} }

func (f *fakeBackend) EnsureNamespace(_ context.Context, ns string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return f.fail
	}
	if _, ok := f.ns[ns]; !ok {
		f.ns[ns] = nil
	}
	return nil
}

func (f *fakeBackend) DeleteNamespace(_ context.Context, ns string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.ns, ns)
	return f.fail
}

func (f *fakeBackend) ListImages(_ context.Context, ns string) ([]Image, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return nil, f.fail
	}
	imgs, ok := f.ns[ns]
	if !ok {
		return nil, errs.NotFound(ns)
	}
	return slices.Clone(imgs), nil
}

func (f *fakeBackend) DeleteImage(_ context.Context, ns, repo, digest string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	imgs := f.ns[ns]
	i := slices.IndexFunc(imgs, func(img Image) bool { return img.Repository == repo && img.Digest == digest })
	if i < 0 {
		return errs.NotFound(digest)
	}
	f.ns[ns] = slices.Delete(imgs, i, i+1)
	return nil
}

func (f *fakeBackend) put(ns string, imgs ...Image) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ns[ns] = append(f.ns[ns], imgs...)
}

func (f *fakeBackend) has(ns string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.ns[ns]
	return ok
}

// fakeHierarchy knows a fixed set of resources.
type fakeHierarchy map[string]string

func (h fakeHierarchy) Parent(_ context.Context, id string) (string, error) {
	p, ok := h[id]
	if !ok {
		return "", errs.NotFound(id)
	}
	return p, nil
}

type fixedEndpoint string

func (e fixedEndpoint) FQDN() string { return string(e) }

// inlineOps runs Prepare then Work immediately, like the engine with delay 0.
type inlineOps struct{}

func (inlineOps) Submit(ctx context.Context, spec operations.Spec) (*operations.Operation, error) {
	if spec.Prepare != nil {
		if err := spec.Prepare(ctx); err != nil {
			return nil, err
		}
	}
	op := &operations.Operation{ID: "operation-test", ResourceID: spec.ResourceID, FinishedAt: time.Unix(1, 0)}
	if spec.Work != nil {
		op.Err = spec.Work(ctx)
	}
	return op, nil
}

type seqIDs struct {
	mu sync.Mutex
	n  int
}

func (s *seqIDs) New(typ string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n++
	return fmt.Sprintf("%s-e00test%013d", typ, s.n), nil
}

type fakeTokens map[string]string

func (t fakeTokens) Principal(tok string) (string, bool) { p, ok := t[tok]; return p, ok }

// fakeAuthorizer allows a permission when the principal's grants contain its verb.
type fakeAuthorizer map[string][]string // principal → verbs

func (a fakeAuthorizer) Check(_ context.Context, principal string, perm authz.Permission, target string) (authz.Decision, error) {
	if !strings.HasPrefix(target, KindRegistry+"-") {
		return authz.Decision{}, errs.NotFound(target)
	}
	return authz.Decision{Allowed: slices.Contains(a[principal], perm.Verb()), Permission: perm, Target: target}, nil
}
