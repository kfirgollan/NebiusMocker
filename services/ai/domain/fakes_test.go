package domain_test

import (
	"context"
	"log/slog"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	aipb "github.com/nebius/gosdk/proto/nebius/ai/v1"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/services/ai/domain"
)

const (
	testProject = "project-e00test0000000000001"
	testBaseURL = "http://dp.test"
)

// fakeRepo is a map-backed domain.EndpointRepository that keeps creation order.
type fakeRepo struct {
	mu    sync.Mutex
	order []string
	byID  map[string]*aipb.Endpoint
}

func newFakeRepo() *fakeRepo { return &fakeRepo{byID: map[string]*aipb.Endpoint{}} }

func (f *fakeRepo) Get(_ context.Context, id string) (*aipb.Endpoint, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.byID[id]
	if !ok {
		return nil, errs.NotFound(id)
	}
	return proto.CloneOf(e), nil
}

func (f *fakeRepo) GetByName(_ context.Context, parentID, name string) (*aipb.Endpoint, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range f.order {
		e := f.byID[id]
		if e.GetMetadata().GetParentId() == parentID && e.GetMetadata().GetName() == name {
			return proto.CloneOf(e), nil
		}
	}
	return nil, errs.NotFound(name)
}

// List pages by creation order; the page token is the decimal start offset.
func (f *fakeRepo) List(_ context.Context, parentID string, pageSize int, pageToken string) (domain.EndpointPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	start := 0
	if pageToken != "" {
		n, err := strconv.Atoi(pageToken)
		if err != nil {
			return domain.EndpointPage{}, errs.BadRequest(errs.Violation{Field: "page_token", Message: "invalid"})
		}
		start = n
	}
	var all []*aipb.Endpoint
	for _, id := range f.order {
		if e := f.byID[id]; e.GetMetadata().GetParentId() == parentID {
			all = append(all, e)
		}
	}
	start = min(start, len(all))
	end := min(start+pageSize, len(all))
	page := domain.EndpointPage{}
	for _, e := range all[start:end] {
		page.Items = append(page.Items, proto.CloneOf(e))
	}
	if end < len(all) {
		page.NextPageToken = strconv.Itoa(end)
	}
	return page, nil
}

func (f *fakeRepo) Create(_ context.Context, e *aipb.Endpoint) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	md := e.GetMetadata()
	for _, id := range f.order {
		o := f.byID[id].GetMetadata()
		if o.GetParentId() == md.GetParentId() && o.GetName() == md.GetName() {
			return errs.AlreadyExists(o.GetId(), "name taken")
		}
	}
	f.byID[md.GetId()] = proto.CloneOf(e)
	f.order = append(f.order, md.GetId())
	return nil
}

func (f *fakeRepo) Update(_ context.Context, e *aipb.Endpoint) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := e.GetMetadata().GetId()
	if _, ok := f.byID[id]; !ok {
		return errs.NotFound(id)
	}
	f.byID[id] = proto.CloneOf(e)
	return nil
}

func (f *fakeRepo) Delete(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.byID[id]; !ok {
		return errs.NotFound(id)
	}
	delete(f.byID, id)
	f.order = slices.DeleteFunc(f.order, func(o string) bool { return o == id })
	return nil
}

func (f *fakeRepo) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.byID)
}

// fakeHierarchy is a set of registered kinds and a set of existing IDs.
type fakeHierarchy struct {
	kinds map[string]bool
	ids   map[string]bool
}

func (h *fakeHierarchy) HasKind(kind string) bool { return h.kinds[kind] }

func (h *fakeHierarchy) Parent(_ context.Context, id string) (string, error) {
	if !h.ids[id] {
		return "", errs.NotFound(id)
	}
	return "", nil
}

// fakePayloads is a peer.SecretPayloads over a fixed set of secrets.
type fakePayloads struct {
	secrets []peer.SecretPayload
	err     error
}

func (f *fakePayloads) SecretPayload(_ context.Context, ref peer.SecretRef) (peer.SecretPayload, error) {
	if f.err != nil {
		return peer.SecretPayload{}, f.err
	}
	for _, p := range f.secrets {
		if (ref.SecretID != "" && p.SecretID == ref.SecretID) || (ref.SecretID == "" && p.VersionID == ref.VersionID) {
			return p, nil
		}
	}
	return peer.SecretPayload{}, errs.NotFound(ref.SecretID + ref.VersionID)
}

// fakeSecrets is a domain.SecretSource whose provider can come and go.
type fakeSecrets struct {
	mu sync.Mutex
	sp peer.SecretPayloads
}

func (f *fakeSecrets) SecretPayloads() (peer.SecretPayloads, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sp, f.sp != nil
}

func (f *fakeSecrets) set(sp peer.SecretPayloads) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sp = sp
}

