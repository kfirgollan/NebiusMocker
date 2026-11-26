package domain_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/nebius/gosdk/proto/fieldmask/mask"
	capacitypb "github.com/nebius/gosdk/proto/nebius/capacity/v1"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/nid"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	"github.com/kfirgollan/NebiusMocker/services/capacity/domain"
)

const (
	tenantID       = "tenant-e00mock0000000000default"
	otherTenantID  = "tenant-e00other"
	projectID      = "project-e00mock000000000default"
	otherProjectID = "project-e00other"
	foreignProject = "project-e00foreign"
	routingCode    = "e00"
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// day returns the instant n days after the catalog anchor (00:00 UTC).
func day(n int) time.Time { return t0.Add(time.Duration(n) * 24 * time.Hour) }

// dayEnd returns 23:59 UTC of the n-th day, where catalog intervals end.
func dayEnd(n int) time.Time { return day(n).Add(23*time.Hour + 59*time.Minute) }

func u64(n uint64) *uint64 { return &n }

// testCatalogYAML has, in the default tenant: a (two overlapping intervals),
// b (same platform as a, other fabric), c, future (starts in 5 days) and gap
// (continuous and separate runs); one group of another tenant and one in a
// region that is not emulated.
const testCatalogYAML = `
block_groups:
  - key: a
    region: eu-north1
    platform: gpu-h100-sxm
    fabric: fabric-2
    intervals:
      - {quantity: 16, start_day: -30, end_day: 334}
      - {quantity: 8, start_day: 14, end_day: 44}
  - key: b
    region: eu-north1
    platform: gpu-h100-sxm
    fabric: fabric-3
    intervals:
      - {quantity: 8, start_day: 0, end_day: 10}
  - key: c
    region: eu-north1
    platform: gpu-h200-sxm
    fabric: fabric-7
    intervals:
      - {quantity: 32, start_day: -7, end_day: 180}
  - key: future
    region: eu-north1
    platform: gpu-b200-sxm
    fabric: fabric-9
    intervals:
      - {quantity: 10, start_day: 5, end_day: 9}
  - key: gap
    region: eu-north1
    platform: gpu-l40s
    fabric: fabric-1
    intervals:
      - {quantity: 4, start_day: 0, end_day: 1}
      - {quantity: 4, start_day: 2, end_day: 3}
      - {quantity: 4, start_day: 5, end_day: 6}
  - key: foreign
    tenant: tenant-e00other
    region: eu-north1
    platform: gpu-h100-sxm
    fabric: fabric-2
    intervals:
      - {quantity: 4, start_day: 0, end_day: 10}
  - key: elsewhere
    region: us-central1
    platform: gpu-h100-sxm
    fabric: fabric-2
    intervals:
      - {quantity: 4, start_day: 0, end_day: 10}
presets:
  - platform: gpu-h100-sxm
    gpu_memory_gigabytes: 80
    presets:
      - {name: 1gpu, vcpu_count: 16, memory_gibibytes: 200, gpu_count: 1}
      - {name: 8gpu, vcpu_count: 128, memory_gibibytes: 1600, gpu_count: 8}
  - platform: gpu-h200-sxm
    gpu_memory_gigabytes: 141
    presets:
      - {name: 8gpu, vcpu_count: 128, memory_gibibytes: 1600, gpu_count: 8}
`

// fakeAllowances is a slice-backed domain.AllowanceRepository that keeps one
// allowance per (project, block group) like the real one.
type fakeAllowances struct {
	items []*capacitypb.CapacityAllowance
}

func (f *fakeAllowances) index(id string) int {
	return slices.IndexFunc(f.items, func(a *capacitypb.CapacityAllowance) bool { return a.GetMetadata().GetId() == id })
}

func (f *fakeAllowances) pair(a *capacitypb.CapacityAllowance) *capacitypb.CapacityAllowance {
	for _, o := range f.items {
		if o.GetMetadata().GetParentId() == a.GetMetadata().GetParentId() &&
			o.GetSpec().GetCapacityBlockGroupId() == a.GetSpec().GetCapacityBlockGroupId() &&
			o.GetMetadata().GetId() != a.GetMetadata().GetId() {
			return o
		}
	}
	return nil
}

func (f *fakeAllowances) Get(_ context.Context, id string) (*capacitypb.CapacityAllowance, error) {
	i := f.index(id)
	if i < 0 {
		return nil, errs.NotFound(id)
	}
	return proto.CloneOf(f.items[i]), nil
}

func (f *fakeAllowances) filter(keep func(*capacitypb.CapacityAllowance) bool) []*capacitypb.CapacityAllowance {
	out := []*capacitypb.CapacityAllowance{}
	for _, a := range f.items {
		if keep(a) {
			out = append(out, proto.CloneOf(a))
		}
	}
	return out
}

func (f *fakeAllowances) ByProject(_ context.Context, projectID string) ([]*capacitypb.CapacityAllowance, error) {
	return f.filter(func(a *capacitypb.CapacityAllowance) bool { return a.GetMetadata().GetParentId() == projectID }), nil
}

func (f *fakeAllowances) ByBlockGroup(_ context.Context, groupID string) ([]*capacitypb.CapacityAllowance, error) {
	return f.filter(func(a *capacitypb.CapacityAllowance) bool { return a.GetSpec().GetCapacityBlockGroupId() == groupID }), nil
}

func (f *fakeAllowances) Create(_ context.Context, a *capacitypb.CapacityAllowance) error {
	if o := f.pair(a); o != nil {
		return errs.AlreadyExists(o.GetMetadata().GetId(), "allowance exists")
	}
	f.items = append(f.items, proto.CloneOf(a))
	return nil
}

func (f *fakeAllowances) Update(_ context.Context, a *capacitypb.CapacityAllowance) error {
	i := f.index(a.GetMetadata().GetId())
	if i < 0 {
		return errs.NotFound(a.GetMetadata().GetId())
	}
	if o := f.pair(a); o != nil {
		return errs.AlreadyExists(o.GetMetadata().GetId(), "allowance exists")
	}
	f.items[i] = proto.CloneOf(a)
	return nil
}

func (f *fakeAllowances) Delete(_ context.Context, id string) error {
	i := f.index(id)
	if i < 0 {
		return errs.NotFound(id)
	}
	f.items = slices.Delete(f.items, i, i+1)
	return nil
}

// fakeUsage is a slice-backed domain.UsageRepository.
type fakeUsage struct{ items []domain.Usage }

func (f *fakeUsage) Put(_ context.Context, u domain.Usage) error {
	if i := slices.IndexFunc(f.items, func(o domain.Usage) bool { return o.ConsumerID == u.ConsumerID }); i >= 0 {
		f.items[i] = u
		return nil
	}
	f.items = append(f.items, u)
	return nil
}

func (f *fakeUsage) Delete(_ context.Context, consumerID string) error {
	f.items = slices.DeleteFunc(f.items, func(u domain.Usage) bool { return u.ConsumerID == consumerID })
	return nil
}

func (f *fakeUsage) ByBlockGroup(_ context.Context, groupID string) ([]domain.Usage, error) {
	var out []domain.Usage
	for _, u := range f.items {
		if u.BlockGroupID == groupID {
			out = append(out, u)
		}
	}
	return out, nil
}

// fakeHierarchy maps a resource to its parent; unknown resources are
// errs.NotFound. The seeded project is deliberately absent.
type fakeHierarchy map[string]string

func (f fakeHierarchy) Parent(_ context.Context, id string) (string, error) {
	p, ok := f[id]
	if !ok {
		return "", errs.NotFound(id)
	}
	return p, nil
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

// env is a Service over fakes, built from testCatalogYAML.
type env struct {
	svc        *domain.Service
	allowances *fakeAllowances
	usage      *fakeUsage
	clk        *clock.Manual
}

// newEnv returns an env whose catalog is anchored at t0 and whose clock
// starts at now.
func newEnv(t *testing.T, now time.Time) *env {
	t.Helper()
	cat, err := domain.ParseCatalog([]byte(testCatalogYAML))
	if err != nil {
		t.Fatalf("ParseCatalog: %v", err)
	}
	inv, err := cat.Build(t0, tenantID, map[string]string{"eu-north1": routingCode})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	ids, err := nid.NewGenerator(nid.Sequential, routingCode, 0)
	if err != nil {
		t.Fatalf("nid.NewGenerator: %v", err)
	}
	e := &env{allowances: &fakeAllowances{}, usage: &fakeUsage{}, clk: clock.NewManual(now)}
	e.svc, err = domain.New(domain.Deps{
		Inventory: inv, Allowances: e.allowances, Usage: e.usage,
		Hierarchy: fakeHierarchy{
			otherProjectID: tenantID, foreignProject: otherTenantID, otherTenantID: "",
		},
		Ops: inlineOps{}, IDs: ids, Clock: e.clk,
		Config: domain.Config{TenantID: tenantID, ProjectID: projectID},
	})
	if err != nil {
		t.Fatalf("domain.New: %v", err)
	}
	return e
}

// gid returns the ID of the catalog block group with the key in the default
// tenant.
func gid(t *testing.T, key string) string {
	t.Helper()
	return gidIn(t, tenantID, key)
}

func gidIn(t *testing.T, tenant, key string) string {
	t.Helper()
	id, err := nid.Deterministic(domain.KindBlockGroup, routingCode, tenant+"/"+key)
	if err != nil {
		t.Fatalf("nid.Deterministic: %v", err)
	}
	return id
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

func wantKind(t *testing.T, what string, err error, want errs.Kind) {
	t.Helper()
	if got := errs.KindOf(err); got != want {
		t.Fatalf("%s: kind = %v, want %v (err: %v)", what, got, want, err)
	}
}

func (e *env) createAllowance(ctx context.Context, project, group string, limit *uint64) (*operations.Operation, error) {
	return e.svc.CreateAllowance(ctx, domain.CreateAllowanceCmd{
		Metadata: &commonpb.ResourceMetadata{ParentId: project},
		Spec:     &capacitypb.CapacityAllowanceSpec{CapacityBlockGroupId: group, Limit: limit},
	})
}

// newAllowance creates an allowance and returns its ID.
func (e *env) newAllowance(t *testing.T, project, group string, limit *uint64) string {
	t.Helper()
	return done(t)(e.createAllowance(t.Context(), project, group, limit))
}

func (e *env) updateAllowance(ctx context.Context, md *commonpb.ResourceMetadata, spec *capacitypb.CapacityAllowanceSpec, resetMask string) (*operations.Operation, error) {
	cmd := domain.UpdateAllowanceCmd{Metadata: md, Spec: spec}
	if resetMask != "" {
		m, err := mask.Parse(resetMask)
		if err != nil {
			return nil, err
		}
		cmd.ResetMask = m
	}
	return e.svc.UpdateAllowance(ctx, cmd)
}

// allowance returns the allowance, failing the test on error.
func (e *env) allowance(t *testing.T, id string) *capacitypb.CapacityAllowance {
	t.Helper()
	a, err := e.svc.GetAllowance(t.Context(), id)
	if err != nil {
		t.Fatalf("GetAllowance(%s): %v", id, err)
	}
	return a
}

// blockGroup returns the block group, failing the test on error.
func (e *env) blockGroup(t *testing.T, id string) *capacitypb.CapacityBlockGroup {
	t.Helper()
	g, err := e.svc.GetBlockGroup(t.Context(), id)
	if err != nil {
		t.Fatalf("GetBlockGroup(%s): %v", id, err)
	}
	return g
}
