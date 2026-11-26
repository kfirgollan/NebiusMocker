package domain

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"
	"google.golang.org/protobuf/proto"

	kmspb "github.com/nebius/gosdk/proto/nebius/kms/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

const (
	testProject = "project-e00test"
	testTenant  = "tenant-e00test"
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

type fakeEntry[T keyMsg] struct {
	key      T
	material []byte
}

// fakeRepo is an in-memory KeyRepository honoring the port contract: names are
// unique among ACTIVE keys of a parent only, material is stored apart from the
// key, and protos are cloned on the way in and out.
type fakeRepo[T keyMsg] struct {
	state func(T) kmspb.KeyState

	mu      sync.Mutex
	entries map[string]*fakeEntry[T]
	order   []string
}

func newFakeRepo[T keyMsg](state func(T) kmspb.KeyState) *fakeRepo[T] {
	return &fakeRepo[T]{state: state, entries: map[string]*fakeEntry[T]{}}
}

func (r *fakeRepo[T]) nameTaken(k T) bool {
	md := k.GetMetadata()
	for _, e := range r.entries {
		emd := e.key.GetMetadata()
		if emd.GetId() != md.GetId() && emd.GetParentId() == md.GetParentId() &&
			emd.GetName() == md.GetName() && r.state(e.key) == kmspb.KeyState_ACTIVE {
			return true
		}
	}
	return false
}

func (r *fakeRepo[T]) Get(_ context.Context, id string) (T, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[id]
	if !ok {
		var zero T
		return zero, errs.NotFound(id)
	}
	return proto.CloneOf(e.key), nil
}

func (r *fakeRepo[T]) GetByName(_ context.Context, parentID, name string) (T, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, id := range r.order {
		e := r.entries[id]
		md := e.key.GetMetadata()
		if md.GetParentId() == parentID && md.GetName() == name && r.state(e.key) == kmspb.KeyState_ACTIVE {
			return proto.CloneOf(e.key), nil
		}
	}
	var zero T
	return zero, errs.NotFound(name)
}

func (r *fakeRepo[T]) List(_ context.Context, parentID string, pageSize int, pageToken string) (Page[T], error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	start := 0
	if pageToken != "" {
		n, err := strconv.Atoi(pageToken)
		if err != nil {
			return Page[T]{}, errs.BadRequest(errs.Violation{Field: "page_token", Message: "invalid"})
		}
		start = n
	}
	var all []T
	for _, id := range r.order {
		if e := r.entries[id]; e.key.GetMetadata().GetParentId() == parentID {
			all = append(all, proto.CloneOf(e.key))
		}
	}
	start = min(start, len(all))
	end := min(start+pageSize, len(all))
	page := Page[T]{Items: all[start:end]}
	if end < len(all) {
		page.NextPageToken = strconv.Itoa(end)
	}
	return page, nil
}

func (r *fakeRepo[T]) Create(_ context.Context, key T, material []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	id := key.GetMetadata().GetId()
	if _, ok := r.entries[id]; ok {
		return errs.AlreadyExists(id, "id exists")
	}
	if r.state(key) == kmspb.KeyState_ACTIVE && r.nameTaken(key) {
		return errs.AlreadyExists("", "name "+key.GetMetadata().GetName()+" is taken")
	}
	r.entries[id] = &fakeEntry[T]{key: proto.CloneOf(key), material: slices.Clone(material)}
	r.order = append(r.order, id)
	return nil
}

// unchanged is the compare-and-swap check; r.mu must be held.
func (r *fakeRepo[T]) unchanged(prev T) (*fakeEntry[T], error) {
	id := prev.GetMetadata().GetId()
	e, ok := r.entries[id]
	if !ok {
		return nil, errs.NotFound(id)
	}
	if !proto.Equal(e.key, prev) {
		return nil, errs.Conflict(id, "changed concurrently")
	}
	return e, nil
}

func (r *fakeRepo[T]) Update(_ context.Context, key, prev T) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, err := r.unchanged(prev)
	if err != nil {
		return err
	}
	if r.state(key) == kmspb.KeyState_ACTIVE && r.nameTaken(key) {
		return errs.AlreadyExists("", "name "+key.GetMetadata().GetName()+" is taken")
	}
	e.key = proto.CloneOf(key)
	return nil
}

