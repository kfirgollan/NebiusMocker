package domain

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
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
)

// txOverlay stages the writes of one transaction over a memRepo; commit
// applies them, dropping the overlay discards them.
type txOverlay[T interface {
	proto.Message
	GetMetadata() *commonpb.ResourceMetadata
}] struct {
	base  *memRepo[T]
	puts  map[string]T
	gone  map[string]bool
	added []string
}

func newOverlay[T interface {
	proto.Message
	GetMetadata() *commonpb.ResourceMetadata
}](base *memRepo[T]) *txOverlay[T] {
	return &txOverlay[T]{base: base, puts: map[string]T{}, gone: map[string]bool{}}
}

func (o *txOverlay[T]) get(id string) (T, error) {
	if o.gone[id] {
		var zero T
		return zero, errs.NotFound(id)
	}
	if v, ok := o.puts[id]; ok {
		return proto.CloneOf(v), nil
	}
	return o.base.get(id)
}

func (o *txOverlay[T]) create(v T) error {
	md := v.GetMetadata()
	if _, err := o.get(md.GetId()); err == nil {
		return errs.AlreadyExists(md.GetId(), "id exists")
	}
	taken := func(other T) bool {
		m := other.GetMetadata()
		return !o.gone[m.GetId()] && m.GetParentId() == md.GetParentId() && m.GetName() == md.GetName()
	}
	for _, other := range o.puts {
		if taken(other) {
			return errs.AlreadyExists(other.GetMetadata().GetId(), "name exists")
		}
	}
	o.base.mu.Lock()
	defer o.base.mu.Unlock()
	for id, other := range o.base.byID {
		if _, shadowed := o.puts[id]; !shadowed && taken(other) {
			return errs.AlreadyExists(id, "name exists")
		}
	}
	o.puts[md.GetId()] = proto.CloneOf(v)
	delete(o.gone, md.GetId())
	o.added = append(o.added, md.GetId())
	return nil
}

func (o *txOverlay[T]) put(v T) error {
	id := v.GetMetadata().GetId()
	if _, err := o.get(id); err != nil {
		return err
	}
	o.puts[id] = proto.CloneOf(v)
	return nil
}

func (o *txOverlay[T]) remove(id string) error {
	if _, err := o.get(id); err != nil {
		return err
	}
	delete(o.puts, id)
	o.gone[id] = true
	return nil
}

func (o *txOverlay[T]) commit() {
	o.base.mu.Lock()
	defer o.base.mu.Unlock()
	if o.base.byID == nil {
		o.base.byID = map[string]T{}
	}
	for id := range o.gone {
		delete(o.base.byID, id)
	}
	for id, v := range o.puts {
		if _, exists := o.base.byID[id]; !exists && !slices.Contains(o.base.order, id) {
			o.base.order = append(o.base.order, id)
		}
		o.base.byID[id] = v
	}
}

// fakeInstanceRepo is an in-memory InstanceRepository. Its disks are the
// repo of a fakeDisks, so the DiskService sees managed disks. Txn applies
// its writes only when fn returns nil.
type fakeInstanceRepo struct {
	txn       sync.Mutex
	instances memRepo[*computepb.Instance]
	disks     *memRepo[*computepb.Disk]
}

func (f *fakeInstanceRepo) Get(_ context.Context, id string) (*computepb.Instance, error) {
	return f.instances.get(id)
}

func (f *fakeInstanceRepo) GetByName(_ context.Context, parentID, name string) (*computepb.Instance, error) {
	return f.instances.getByName(parentID, name)
}

func (f *fakeInstanceRepo) List(_ context.Context, parentID string, size int, token string) (InstancePage, error) {
	items, next, err := f.instances.page(func(i *computepb.Instance) bool { return i.GetMetadata().GetParentId() == parentID }, size, token)
	return InstancePage{Items: items, NextPageToken: next}, err
}

