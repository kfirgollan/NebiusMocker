package domain_test

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	mk8spb "github.com/nebius/gosdk/proto/nebius/mk8s/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/nid"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/services/mk8s/backend/mock"
	"github.com/kfirgollan/NebiusMocker/services/mk8s/domain"
)

const (
	projectID      = "project-e00mock000000000default"
	otherProjectID = "project-e00mock000000000other"
	defaultSubnet  = "vpcsubnet-e00default"
	defaultNetwork = "vpcnetwork-e00default"
	sameNetSubnet  = "vpcsubnet-e00second"
	otherNetSubnet = "vpcsubnet-e00othernet"
	foreignSubnet  = "vpcsubnet-e00foreign"
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// metadataOwner is the common shape of Cluster and NodeGroup.
type metadataOwner interface {
	proto.Message
	GetMetadata() *commonpb.ResourceMetadata
}

// fakeRepo is a map-backed repository with names unique per parent.
type fakeRepo[T metadataOwner] struct {
	mu    sync.Mutex
	items map[string]T
	order []string
}

func newFakeRepo[T metadataOwner]() *fakeRepo[T] { return &fakeRepo[T]{items: map[string]T{}} }

func (f *fakeRepo[T]) Get(_ context.Context, id string) (T, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.items[id]
	if !ok {
		var zero T
		return zero, errs.NotFound(id)
	}
	return proto.CloneOf(v), nil
}

func (f *fakeRepo[T]) GetByName(_ context.Context, parentID, name string) (T, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range f.order {
		if md := f.items[id].GetMetadata(); md.GetParentId() == parentID && md.GetName() == name {
			return proto.CloneOf(f.items[id]), nil
		}
	}
	var zero T
	return zero, errs.NotFound(name)
}

func (f *fakeRepo[T]) List(ctx context.Context, parentID string, _ int, _ string) (domain.Page[T], error) {
	all, err := f.All(ctx, parentID)
	return domain.Page[T]{Items: all}, err
}

func (f *fakeRepo[T]) All(_ context.Context, parentID string) ([]T, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []T
	for _, id := range f.order {
		if f.items[id].GetMetadata().GetParentId() == parentID {
			out = append(out, proto.CloneOf(f.items[id]))
		}
	}
	return out, nil
}

func (f *fakeRepo[T]) Create(_ context.Context, v T) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	md := v.GetMetadata()
	for _, id := range f.order {
		if o := f.items[id].GetMetadata(); o.GetParentId() == md.GetParentId() && o.GetName() == md.GetName() {
			return errs.AlreadyExists(id, "name taken")
		}
	}
	f.items[md.GetId()] = proto.CloneOf(v)
	f.order = append(f.order, md.GetId())
	return nil
}

func (f *fakeRepo[T]) Update(_ context.Context, v T) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := v.GetMetadata().GetId()
	if _, ok := f.items[id]; !ok {
		return errs.NotFound(id)
	}
	f.items[id] = proto.CloneOf(v)
	return nil
}

func (f *fakeRepo[T]) Delete(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.items[id]; !ok {
		return errs.NotFound(id)
	}
	delete(f.items, id)
	f.order = slices.DeleteFunc(f.order, func(s string) bool { return s == id })
	return nil
}

func (f *fakeRepo[T]) len() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.items)
}

// fakeBindings is the NetworkBindingRepository over a map.
type fakeBindings struct {
	mu sync.Mutex
	m  map[string]string
}

func (f *fakeBindings) Set(_ context.Context, cluster, network string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.m == nil {
		f.m = map[string]string{}
	}
	f.m[cluster] = network
	return nil
}

func (f *fakeBindings) Get(_ context.Context, cluster string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, ok := f.m[cluster]
	if !ok {
		return "", errs.NotFound(cluster)
	}
	return n, nil
}

func (f *fakeBindings) Delete(_ context.Context, cluster string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.m, cluster)
	return nil
}

// fakeVPC is the vpc capabilities: subnets, default subnets and network bindings.
type fakeVPC struct {
	mu      sync.Mutex
	subnets map[string]peer.Subnet
	bound   map[string]bool // "network|user"
}

