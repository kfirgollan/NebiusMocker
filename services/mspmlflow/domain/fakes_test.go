package domain_test

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	mlflowpb "github.com/nebius/gosdk/proto/nebius/msp/mlflow/v1alpha1"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/services/mspmlflow/domain"
)

const (
	projectID      = "project-e00mock000000000default"
	otherProjectID = "project-e00mock000000000other"
	saID           = "serviceaccount-e00sa1"
	otherSAID      = "serviceaccount-e00sa2"
	defaultNetwork = "vpcnetwork-e00default"
	otherNetwork   = "vpcnetwork-e00other"
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// fakeClusters is a map-backed domain.ClusterRepository.
type fakeClusters struct {
	mu    sync.Mutex
	items []*mlflowpb.Cluster // creation order
}

func (f *fakeClusters) find(pred func(*mlflowpb.Cluster) bool, key string) (*mlflowpb.Cluster, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.items {
		if pred(c) {
			return proto.CloneOf(c), nil
		}
	}
	return nil, errs.NotFound(key)
}

func (f *fakeClusters) Get(_ context.Context, id string) (*mlflowpb.Cluster, error) {
	return f.find(func(c *mlflowpb.Cluster) bool { return c.GetMetadata().GetId() == id }, id)
}

func (f *fakeClusters) GetByName(_ context.Context, parentID, name string) (*mlflowpb.Cluster, error) {
	return f.find(func(c *mlflowpb.Cluster) bool {
		return c.GetMetadata().GetParentId() == parentID && c.GetMetadata().GetName() == name
	}, name)
}

func (f *fakeClusters) List(_ context.Context, parentID string, size int, token string) (domain.Page[*mlflowpb.Cluster], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var all []*mlflowpb.Cluster
	for _, c := range f.items {
		if c.GetMetadata().GetParentId() == parentID {
			all = append(all, proto.CloneOf(c))
		}
	}
	start := 0
	if token != "" {
		if _, err := fmt.Sscan(token, &start); err != nil {
			return domain.Page[*mlflowpb.Cluster]{}, errs.BadRequest(errs.Violation{Field: "page_token", Message: "malformed"})
		}
	}
	end := min(start+size, len(all))
	page := domain.Page[*mlflowpb.Cluster]{Items: all[start:end]}
	if end < len(all) {
		page.NextPageToken = strconv.Itoa(end)
	}
	return page, nil
}

func (f *fakeClusters) Create(_ context.Context, c *mlflowpb.Cluster) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range f.items {
		if e.GetMetadata().GetParentId() == c.GetMetadata().GetParentId() && e.GetMetadata().GetName() == c.GetMetadata().GetName() {
			return errs.AlreadyExists(e.GetMetadata().GetId(), "name taken")
		}
	}
	f.items = append(f.items, proto.CloneOf(c))
	return nil
}

func (f *fakeClusters) Update(_ context.Context, c *mlflowpb.Cluster) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, e := range f.items {
		if e.GetMetadata().GetId() == c.GetMetadata().GetId() {
			f.items[i] = proto.CloneOf(c)
			return nil
		}
	}
	return errs.NotFound(c.GetMetadata().GetId())
}

func (f *fakeClusters) Delete(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := len(f.items)
	f.items = slices.DeleteFunc(f.items, func(c *mlflowpb.Cluster) bool { return c.GetMetadata().GetId() == id })
	if len(f.items) == n {
		return errs.NotFound(id)
	}
	return nil
}

// fakeInternals is a map-backed domain.InternalsRepository.
type fakeInternals struct {
	mu sync.Mutex
	m  map[string]domain.Internals
}

func (f *fakeInternals) Put(_ context.Context, id string, in domain.Internals) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.m == nil {
		f.m = map[string]domain.Internals{}
	}
	f.m[id] = in
	return nil
}

func (f *fakeInternals) Get(_ context.Context, id string) (domain.Internals, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	in, ok := f.m[id]
	if !ok {
		return domain.Internals{}, errs.NotFound(id)
	}
	return in, nil
}

func (f *fakeInternals) Delete(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.m, id)
	return nil
}

// fakeHierarchy knows the service accounts and the other project.
type fakeHierarchy struct{}