func (r *fakeRepo[T]) Material(_ context.Context, id string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[id]
	if !ok {
		return nil, errs.NotFound(id)
	}
	return slices.Clone(e.material), nil
}

func (r *fakeRepo[T]) UpdateWithMaterial(_ context.Context, key T, material []byte, prev T) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, err := r.unchanged(prev)
	if err != nil {
		return err
	}
	e.key = proto.CloneOf(key)
	e.material = slices.Clone(material)
	return nil
}

func (r *fakeRepo[T]) Delete(_ context.Context, prev T) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.unchanged(prev); err != nil {
		return err
	}
	id := prev.GetMetadata().GetId()
	delete(r.entries, id)
	r.order = slices.DeleteFunc(r.order, func(x string) bool { return x == id })
	return nil
}

// fakeOps runs Prepare then Work inline.
type fakeOps struct {
	clk clock.Clock
	mu  sync.Mutex
	n   int
}

func (o *fakeOps) Submit(ctx context.Context, spec operations.Spec) (*operations.Operation, error) {
	if spec.Prepare != nil {
		if err := spec.Prepare(ctx); err != nil {
			return nil, err
		}
	}
	var werr error
	if spec.Work != nil {
		werr = spec.Work(ctx)
	}
	o.mu.Lock()
	o.n++
	n := o.n
	o.mu.Unlock()
	now := o.clk.Now()
	return &operations.Operation{
		ID: fmt.Sprintf("operation-e00test%04d", n), Description: spec.Description,
		CreatedAt: now, CreatedBy: spec.CreatedBy, FinishedAt: now,
		ResourceID: spec.ResourceID, Request: spec.Request, Err: werr,
	}, nil
}

// fakeHierarchy knows one project.
type fakeHierarchy struct{}

func (fakeHierarchy) Parent(_ context.Context, id string) (string, error) {
	if id == testProject {
		return testTenant, nil
	}
	return "", errs.NotFound(id)
}

// seqIDs yields <type>-e00test<seq>.
type seqIDs struct {
	mu sync.Mutex
	n  int
}

func (g *seqIDs) New(typ string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.n++
	return fmt.Sprintf("%s-e00test%04d", typ, g.n), nil
}

type symEnv struct {
	svc  *SymmetricKeyService
	repo *fakeRepo[*kmspb.SymmetricKey]
	clk  *clock.Manual
}

func newSymEnv(t *testing.T) *symEnv {
	t.Helper()
	clk := clock.NewManual(t0)
	repo := newFakeRepo(func(k *kmspb.SymmetricKey) kmspb.KeyState { return k.GetStatus().GetState() })
	svc, err := NewSymmetricKeyService(KeyDeps[*kmspb.SymmetricKey]{
		Keys: repo, Hierarchy: fakeHierarchy{}, Ops: &fakeOps{clk: clk}, IDs: &seqIDs{}, Clock: clk, Quota: passGate{},
	})
	if err != nil {
		t.Fatalf("NewSymmetricKeyService: %v", err)
	}
	return &symEnv{svc: svc, repo: repo, clk: clk}
}

type asymEnv struct {
	svc  *AsymmetricKeyService
	repo *fakeRepo[*kmspb.AsymmetricKey]
	clk  *clock.Manual
}

func newAsymEnv(t *testing.T) *asymEnv {
	t.Helper()
	clk := clock.NewManual(t0)
	repo := newFakeRepo(func(k *kmspb.AsymmetricKey) kmspb.KeyState { return k.GetStatus().GetState() })
	svc, err := NewAsymmetricKeyService(KeyDeps[*kmspb.AsymmetricKey]{
		Keys: repo, Hierarchy: fakeHierarchy{}, Ops: &fakeOps{clk: clk}, IDs: &seqIDs{}, Clock: clk, Quota: passGate{},
	})
	if err != nil {
		t.Fatalf("NewAsymmetricKeyService: %v", err)
	}
	return &asymEnv{svc: svc, repo: repo, clk: clk}
}

// passGate admits every claim and just runs apply.
type passGate struct{}

func (passGate) Admit(ctx context.Context, _ peer.QuotaClaim, apply func(ctx context.Context) error) error {
	return apply(ctx)
}
