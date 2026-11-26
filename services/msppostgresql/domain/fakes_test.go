package domain_test

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	pgpb "github.com/nebius/gosdk/proto/nebius/msp/postgresql/v1alpha1"
	resourcepb "github.com/nebius/gosdk/proto/nebius/msp/v1alpha1/resource"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/nid"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/services/msppostgresql/backend/mock"
	"github.com/kfirgollan/NebiusMocker/services/msppostgresql/domain"
)

const (
	projectID      = "project-e00mock000000000default"
	otherProjectID = "project-e00mock000000000other"
	defaultNetwork = "vpcnetwork-e00default"
	otherNetwork   = "vpcnetwork-e00other"
	foreignNetwork = "vpcnetwork-e00foreign"
)

var t0 = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

// fakeClusters is a map-backed ClusterRepository, names unique per project.
type fakeClusters struct {
	mu    sync.Mutex
	items map[string]*pgpb.Cluster
	order []string
}

func newFakeClusters() *fakeClusters { return &fakeClusters{items: map[string]*pgpb.Cluster{}} }

func (f *fakeClusters) Get(_ context.Context, id string) (*pgpb.Cluster, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.items[id]
	if !ok {
		return nil, errs.NotFound(id)
	}
	return proto.CloneOf(c), nil
}

func (f *fakeClusters) GetByName(_ context.Context, parentID, name string) (*pgpb.Cluster, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range f.order {
		if md := f.items[id].GetMetadata(); md.GetParentId() == parentID && md.GetName() == name {
			return proto.CloneOf(f.items[id]), nil
		}
	}
	return nil, errs.NotFound(name)
}

func (f *fakeClusters) All(context.Context) ([]*pgpb.Cluster, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*pgpb.Cluster
	for _, id := range f.order {
		out = append(out, proto.CloneOf(f.items[id]))
	}
	return out, nil
}

func (f *fakeClusters) List(ctx context.Context, parentID string, _ int, _ string) (domain.Page[*pgpb.Cluster], error) {
	all, _ := f.All(ctx)
	var out []*pgpb.Cluster
	for _, c := range all {
		if c.GetMetadata().GetParentId() == parentID {
			out = append(out, c)
		}
	}
	return domain.Page[*pgpb.Cluster]{Items: out}, nil
}

func (f *fakeClusters) Create(_ context.Context, c *pgpb.Cluster) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	md := c.GetMetadata()
	for _, id := range f.order {
		if o := f.items[id].GetMetadata(); o.GetParentId() == md.GetParentId() && o.GetName() == md.GetName() {
			return errs.AlreadyExists(id, "name taken")
		}
	}
	f.items[md.GetId()] = proto.CloneOf(c)
	f.order = append(f.order, md.GetId())
	return nil
}

func (f *fakeClusters) Update(_ context.Context, c *pgpb.Cluster) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := c.GetMetadata().GetId()
	if _, ok := f.items[id]; !ok {
		return errs.NotFound(id)
	}
	f.items[id] = proto.CloneOf(c)
	return nil
}

func (f *fakeClusters) Delete(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.items[id]; !ok {
		return errs.NotFound(id)
	}
	delete(f.items, id)
	f.order = slices.DeleteFunc(f.order, func(s string) bool { return s == id })
	return nil
}

// fakeBackups is a map-backed BackupRepository.
type fakeBackups struct {
	mu    sync.Mutex
	items map[string]domain.BackupRecord // backup ID
}

func newFakeBackups() *fakeBackups { return &fakeBackups{items: map[string]domain.BackupRecord{}} }

func clone(r domain.BackupRecord) domain.BackupRecord {
	r.Backup, r.Cluster = proto.CloneOf(r.Backup), proto.CloneOf(r.Cluster)
	return r
}

func (f *fakeBackups) Get(_ context.Context, clusterID, backupID string) (domain.BackupRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.items[backupID]
	if !ok || r.Backup.GetSourceClusterId() != clusterID {
		return domain.BackupRecord{}, errs.NotFound(backupID)
	}
	return clone(r), nil
}

func (f *fakeBackups) filter(keep func(domain.BackupRecord) bool) []domain.BackupRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []domain.BackupRecord
	for _, r := range f.items {
		if keep(r) {
			out = append(out, clone(r))
		}
	}
	slices.SortFunc(out, func(a, b domain.BackupRecord) int {
		return b.Backup.GetCreationStart().AsTime().Compare(a.Backup.GetCreationStart().AsTime())
	})
	return out
}