func (fakeHierarchy) Parent(_ context.Context, id string) (string, error) {
	switch id {
	case saID:
		return projectID, nil
	case otherSAID:
		return otherProjectID, nil
	case otherProjectID:
		return "tenant-e00mock000000000default", nil
	}
	return "", errs.NotFound(id)
}

// fakeBuckets implements peer.Buckets.
type fakeBuckets struct {
	mu      sync.Mutex
	buckets map[string]peer.Bucket // by name, one project
	created []string
	racing  bool // CreateBucket reports AlreadyExists after creating it elsewhere
}

func (f *fakeBuckets) BucketByName(_ context.Context, project, name string) (peer.Bucket, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.buckets[name]
	if !ok || b.ProjectID != project {
		return peer.Bucket{}, errs.NotFound(name)
	}
	return b, nil
}

func (f *fakeBuckets) CreateBucket(_ context.Context, project, name string) (peer.Bucket, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.buckets == nil {
		f.buckets = map[string]peer.Bucket{}
	}
	if _, ok := f.buckets[name]; ok {
		return peer.Bucket{}, errs.AlreadyExists("", "bucket name taken")
	}
	b := peer.Bucket{ID: "bucket-" + name, ProjectID: project, Name: name}
	f.buckets[name] = b
	f.created = append(f.created, name)
	if f.racing {
		return peer.Bucket{}, errs.AlreadyExists(b.ID, "bucket name taken")
	}
	return b, nil
}

func (f *fakeBuckets) add(project, name string) {
	if f.buckets == nil {
		f.buckets = map[string]peer.Bucket{}
	}
	f.buckets[name] = peer.Bucket{ID: "bucket-" + name, ProjectID: project, Name: name}
}

// fakeKeys implements peer.SystemAccessKeys.
type fakeKeys struct {
	mu      sync.Mutex
	specs   []peer.SystemAccessKeySpec
	live    map[string]bool
	n       int
	failNew error
}

func (f *fakeKeys) CreateSystemAccessKey(_ context.Context, spec peer.SystemAccessKeySpec) (peer.SystemAccessKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNew != nil {
		return peer.SystemAccessKey{}, f.failNew
	}
	if f.live == nil {
		f.live = map[string]bool{}
	}
	f.n++
	k := peer.SystemAccessKey{KeyID: fmt.Sprintf("accesskey-e00k%d", f.n), AccessKeyID: fmt.Sprintf("NMKTEST%d", f.n), Secret: fmt.Sprintf("secret%d", f.n)}
	f.specs = append(f.specs, spec)
	f.live[k.KeyID] = true
	return k, nil
}

func (f *fakeKeys) DeleteSystemAccessKey(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.live, id)
	return nil
}

// fakeNetworks implements peer.Networks and peer.DefaultSubnets.
type fakeNetworks struct {
	mu       sync.Mutex
	bound    map[string]string // user ID -> network ID
	noDefult bool
}

func (f *fakeNetworks) Network(_ context.Context, id string) (peer.Network, error) {
	switch id {
	case defaultNetwork:
		return peer.Network{ID: id, ProjectID: projectID}, nil
	case otherNetwork:
		return peer.Network{ID: id, ProjectID: otherProjectID}, nil
	}
	return peer.Network{}, errs.NotFound(id)
}

func (f *fakeNetworks) Bind(_ context.Context, networkID, userID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if networkID != defaultNetwork && networkID != otherNetwork {
		return errs.NotFound(networkID)
	}
	if f.bound == nil {
		f.bound = map[string]string{}
	}
	f.bound[userID] = networkID
	return nil
}

func (f *fakeNetworks) Unbind(_ context.Context, _, userID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.bound, userID)
	return nil
}

func (f *fakeNetworks) DefaultSubnet(_ context.Context, project string) (peer.Subnet, error) {
	if f.noDefult || project != projectID {
		return peer.Subnet{}, errs.NotFound(project)
	}
	return peer.Subnet{ID: "vpcsubnet-e00default", NetworkID: defaultNetwork, ProjectID: project}, nil
}

type fakeRegions struct{}

func (fakeRegions) Region(context.Context, string) (string, error) { return "eu-north1", nil }

// fakePeers serves the fakes; a nil field is a disabled provider.
type fakePeers struct {
	networks *fakeNetworks
	buckets  *fakeBuckets
	keys     *fakeKeys
}

