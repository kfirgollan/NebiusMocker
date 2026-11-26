package domain_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	dnspb "github.com/nebius/gosdk/proto/nebius/dns/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/nid"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	"github.com/kfirgollan/NebiusMocker/services/dns/domain"
)

const (
	projectID      = "project-e00mock000000000default"
	otherProjectID = "project-e00mock000000000other"
	networkID      = "vpcnetwork-e00test000000000001"
	network2ID     = "vpcnetwork-e00test000000000002"
	foreignNetwork = "vpcnetwork-e00test000000000009"
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// fakeZones is a slice-backed domain.ZoneRepository (creation order).
type fakeZones struct{ items []*dnspb.Zone }

func (f *fakeZones) index(id string) int {
	return slices.IndexFunc(f.items, func(z *dnspb.Zone) bool { return z.GetMetadata().GetId() == id })
}

func (f *fakeZones) named(parentID, name string) *dnspb.Zone {
	if name == "" {
		return nil
	}
	for _, z := range f.items {
		if md := z.GetMetadata(); md.GetParentId() == parentID && md.GetName() == name {
			return z
		}
	}
	return nil
}

func (f *fakeZones) Get(_ context.Context, id string) (*dnspb.Zone, error) {
	i := f.index(id)
	if i < 0 {
		return nil, errs.NotFound(id)
	}
	return proto.CloneOf(f.items[i]), nil
}

func (f *fakeZones) GetByName(_ context.Context, parentID, name string) (*dnspb.Zone, error) {
	z := f.named(parentID, name)
	if z == nil {
		return nil, errs.NotFound(name)
	}
	return proto.CloneOf(z), nil
}

func (f *fakeZones) List(_ context.Context, parentID string, _ int, _ string) (domain.Page[*dnspb.Zone], error) {
	out := []*dnspb.Zone{}
	for _, z := range f.items {
		if z.GetMetadata().GetParentId() == parentID {
			out = append(out, proto.CloneOf(z))
		}
	}
	return domain.Page[*dnspb.Zone]{Items: out}, nil
}

func (f *fakeZones) All(context.Context) ([]*dnspb.Zone, error) {
	out := make([]*dnspb.Zone, 0, len(f.items))
	for _, z := range f.items {
		out = append(out, proto.CloneOf(z))
	}
	return out, nil
}

func (f *fakeZones) Create(_ context.Context, z *dnspb.Zone) error {
	md := z.GetMetadata()
	if other := f.named(md.GetParentId(), md.GetName()); other != nil {
		return errs.AlreadyExists(other.GetMetadata().GetId(), "name taken")
	}
	f.items = append(f.items, proto.CloneOf(z))
	return nil
}

func (f *fakeZones) Update(_ context.Context, z *dnspb.Zone) error {
	md := z.GetMetadata()
	i := f.index(md.GetId())
	if i < 0 {
		return errs.NotFound(md.GetId())
	}
	if other := f.named(md.GetParentId(), md.GetName()); other != nil && other.GetMetadata().GetId() != md.GetId() {
		return errs.AlreadyExists(other.GetMetadata().GetId(), "name taken")
	}
	f.items[i] = proto.CloneOf(z)
	return nil
}

func (f *fakeZones) Delete(_ context.Context, id string) error {
	i := f.index(id)
	if i < 0 {
		return errs.NotFound(id)
	}
	f.items = slices.Delete(f.items, i, i+1)
	return nil
}

// fakeRecords is a slice-backed domain.RecordRepository (creation order).
type fakeRecords struct{ items []*dnspb.Record }

func (f *fakeRecords) index(id string) int {
	return slices.IndexFunc(f.items, func(r *dnspb.Record) bool { return r.GetMetadata().GetId() == id })
}

func (f *fakeRecords) named(zoneID, name string) *dnspb.Record {
	if name == "" {
		return nil
	}
	for _, r := range f.items {
		if md := r.GetMetadata(); md.GetParentId() == zoneID && md.GetName() == name {
			return r
		}
	}
	return nil
}

func (f *fakeRecords) Get(_ context.Context, id string) (*dnspb.Record, error) {
	i := f.index(id)
	if i < 0 {
		return nil, errs.NotFound(id)
	}
	return proto.CloneOf(f.items[i]), nil
}

func (f *fakeRecords) GetByName(_ context.Context, zoneID, name string) (*dnspb.Record, error) {
	r := f.named(zoneID, name)
	if r == nil {
		return nil, errs.NotFound(name)
	}
	return proto.CloneOf(r), nil
}

func (f *fakeRecords) All(_ context.Context, zoneID string) ([]*dnspb.Record, error) {
	out := []*dnspb.Record{}
	for _, r := range f.items {
		if r.GetMetadata().GetParentId() == zoneID {
			out = append(out, proto.CloneOf(r))
		}
	}
	return out, nil
}

func (f *fakeRecords) List(ctx context.Context, zoneID string, _ int, _ string) (domain.Page[*dnspb.Record], error) {
	items, err := f.All(ctx, zoneID)
	return domain.Page[*dnspb.Record]{Items: items}, err
}

func (f *fakeRecords) Create(_ context.Context, r *dnspb.Record) error {
	md := r.GetMetadata()
	if other := f.named(md.GetParentId(), md.GetName()); other != nil {
		return errs.AlreadyExists(other.GetMetadata().GetId(), "name taken")
	}
	f.items = append(f.items, proto.CloneOf(r))
	return nil
}

func (f *fakeRecords) Update(_ context.Context, r *dnspb.Record) error {
	md := r.GetMetadata()
	i := f.index(md.GetId())
	if i < 0 {
		return errs.NotFound(md.GetId())
	}
	if other := f.named(md.GetParentId(), md.GetName()); other != nil && other.GetMetadata().GetId() != md.GetId() {
		return errs.AlreadyExists(other.GetMetadata().GetId(), "name taken")
	}
	f.items[i] = proto.CloneOf(r)
	return nil
}

func (f *fakeRecords) Delete(_ context.Context, id string) error {
	i := f.index(id)
	if i < 0 {
		return errs.NotFound(id)
	}
	f.items = slices.Delete(f.items, i, i+1)
	return nil
}

// fakeNetworks knows two networks of the default project and one of another
// project, and records Bind/Unbind calls.
type fakeNetworks struct {
	projects map[string]string
	bound    map[string]bool // "network|zone"
	binds    int
	unbinds  int
}

func newFakeNetworks() *fakeNetworks {
	return &fakeNetworks{
		projects: map[string]string{networkID: projectID, network2ID: projectID, foreignNetwork: otherProjectID},
		bound:    map[string]bool{},
	}
}

func (f *fakeNetworks) Network(_ context.Context, id string) (domain.Network, error) {
	p, ok := f.projects[id]
	if !ok {
		return domain.Network{}, errs.NotFound(id)
	}
	return domain.Network{ID: id, ProjectID: p}, nil
}

func (f *fakeNetworks) Bind(_ context.Context, netID, zoneID string) error {
	if _, ok := f.projects[netID]; !ok {
		return errs.NotFound(netID)
	}
	f.binds++
	f.bound[netID+"|"+zoneID] = true
	return nil
}

func (f *fakeNetworks) Unbind(_ context.Context, netID, zoneID string) error {
	f.unbinds++
	delete(f.bound, netID+"|"+zoneID)
	return nil
}

// fakeHierarchy knows exactly one project besides the configured one.
type fakeHierarchy struct{}

func (fakeHierarchy) Parent(_ context.Context, id string) (string, error) {
	if id != otherProjectID {
		return "", errs.NotFound(id)
	}
	return "tenant-e00mock000000000default", nil
}

// inlineOps runs Prepare and Work immediately, like a zero-delay engine.
type inlineOps struct{}

func (inlineOps) Submit(ctx context.Context, s operations.Spec) (*operations.Operation, error) {
	if s.Prepare != nil {
		if err := s.Prepare(ctx); err != nil {
			return nil, err
		}
	}
	op := &operations.Operation{
		ID: "op-" + s.ResourceID, Description: s.Description, CreatedBy: s.CreatedBy,
		ResourceID: s.ResourceID, Request: s.Request, CreatedAt: t0, FinishedAt: t0,
	}
	if s.Work != nil {
		op.Err = s.Work(ctx)
	}
	return op, nil
}

// env is a Service wired to fakes.
type env struct {
	svc      *domain.Service
	zones    *fakeZones
	records  *fakeRecords
	networks *fakeNetworks
	clk      *clock.Manual
	gate     *fakeGate
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ids, err := nid.NewGenerator(nid.Sequential, "e00", 0)
	if err != nil {
		t.Fatalf("nid.NewGenerator: %v", err)
	}
	e := &env{zones: &fakeZones{}, records: &fakeRecords{}, networks: newFakeNetworks(), clk: clock.NewManual(t0), gate: &fakeGate{}}
	e.svc, err = domain.New(domain.Deps{
		Zones: e.zones, Records: e.records, Networks: e.networks, Hierarchy: fakeHierarchy{}, Quota: e.gate,
		Ops: inlineOps{}, IDs: ids, Clock: e.clk, Config: domain.Config{ProjectID: projectID},
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

func zoneSpec(domainName, network string) *dnspb.ZoneSpec {
	return &dnspb.ZoneSpec{
		DomainName: domainName,
		Scope:      &dnspb.ZoneSpec_Vpc{Vpc: &dnspb.VpcZoneScope{PrimaryNetworkId: network}},
	}
}

func (e *env) createZone(ctx context.Context, name, domainName, network string, neg int64) (*operations.Operation, error) {
	spec := zoneSpec(domainName, network)
	if neg != 0 {
		spec.SoaSpec = &dnspb.SoaSpec{NegativeTtl: neg}
	}
	return e.svc.CreateZone(ctx, domain.CreateZoneCmd{
		Metadata: &commonpb.ResourceMetadata{ParentId: projectID, Name: name}, Spec: spec,
	})
}

// newZone creates a zone in the default project and network.
func (e *env) newZone(t *testing.T, domainName string) string {
	t.Helper()
	return done(t)(e.createZone(t.Context(), "", domainName, networkID, 0))
}

func (e *env) createRecord(ctx context.Context, zoneID, name string, spec *dnspb.RecordSpec) (*operations.Operation, error) {
	return e.svc.CreateRecord(ctx, domain.CreateRecordCmd{
		Metadata: &commonpb.ResourceMetadata{ParentId: zoneID, Name: name}, Spec: spec,
	})
}

func (e *env) newRecord(t *testing.T, zoneID, rel string, typ dnspb.RecordSpec_RecordType, data string) string {
	t.Helper()
	return done(t)(e.createRecord(t.Context(), zoneID, "", &dnspb.RecordSpec{RelativeName: rel, Type: typ, Data: data}))
}

// recordsOf returns the zone's records, failing the test on error.
func (e *env) recordsOf(t *testing.T, zoneID string) []*dnspb.Record {
	t.Helper()
	rs, err := e.records.All(t.Context(), zoneID)
	if err != nil {
		t.Fatalf("records.All(%s): %v", zoneID, err)
	}
	return rs
}

func wantKind(t *testing.T, what string, err error, want errs.Kind) {
	t.Helper()
	if got := errs.KindOf(err); got != want {
		t.Fatalf("%s: kind = %v, want %v (err: %v)", what, got, want, err)
	}
}