func newFakeVPC() *fakeVPC {
	sn := func(id, project, network string) peer.Subnet {
		return peer.Subnet{ID: id, ProjectID: project, NetworkID: network}
	}
	return &fakeVPC{
		subnets: map[string]peer.Subnet{
			defaultSubnet:  sn(defaultSubnet, projectID, defaultNetwork),
			sameNetSubnet:  sn(sameNetSubnet, projectID, defaultNetwork),
			otherNetSubnet: sn(otherNetSubnet, projectID, "vpcnetwork-e00other"),
			foreignSubnet:  sn(foreignSubnet, otherProjectID, "vpcnetwork-e00foreign"),
		},
		bound: map[string]bool{},
	}
}

func (f *fakeVPC) Subnet(_ context.Context, id string) (peer.Subnet, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sn, ok := f.subnets[id]
	if !ok {
		return peer.Subnet{}, errs.NotFound(id)
	}
	return sn, nil
}

func (*fakeVPC) BindInterface(context.Context, peer.InterfaceBinding) (peer.InterfaceAddresses, error) {
	return peer.InterfaceAddresses{}, errs.Internal("not used by mk8s")
}

func (*fakeVPC) ReleaseInterface(context.Context, string, string) error {
	return errs.Internal("not used by mk8s")
}

func (f *fakeVPC) DefaultSubnet(ctx context.Context, project string) (peer.Subnet, error) {
	if project != projectID {
		return peer.Subnet{}, errs.NotFound(project)
	}
	return f.Subnet(ctx, defaultSubnet)
}

func (*fakeVPC) Network(_ context.Context, id string) (peer.Network, error) {
	return peer.Network{ID: id, ProjectID: projectID}, nil
}

func (f *fakeVPC) Bind(_ context.Context, network, user string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bound[network+"|"+user] = true
	return nil
}

func (f *fakeVPC) Unbind(_ context.Context, network, user string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.bound, network+"|"+user)
	return nil
}

func (f *fakeVPC) isBound(network, user string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bound[network+"|"+user]
}

// fakeCompute is peer.ComputeNodes over a map of node groups' instances.
type fakeCompute struct {
	mu      sync.Mutex
	next    int
	nodes   map[string][]peer.NodeInstance // node group → nodes in creation order
	specs   map[string]peer.NodeSpec       // instance ID → what created it
	presets map[string]int                 // "platform/preset" → GPUs
}

func newFakeCompute() *fakeCompute {
	return &fakeCompute{
		nodes: map[string][]peer.NodeInstance{}, specs: map[string]peer.NodeSpec{},
		presets: map[string]int{"cpu-d3/2vcpu-8gb": 0, "cpu-d3/4vcpu-16gb": 0, "gpu-h100-sxm/8gpu-128vcpu-1600gb": 8},
	}
}

func (f *fakeCompute) CheckTemplate(_ context.Context, project string, t peer.NodeTemplate) (peer.NodeResources, error) {
	gpus, ok := f.presets[t.Platform+"/"+t.Preset]
	if !ok {
		return peer.NodeResources{}, errs.BadRequest(errs.Violation{Field: "resources.preset", Message: "unknown preset"})
	}
	if t.ServiceAccountID != "" && project != projectID {
		return peer.NodeResources{}, errs.BadRequest(errs.Violation{Field: "service_account_id", Message: "wrong project"})
	}
	return peer.NodeResources{GPUs: gpus}, nil
}

func (f *fakeCompute) CreateNode(_ context.Context, n peer.NodeSpec) (peer.NodeInstance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, in := range f.nodes[n.NodeGroupID] {
		if in.Name == n.Name {
			return in, nil
		}
	}
	f.next++
	in := peer.NodeInstance{
		ID: fmt.Sprintf("computeinstance-%03d", f.next), Name: n.Name, PrivateIP: fmt.Sprintf("10.0.0.%d", f.next),
		Resources: peer.NodeResources{GPUs: f.presets[n.Template.Platform+"/"+n.Template.Preset]},
	}
	if n.Template.PublicIP {
		in.PublicIP = fmt.Sprintf("203.0.113.%d", f.next)
	}
	f.nodes[n.NodeGroupID] = append(f.nodes[n.NodeGroupID], in)
	f.specs[in.ID] = n
	return in, nil
}

func (f *fakeCompute) DeleteNode(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for g, nodes := range f.nodes {
		f.nodes[g] = slices.DeleteFunc(nodes, func(n peer.NodeInstance) bool { return n.ID == id })
	}
	delete(f.specs, id)
	return nil
}

func (f *fakeCompute) ListNodes(_ context.Context, groupID string) ([]peer.NodeInstance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.nodes[groupID]), nil
}