type fakeDataPlane struct{}

func (fakeDataPlane) BaseURL() string { return testBaseURL }

// seqIDs is a sequential domain.IDGenerator: "<type>-<n>", counted per type.
type seqIDs struct {
	mu sync.Mutex
	n  map[string]int
}

func (s *seqIDs) New(typ string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.n == nil {
		s.n = map[string]int{}
	}
	s.n[typ]++
	return typ + "-" + strconv.Itoa(s.n[typ]), nil
}

// fakeOps runs Prepare inline and then Work, like a zero-delay engine. With
// hold set, Work is queued instead, so tests can see the "during" states.
type fakeOps struct {
	hold    bool
	pending []func()
}

func (o *fakeOps) Submit(ctx context.Context, s operations.Spec) (*operations.Operation, error) {
	if s.Prepare != nil {
		if err := s.Prepare(ctx); err != nil {
			return nil, err
		}
	}
	op := &operations.Operation{ID: "operation-1", ResourceID: s.ResourceID, Description: s.Description}
	if s.Work == nil {
		return op, nil
	}
	run := func() { op.Err = s.Work(ctx) }
	if o.hold {
		o.pending = append(o.pending, run)
		return op, nil
	}
	run()
	return op, nil
}

// release runs every held Work and stops holding.
func (o *fakeOps) release() {
	o.hold = false
	for _, run := range o.pending {
		run()
	}
	o.pending = nil
}

// opts configures newService.
type opts struct {
	kinds   []string // registered kinds in the hierarchy
	ids     []string // IDs that exist in the hierarchy
	secrets peer.SecretPayloads
}

// harness is a service with its fakes.
type harness struct {
	svc     *domain.EndpointService
	repo    *fakeRepo
	ops     *fakeOps
	secrets *fakeSecrets
}

func newService(t *testing.T, o opts) *harness {
	t.Helper()
	hier := &fakeHierarchy{kinds: map[string]bool{}, ids: map[string]bool{}}
	for _, k := range o.kinds {
		hier.kinds[k] = true
	}
	for _, id := range o.ids {
		hier.ids[id] = true
	}
	h := &harness{repo: newFakeRepo(), ops: &fakeOps{}, secrets: &fakeSecrets{sp: o.secrets}}
	svc, err := domain.NewEndpointService(domain.EndpointDeps{
		Endpoints: h.repo, Ops: h.ops, IDs: &seqIDs{}, Clock: clock.NewManual(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
		Hierarchy: hier, Secrets: h.secrets, DataPlane: fakeDataPlane{},
		Catalog: testCatalog(), Log: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("NewEndpointService: %v", err)
	}
	h.svc = svc
	return h
}

// testCatalog converts the default config catalog like services/ai/module.go.
func testCatalog() domain.Catalog {
	ps := config.DefaultAIPlatforms()
	out := make(domain.Catalog, 0, len(ps))
	for _, p := range ps {
		presets := make([]domain.Preset, 0, len(p.Presets))
		for _, pr := range p.Presets {
			presets = append(presets, domain.Preset{Name: pr.Name, GPUs: pr.GPUs, VCPUs: pr.VCPUs})
		}
		out = append(out, domain.Platform{Name: p.Name, GPU: p.GPU, CPU: p.CPU, Presets: presets})
	}
	return out
}

// baseSpec is a valid CPU spec with one authenticated HTTP port.
func baseSpec() *aipb.EndpointSpec {
	return &aipb.EndpointSpec{
		Image:     "nginx:latest",
		Platform:  "cpu-d3",
		Preset:    "4vcpu-16gb",
		Ports:     []*aipb.EndpointSpec_Port{{ContainerPort: 80, Protocol: aipb.EndpointSpec_Port_HTTP}},
		AuthToken: "s3cret",
	}
}

func createCmd(name string, spec *aipb.EndpointSpec) domain.CreateEndpointCmd {
	return domain.CreateEndpointCmd{
		Metadata: &commonpb.ResourceMetadata{ParentId: testProject, Name: name},
		Spec:     spec,
	}
}

// mustCreate creates an endpoint and returns it (full spec, via GetSecret).
func mustCreate(t *testing.T, h *harness, name string, spec *aipb.EndpointSpec) *aipb.Endpoint {
	t.Helper()
	op, err := h.svc.Create(t.Context(), createCmd(name, spec))
	if err != nil {
		t.Fatalf("Create(%q): %v", name, err)
	}
	e, err := h.svc.GetSecret(t.Context(), op.ResourceID)
	if err != nil {
		t.Fatalf("GetSecret(%s): %v", op.ResourceID, err)
	}
	return e
}

func mustGet(t *testing.T, h *harness, id string) *aipb.Endpoint {
	t.Helper()
	e, err := h.svc.Get(t.Context(), id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	return e
}