func (f *fakeInstanceRepo) Txn(_ context.Context, fn func(tx InstanceTx) error) error {
	f.txn.Lock()
	defer f.txn.Unlock()
	tx := &fakeInstanceTx{inst: newOverlay(&f.instances), disks: newOverlay(f.disks)}
	if err := fn(tx); err != nil {
		return err
	}
	tx.inst.commit()
	tx.disks.commit()
	return nil
}

func (f *fakeInstanceRepo) count() int {
	f.instances.mu.Lock()
	defer f.instances.mu.Unlock()
	return len(f.instances.byID)
}

type fakeInstanceTx struct {
	inst  *txOverlay[*computepb.Instance]
	disks *txOverlay[*computepb.Disk]
}

func (t *fakeInstanceTx) Instance(id string) (*computepb.Instance, error) { return t.inst.get(id) }
func (t *fakeInstanceTx) CreateInstance(i *computepb.Instance) error      { return t.inst.create(i) }
func (t *fakeInstanceTx) PutInstance(i *computepb.Instance) error         { return t.inst.put(i) }
func (t *fakeInstanceTx) DeleteInstance(id string) error                  { return t.inst.remove(id) }
func (t *fakeInstanceTx) Disk(id string) (*computepb.Disk, error)         { return t.disks.get(id) }
func (t *fakeInstanceTx) CreateDisk(d *computepb.Disk) error              { return t.disks.create(d) }
func (t *fakeInstanceTx) PutDisk(d *computepb.Disk) error                 { return t.disks.put(d) }
func (t *fakeInstanceTx) DeleteDisk(id string) error                      { return t.disks.remove(id) }

// fakeNet is a peer.NetworkInterfaces that hands out 10.0.0.N / 203.0.113.N
// and records what is bound. failNIC makes BindInterface fail for a NIC name.
type fakeNet struct {
	mu      sync.Mutex
	subnets map[string]peer.Subnet
	bound   map[string]peer.InterfaceBinding // "instance/nic"
	addrs   map[string]peer.InterfaceAddresses
	failNIC map[string]error
	binds   []string // "instance/nic" per BindInterface call that succeeded
	freed   []string // "instance/nic" per ReleaseInterface call
	priv    int
	pub     int
}

func newFakeNet() *fakeNet {
	return &fakeNet{
		subnets: map[string]peer.Subnet{
			"subnet-a":     {ID: "subnet-a", ProjectID: testProject, NetworkID: "network-1"},
			"subnet-b":     {ID: "subnet-b", ProjectID: testProject, NetworkID: "network-1"},
			"subnet-other": {ID: "subnet-other", ProjectID: otherProject, NetworkID: "network-2"},
		},
		bound: map[string]peer.InterfaceBinding{}, addrs: map[string]peer.InterfaceAddresses{}, failNIC: map[string]error{},
	}
}

func (f *fakeNet) NetworkInterfaces() (peer.NetworkInterfaces, bool) { return f, true }

func (f *fakeNet) Subnet(_ context.Context, id string) (peer.Subnet, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sn, ok := f.subnets[id]
	if !ok {
		return peer.Subnet{}, errs.NotFound(id)
	}
	return sn, nil
}