func (f *fakeCompute) instanceIDs(groupID string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ids []string
	for _, n := range f.nodes[groupID] {
		ids = append(ids, n.ID)
	}
	return ids
}

func (f *fakeCompute) spec(id string) peer.NodeSpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.specs[id]
}

func (f *fakeCompute) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.specs)
}

// fakeReleases records the clusters whose releases were deleted.
type fakeReleases struct {
	mu      sync.Mutex
	deleted []string
}

func (f *fakeReleases) DeleteClusterReleases(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, id)
	return nil
}

// fakePeers hands out the fakes; a nil field is a disabled module.
type fakePeers struct {
	compute  *fakeCompute
	vpc      *fakeVPC
	releases *fakeReleases
}

func (p fakePeers) ComputeNodes() (peer.ComputeNodes, bool) { return p.compute, p.compute != nil }

func (p fakePeers) DefaultSubnets() (peer.DefaultSubnets, bool) { return p.vpc, p.vpc != nil }

func (p fakePeers) NetworkInterfaces() (peer.NetworkInterfaces, bool) { return p.vpc, p.vpc != nil }

func (p fakePeers) Networks() (peer.Networks, bool) { return p.vpc, p.vpc != nil }

func (p fakePeers) ClusterReleases() (peer.ClusterReleases, bool) {
	return p.releases, p.releases != nil
}

// fakeHierarchy knows one project besides the configured one.
type fakeHierarchy struct{}

func (fakeHierarchy) Parent(_ context.Context, id string) (string, error) {
	if id != otherProjectID {
		return "", errs.NotFound(id)
	}
	return "tenant-e00mock000000000default", nil
}

// backend is the mock backend with switches: a failing control plane, nodes
// that never get ready, and a record of the last node set of every group.
type backend struct {
	*mock.Backend
	mu         sync.Mutex
	clusterErr error
	notReady   string // reason; "" means nodes get ready
	groups     map[string]domain.BackendNodeGroup
	// sizes are the node counts of every EnsureNodes call, per group.
	sizes   map[string][]int
	deleted []string
}

func newBackend() *backend {
	return &backend{Backend: mock.New(clock.NewManual(t0)), groups: map[string]domain.BackendNodeGroup{}, sizes: map[string][]int{}}
}

func (b *backend) EnsureCluster(ctx context.Context, c domain.BackendCluster) (domain.ClusterEndpoint, error) {
	b.mu.Lock()
	err := b.clusterErr
	b.mu.Unlock()
	if err != nil {
		return domain.ClusterEndpoint{}, err
	}
	return b.Backend.EnsureCluster(ctx, c)
}

func (b *backend) DeleteCluster(ctx context.Context, id string) error {
	b.mu.Lock()
	b.deleted = append(b.deleted, id)
	b.mu.Unlock()
	return b.Backend.DeleteCluster(ctx, id)
}

func (b *backend) EnsureNodes(ctx context.Context, g domain.BackendNodeGroup) ([]domain.NodeReadiness, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.groups[g.NodeGroupID] = g
	b.sizes[g.NodeGroupID] = append(b.sizes[g.NodeGroupID], len(g.Nodes))
	if b.notReady == "" {
		return b.Backend.EnsureNodes(ctx, g)
	}
	out := make([]domain.NodeReadiness, 0, len(g.Nodes))
	for _, n := range g.Nodes {
		out = append(out, domain.NodeReadiness{Name: n.Name, Reason: b.notReady})
	}
	return out, nil
}

func (b *backend) sizesOf(id string) []int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.sizes[id])
}

func (b *backend) group(id string) domain.BackendNodeGroup {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.groups[id]
}

// inlineOps runs Prepare and Work immediately, like a zero-delay engine.
type inlineOps struct{}

func (inlineOps) Submit(ctx context.Context, s operations.Spec) (*operations.Operation, error) {
	if s.Prepare != nil {
		if err := s.Prepare(ctx); err != nil {
			return nil, err
		}
	}
	op := &operations.Operation{ID: "op-" + s.ResourceID, ResourceID: s.ResourceID, CreatedAt: t0, FinishedAt: t0}
	if s.Work != nil {
		op.Err = s.Work(ctx)
	}
	return op, nil
}