func (p fakePeers) Networks() (peer.Networks, bool) { return p.networks, p.networks != nil }
func (p fakePeers) DefaultSubnets() (peer.DefaultSubnets, bool) {
	return p.networks, p.networks != nil
}
func (fakePeers) ProjectRegions() (peer.ProjectRegions, bool) { return fakeRegions{}, true }
func (p fakePeers) Buckets() (peer.Buckets, bool)             { return p.buckets, p.buckets != nil }
func (p fakePeers) SystemAccessKeys() (peer.SystemAccessKeys, bool) {
	return p.keys, p.keys != nil
}

// fakeBackend records calls and can fail.
type fakeBackend struct {
	mu          sync.Mutex
	ensured     []domain.BackendCluster
	stopped     []string
	deleted     []string
	experiments int
	expErr      error
	ensureErr   error
	stopErr     error
	deleteErr   error
}

func (b *fakeBackend) Name() string { return "fake" }

func (b *fakeBackend) EnsureCluster(_ context.Context, c domain.BackendCluster) (domain.Endpoints, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ensureErr != nil {
		return domain.Endpoints{}, b.ensureErr
	}
	b.ensured = append(b.ensured, c)
	ep := domain.Endpoints{Private: c.ID + ".private.test:443"}
	if c.PublicAccess {
		ep.Public = c.ID + ".test:443"
	}
	return ep, nil
}

func (b *fakeBackend) StopCluster(_ context.Context, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.stopErr != nil {
		return b.stopErr
	}
	b.stopped = append(b.stopped, id)
	return nil
}

func (b *fakeBackend) DeleteCluster(_ context.Context, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.deleteErr != nil {
		return b.deleteErr
	}
	b.deleted = append(b.deleted, id)
	return nil
}

func (b *fakeBackend) ExperimentsCount(context.Context, string) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.experiments, b.expErr
}

func (*fakeBackend) Close(context.Context) error { return nil }

// inlineOps runs Prepare and Work immediately, like a zero-delay engine.
type inlineOps struct{}

func (inlineOps) Submit(ctx context.Context, s operations.Spec) (*operations.Operation, error) {
	if s.Prepare != nil {
		if err := s.Prepare(ctx); err != nil {
			return nil, err
		}
	}
	op := &operations.Operation{ID: "op-" + s.ResourceID, ResourceID: s.ResourceID, Request: s.Request, CreatedAt: t0, FinishedAt: t0}
	if s.Work != nil {
		op.Err = s.Work(ctx)
	}
	return op, nil
}

type seqIDs struct{ n int }

func (s *seqIDs) New(typ string) (string, error) {
	s.n++
	return fmt.Sprintf("%s-e00test%04d", typ, s.n), nil
}

// env is a Service wired to fakes.
type env struct {
	svc      *domain.Service
	clusters *fakeClusters
	internal *fakeInternals
	backend  *fakeBackend
	peers    fakePeers
	clk      *clock.Manual
	quota    *fakeQuota
}

func newEnv(t *testing.T) *env {
	t.Helper()
	return newEnvWith(t, func(p fakePeers) fakePeers { return p })
}

// newEnvWith builds the environment with its peers adjusted by edit.
func newEnvWith(t *testing.T, edit func(fakePeers) fakePeers) *env {
	t.Helper()
	e := &env{
		clusters: &fakeClusters{}, internal: &fakeInternals{}, backend: &fakeBackend{}, clk: clock.NewManual(t0), quota: &fakeQuota{},
		peers: fakePeers{networks: &fakeNetworks{}, buckets: &fakeBuckets{}, keys: &fakeKeys{}},
	}
	e.peers = edit(e.peers)
	svc, err := domain.New(domain.Deps{
		Clusters: e.clusters, Internals: e.internal, Backend: e.backend, Peers: e.peers, Hierarchy: fakeHierarchy{},
		Quota: e.quota, Ops: inlineOps{}, IDs: &seqIDs{}, Clock: e.clk,
		Config: domain.Config{
			ProjectID: projectID, MLflowVersion: "2.22.0",
			Sizes: map[string]string{"medium": "8vcpu-32gb", "large": "16vcpu-64gb"}, SizeOrder: []string{"medium", "large"},
		},
	})
	if err != nil {
		t.Fatalf("domain.New: %v", err)
	}
	e.svc = svc
	return e
}