func (f *fakeBackups) ListByProject(_ context.Context, projectID string, _ int, _ string) (domain.Page[domain.BackupRecord], error) {
	return domain.Page[domain.BackupRecord]{Items: f.filter(func(r domain.BackupRecord) bool { return r.ProjectID == projectID })}, nil
}

func (f *fakeBackups) ListByCluster(_ context.Context, clusterID string, _ int, _ string) (domain.Page[domain.BackupRecord], error) {
	return domain.Page[domain.BackupRecord]{Items: f.filter(func(r domain.BackupRecord) bool { return r.Backup.GetSourceClusterId() == clusterID })}, nil
}

func (f *fakeBackups) All(context.Context) ([]domain.BackupRecord, error) {
	return f.filter(func(domain.BackupRecord) bool { return true }), nil
}

func (f *fakeBackups) Put(_ context.Context, b domain.BackupRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items[b.Backup.GetId()] = clone(b)
	return nil
}

func (f *fakeBackups) Delete(_ context.Context, _, backupID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.items, backupID)
	return nil
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

// fakeVPC is the vpc capabilities: networks, the default subnet and bindings.
type fakeVPC struct {
	mu    sync.Mutex
	bound map[string]bool // "network|user"
}

func newFakeVPC() *fakeVPC { return &fakeVPC{bound: map[string]bool{}} }

func (*fakeVPC) DefaultSubnet(_ context.Context, project string) (peer.Subnet, error) {
	if project != projectID {
		return peer.Subnet{}, errs.NotFound(project)
	}
	return peer.Subnet{ID: "vpcsubnet-e00default", ProjectID: projectID, NetworkID: defaultNetwork}, nil
}

func (*fakeVPC) Network(_ context.Context, id string) (peer.Network, error) {
	switch id {
	case defaultNetwork, otherNetwork:
		return peer.Network{ID: id, ProjectID: projectID}, nil
	case foreignNetwork:
		return peer.Network{ID: id, ProjectID: otherProjectID}, nil
	}
	return peer.Network{}, errs.NotFound(id)
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

// fakePeers hands out the vpc fake; nil is a disabled module.
type fakePeers struct{ vpc *fakeVPC }

func (p fakePeers) Networks() (peer.Networks, bool) { return p.vpc, p.vpc != nil }

func (p fakePeers) DefaultSubnets() (peer.DefaultSubnets, bool) { return p.vpc, p.vpc != nil }

func (fakePeers) ProjectRegions() (peer.ProjectRegions, bool) { return nil, false }

// fakeHierarchy knows one project besides the configured one.
type fakeHierarchy struct{}

func (fakeHierarchy) Parent(_ context.Context, id string) (string, error) {
	if id != otherProjectID {
		return "", errs.NotFound(id)
	}
	return "tenant-e00mock000000000default", nil
}

// backend is the mock backend that records its calls and can fail.
type backend struct {
	*mock.Backend
	mu         sync.Mutex
	ensureErr  error
	ensured    []domain.BackendCluster
	stopped    []string
	deleted    []string
	backups    []string
	backupErr  error
	deleteErr  error
	keepWAL    map[string]time.Time // backup ID → keepWALFrom of its deletion
	deletedBkp []string
	diskUsed   map[string]int64 // cluster ID → bytes CheckDisk reports
	diskErr    error
	diskLimits []int64 // limit of every CheckDisk call
}

func newBackend() *backend { return &backend{Backend: mock.New(), keepWAL: map[string]time.Time{}} }

func (b *backend) EnsureCluster(ctx context.Context, c domain.BackendCluster) (domain.Endpoints, error) {
	b.mu.Lock()
	err := b.ensureErr
	b.ensured = append(b.ensured, c)
	b.mu.Unlock()
	if err != nil {
		return domain.Endpoints{}, err
	}
	return b.Backend.EnsureCluster(ctx, c)
}

func (b *backend) StopCluster(ctx context.Context, id string) error {
	b.mu.Lock()
	b.stopped = append(b.stopped, id)
	b.mu.Unlock()
	return b.Backend.StopCluster(ctx, id)
}

func (b *backend) DeleteCluster(ctx context.Context, id string) error {
	b.mu.Lock()
	b.deleted = append(b.deleted, id)
	err := b.deleteErr
	b.mu.Unlock()
	if err != nil {
		return err
	}
	return b.Backend.DeleteCluster(ctx, id)
}

func (b *backend) CreateBackup(ctx context.Context, clusterID, backupID string) (domain.BackupInfo, error) {
	b.mu.Lock()
	err := b.backupErr
	b.backups = append(b.backups, backupID)
	b.mu.Unlock()
	if err != nil {
		return domain.BackupInfo{}, err
	}
	return b.Backend.CreateBackup(ctx, clusterID, backupID)
}

func (b *backend) DeleteBackup(_ context.Context, _, backupID string, keepWALFrom time.Time) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.keepWAL[backupID] = keepWALFrom
	b.deletedBkp = append(b.deletedBkp, backupID)
	return nil
}