// env is a Service wired to fakes.
type env struct {
	svc      *domain.Service
	clusters *fakeRepo[*mk8spb.Cluster]
	groups   *fakeRepo[*mk8spb.NodeGroup]
	compute  *fakeCompute
	vpc      *fakeVPC
	releases *fakeReleases
	backend  *backend
}

func newEnv(t *testing.T, versions ...string) *env {
	t.Helper()
	if len(versions) == 0 {
		versions = []string{"1.34", "1.35", "1.36"}
	}
	ids, err := nid.NewGenerator(nid.Sequential, "e00", 0)
	if err != nil {
		t.Fatalf("nid.NewGenerator: %v", err)
	}
	e := &env{
		clusters: newFakeRepo[*mk8spb.Cluster](), groups: newFakeRepo[*mk8spb.NodeGroup](),
		compute: newFakeCompute(), vpc: newFakeVPC(), releases: &fakeReleases{}, backend: newBackend(),
	}
	e.svc, err = domain.New(domain.Deps{
		Clusters: e.clusters, NodeGroups: e.groups, Bindings: &fakeBindings{}, Backend: e.backend,
		Peers: fakePeers{compute: e.compute, vpc: e.vpc, releases: e.releases}, Hierarchy: fakeHierarchy{},
		Ops: inlineOps{}, IDs: ids, Clock: clock.NewManual(t0),
		Config: domain.Config{ProjectID: projectID, Versions: versions},
	})
	if err != nil {
		t.Fatalf("domain.New: %v", err)
	}
	return e
}

// done checks that an operation was accepted and succeeded; it returns its
// resource ID. Use as done(t)(svc.Method(...)).
func done(t *testing.T) func(*operations.Operation, error) string {
	t.Helper()
	return func(op *operations.Operation, err error) string {
		t.Helper()
		if err != nil {
			t.Fatalf("operation: unexpected error: %v", err)
		}
		if op.Err != nil {
			t.Fatalf("operation %s failed: %v", op.ID, op.Err)
		}
		return op.ResourceID
	}
}

// createCluster creates a RUNNING cluster on version ("" for the default).
func (e *env) createCluster(t *testing.T, name, version string) *mk8spb.Cluster {
	t.Helper()
	id := done(t)(e.svc.CreateCluster(t.Context(), domain.CreateClusterCmd{
		Metadata: &commonpb.ResourceMetadata{ParentId: projectID, Name: name},
		Spec:     &mk8spb.ClusterSpec{ControlPlane: &mk8spb.ControlPlaneSpec{Version: version}},
	}))
	c, err := e.svc.GetCluster(t.Context(), id)
	if err != nil {
		t.Fatalf("GetCluster: %v", err)
	}
	return c
}

func preset(platform, name string) *mk8spb.ResourcesSpec {
	return &mk8spb.ResourcesSpec{Platform: platform, Size: &mk8spb.ResourcesSpec_Preset{Preset: name}}
}

// fixedSpec is a minimal CPU node group of n nodes.
func fixedSpec(n int64) *mk8spb.NodeGroupSpec {
	return &mk8spb.NodeGroupSpec{
		Size:     &mk8spb.NodeGroupSpec_FixedNodeCount{FixedNodeCount: n},
		Template: &mk8spb.NodeTemplate{Resources: preset("cpu-d3", "2vcpu-8gb")},
	}
}

func (e *env) createGroup(ctx context.Context, clusterID, name string, spec *mk8spb.NodeGroupSpec) (*operations.Operation, error) {
	return e.svc.CreateNodeGroup(ctx, domain.CreateNodeGroupCmd{
		Metadata: &commonpb.ResourceMetadata{ParentId: clusterID, Name: name}, Spec: spec,
	})
}

func (e *env) group(t *testing.T, id string) *mk8spb.NodeGroup {
	t.Helper()
	g, err := e.svc.GetNodeGroup(t.Context(), id)
	if err != nil {
		t.Fatalf("GetNodeGroup(%s): %v", id, err)
	}
	return g
}

// update replaces a node group's spec the way Terraform does (no reset mask
// needed: the full spec is sent).
func (e *env) update(ctx context.Context, g *mk8spb.NodeGroup, spec *mk8spb.NodeGroupSpec) (*operations.Operation, error) {
	return e.svc.UpdateNodeGroup(ctx, domain.UpdateNodeGroupCmd{Metadata: g.GetMetadata(), Spec: spec})
}

func sortedKeys(m map[string]string) []string { return slices.Sorted(maps.Keys(m)) }