func (f *fakeNet) BindInterface(_ context.Context, b peer.InterfaceBinding) (peer.InterfaceAddresses, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.failNIC[b.Name]; ok {
		return peer.InterfaceAddresses{}, err
	}
	key := b.InstanceID + "/" + b.Name
	cur := f.addrs[key]
	a := peer.InterfaceAddresses{NetworkID: f.subnets[b.SubnetID].NetworkID}
	switch {
	case b.PrivateAllocationID != "":
		a.PrivateAddress, a.PrivateAllocationID = "10.0.99.1", b.PrivateAllocationID
	case cur.PrivateAddress != "" && cur.PrivateAllocationID == b.InstanceID+"-"+b.Name+"-private":
		a.PrivateAddress, a.PrivateAllocationID = cur.PrivateAddress, cur.PrivateAllocationID
	default:
		f.priv++
		a.PrivateAddress, a.PrivateAllocationID = fmt.Sprintf("10.0.0.%d", f.priv), b.InstanceID+"-"+b.Name+"-private"
	}
	switch {
	case b.Public == nil:
	case b.Public.AllocationID != "":
		a.PublicAddress, a.PublicAllocationID = "203.0.113.200", b.Public.AllocationID
	case cur.PublicAddress != "" && cur.PublicAllocationID == b.InstanceID+"-"+b.Name+"-public":
		a.PublicAddress, a.PublicAllocationID = cur.PublicAddress, cur.PublicAllocationID
	default:
		f.pub++
		a.PublicAddress, a.PublicAllocationID = fmt.Sprintf("203.0.113.%d", f.pub), b.InstanceID+"-"+b.Name+"-public"
	}
	for i := range b.AliasAllocationIDs {
		a.AliasCIDRs = append(a.AliasCIDRs, fmt.Sprintf("10.1.%d.0/24", i))
	}
	a.SecurityGroupIDs = b.SecurityGroupIDs
	if len(a.SecurityGroupIDs) == 0 {
		a.SecurityGroupIDs = []string{"securitygroup-default"}
	}
	f.bound[key], f.addrs[key] = b, a
	f.binds = append(f.binds, key)
	return a, nil
}

func (f *fakeNet) ReleaseInterface(_ context.Context, instanceID, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := instanceID + "/" + name
	delete(f.bound, key)
	delete(f.addrs, key)
	f.freed = append(f.freed, key)
	return nil
}

// boundNICs returns the "instance/nic" keys that are bound now, sorted.
func (f *fakeNet) boundNICs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Sorted(maps.Keys(f.bound))
}

func (f *fakeNet) binding(instanceID, nic string) (peer.InterfaceBinding, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.bound[instanceID+"/"+nic]
	return b, ok
}

// fakeGpu records GPU cluster joins and leaves.
type fakeGpu struct {
	mu       sync.Mutex
	clusters map[string]bool
	members  map[string][]string
	joins    []string // "cluster/instance/platform"
	leaves   []string // "cluster/instance"
}

func newFakeGpu(clusters ...string) *fakeGpu {
	f := &fakeGpu{clusters: map[string]bool{}, members: map[string][]string{}}
	for _, c := range clusters {
		f.clusters[c] = true
	}
	return f
}

func (f *fakeGpu) Join(_ context.Context, clusterID, instanceID, projectID, platform string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.clusters[clusterID] {
		return nil, errs.NotFound(clusterID)
	}
	if projectID != testProject {
		return nil, errs.BadRequest(errs.Violation{Field: "spec.gpu_cluster.id", Message: "cluster is in another project"})
	}
	if !slices.Contains(f.members[clusterID], instanceID) {
		f.members[clusterID] = append(f.members[clusterID], instanceID)
	}
	f.joins = append(f.joins, clusterID+"/"+instanceID+"/"+platform)
	return []string{"spine-1", "leaf-" + clusterID}, nil
}

func (f *fakeGpu) Leave(_ context.Context, clusterID, instanceID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.members[clusterID] = slices.DeleteFunc(f.members[clusterID], func(s string) bool { return s == instanceID })
	f.leaves = append(f.leaves, clusterID+"/"+instanceID)
	return nil
}

func (f *fakeGpu) membersOf(cluster string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.members[cluster])
}

// fakeFS records filesystem attachments.
type fakeFS struct {
	mu       sync.Mutex
	known    map[string]bool
	attached map[string][]string // fs id → instance ids
	attaches []string            // "fs/instance"
	detaches []string
}

func newFakeFS(ids ...string) *fakeFS {
	f := &fakeFS{known: map[string]bool{}, attached: map[string][]string{}}
	for _, id := range ids {
		f.known[id] = true
	}
	return f
}