func (b *backend) last() domain.BackendCluster {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.ensured[len(b.ensured)-1]
}

func (b *backend) ensureCalls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.ensured)
}

// inlineOps runs Prepare and Work immediately, like a zero-delay engine, and
// refuses a second operation on a resource that is busy (never here).
type inlineOps struct{ clk clock.Clock }

func (o inlineOps) Submit(ctx context.Context, s operations.Spec) (*operations.Operation, error) {
	if s.Prepare != nil {
		if err := s.Prepare(ctx); err != nil {
			return nil, err
		}
	}
	op := &operations.Operation{ID: "op-" + s.ResourceID, ResourceID: s.ResourceID, CreatedAt: o.clk.Now(), FinishedAt: o.clk.Now()}
	if s.Work != nil {
		op.Err = s.Work(ctx)
	}
	return op, nil
}

// env is a Service wired to fakes.
type env struct {
	svc      *domain.Service
	clk      *clock.Manual
	clusters *fakeClusters
	backups  *fakeBackups
	vpc      *fakeVPC
	backend  *backend
}

func newEnv(t *testing.T, opts ...func(*domain.Deps)) *env {
	t.Helper()
	ids, err := nid.NewGenerator(nid.Sequential, "e00", 0)
	if err != nil {
		t.Fatalf("nid.NewGenerator: %v", err)
	}
	clk := clock.NewManual(t0)
	e := &env{clk: clk, clusters: newFakeClusters(), backups: newFakeBackups(), vpc: newFakeVPC(), backend: newBackend()}
	deps := domain.Deps{
		Clusters: e.clusters, Backups: e.backups, Bindings: &fakeBindings{}, Backend: e.backend,
		Peers: fakePeers{vpc: e.vpc}, Hierarchy: fakeHierarchy{}, Ops: inlineOps{clk: clk}, IDs: ids, Clock: clk,
		Config: domain.Config{ProjectID: projectID, MSP: config.Default().MSP},
	}
	for _, o := range opts {
		o(&deps)
	}
	e.svc, err = domain.New(deps)
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

// validSpec is a Terraform-like minimal spec.
func validSpec() *pgpb.ClusterSpec {
	return &pgpb.ClusterSpec{
		Config: &pgpb.ConfigSpec{
			Version: "16",
			Template: &pgpb.TemplateSpec{
				Resources: &resourcepb.ResourcesSpec{Platform: "cpu-e2", Preset: "2vcpu-8gb"},
				Hosts:     &resourcepb.HostSpec{Count: 1},
				Disk:      &resourcepb.DiskSpec{Type: "network-ssd", SizeGibibytes: 20},
			},
		},
		Bootstrap: &pgpb.BootstrapSpec{UserName: "app", UserPassword: "s3cret-pass", DbName: "appdb"},
	}
}

func (e *env) create(t *testing.T, name string, mod func(*pgpb.ClusterSpec)) *pgpb.Cluster {
	t.Helper()
	spec := validSpec()
	if mod != nil {
		mod(spec)
	}
	id := done(t)(e.svc.CreateCluster(t.Context(), domain.CreateClusterCmd{
		Metadata: &commonpb.ResourceMetadata{ParentId: projectID, Name: name}, Spec: spec,
	}))
	return e.get(t, id)
}

func (e *env) get(t *testing.T, id string) *pgpb.Cluster {
	t.Helper()
	c, err := e.svc.GetCluster(t.Context(), id)
	if err != nil {
		t.Fatalf("GetCluster(%s): %v", id, err)
	}
	return c
}

func (e *env) backupsOf(t *testing.T, clusterID string) []domain.BackupRecord {
	t.Helper()
	p, err := e.svc.ListBackupsByCluster(t.Context(), clusterID, 0, "")
	if err != nil {
		t.Fatalf("ListBackupsByCluster: %v", err)
	}
	return p.Items
}