func (f *fakeFS) Attach(_ context.Context, fsID, instanceID, _ string, mode computepb.AttachedFilesystemSpec_AttachMode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.known[fsID] {
		return errs.NotFound(fsID)
	}
	if mode != computepb.AttachedFilesystemSpec_READ_WRITE {
		return errs.BadRequest(errs.Violation{Field: "spec.filesystems.attach_mode", Message: "only READ_WRITE is supported"})
	}
	if !slices.Contains(f.attached[fsID], instanceID) {
		f.attached[fsID] = append(f.attached[fsID], instanceID)
	}
	f.attaches = append(f.attaches, fsID+"/"+instanceID)
	return nil
}

func (f *fakeFS) Detach(_ context.Context, fsID, instanceID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attached[fsID] = slices.DeleteFunc(f.attached[fsID], func(s string) bool { return s == instanceID })
	f.detaches = append(f.detaches, fsID+"/"+instanceID)
	return nil
}

func (f *fakeFS) attachedTo(fsID string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.attached[fsID])
}

// fakePerms denies the listed resource IDs.
type fakePerms struct {
	mu   sync.Mutex
	deny map[string]bool
}

func (f *fakePerms) Require(_ context.Context, _, resourceID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deny[resourceID] {
		return errs.PermissionDenied(resourceID)
	}
	return nil
}

// fakeParents maps an ID to its parent project.
type fakeParents map[string]string

func (f fakeParents) Parent(_ context.Context, id string) (string, error) {
	p, ok := f[id]
	if !ok {
		return "", errs.NotFound(id)
	}
	return p, nil
}

const (
	testSA      = "serviceaccount-e00mock0000000000sa1"
	otherSA     = "serviceaccount-e00mock00000000other"
	testNetwork = "network-1"
)

// instFixture wires an InstanceService with the real PlatformService
// (default catalog, eu-north1) and a real DiskService over the same fake
// disk repository the instance transactions write to.
type instFixture struct {
	ctx   context.Context //nolint:containedctx // test fixture: the context is the running test's t.Context()
	svc   *InstanceService
	ds    *DiskService
	repo  *fakeInstanceRepo
	disks *fakeDisks
	net   *fakeNet
	gpu   *fakeGpu
	fs    *fakeFS
	perms *fakePerms
	ops   *deferOps
	idx   *fakeOpIndex
	clk   *clock.Manual
}

func newInstFixture(t *testing.T) *instFixture {
	t.Helper()
	projects := fakeParents{
		testProject: "tenant-e00mock0000000000default", otherProject: "tenant-e00mock0000000000default",
		testSA: testProject, otherSA: otherProject,
	}
	catalog, err := DefaultPlatformCatalog()
	if err != nil {
		t.Fatal(err)
	}
	platforms, err := NewPlatformService(PlatformDeps{Catalog: catalog, Regions: testRegions, Projects: projects})
	if err != nil {
		t.Fatal(err)
	}
	f := &instFixture{
		ctx:   t.Context(),
		disks: &fakeDisks{}, net: newFakeNet(), gpu: newFakeGpu("computegpucluster-e00c1"), fs: newFakeFS("computefilesystem-e00f1", "computefilesystem-e00f2"),
		perms: &fakePerms{deny: map[string]bool{}}, ops: &deferOps{}, idx: &fakeOpIndex{},
		clk: clock.NewManual(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
	}
	f.repo = &fakeInstanceRepo{disks: &f.disks.repo}
	ids := &fakeIDs{}
	f.ds, err = NewDiskService(DiskDeps{
		Disks: f.disks, Snapshots: &fakeSnapshots{}, Images: &fakeImageLookup{}, OpIndex: &fakeOpIndex{},
		Projects: projects, Ops: f.ops, IDs: ids, Clock: f.clk,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.svc, err = NewInstanceService(InstanceDeps{
		Instances: f.repo, OpIndex: f.idx, Projects: projects, Platforms: platforms, Network: f.net, Disks: f.ds,
		GpuClusters: f.gpu, Filesystems: f.fs, Perms: f.perms, Ops: f.ops, IDs: ids, Clock: f.clk,
	})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// noNetwork is a Networking with no vpc module.
type noNetwork struct{}

func (noNetwork) NetworkInterfaces() (peer.NetworkInterfaces, bool) { return nil, false }
