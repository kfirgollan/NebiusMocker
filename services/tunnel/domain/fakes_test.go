package domain_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	tunnelpb "github.com/nebius/gosdk/proto/nebius/tunnel/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/nid"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	"github.com/kfirgollan/NebiusMocker/services/tunnel/domain"
)

const (
	projectID      = "project-e00mock000000000default"
	otherProjectID = "project-e00mock000000000other"
	missingProject = "project-e00mock000000000missing"
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// fakeTunnels is a slice-backed domain.TunnelRepository (creation order).
type fakeTunnels struct{ items []*tunnelpb.Tunnel }

func (f *fakeTunnels) index(id string) int {
	return slices.IndexFunc(f.items, func(t *tunnelpb.Tunnel) bool { return t.GetMetadata().GetId() == id })
}

func (f *fakeTunnels) Get(_ context.Context, id string) (*tunnelpb.Tunnel, error) {
	i := f.index(id)
	if i < 0 {
		return nil, errs.NotFound(id)
	}
	return proto.CloneOf(f.items[i]), nil
}

func (f *fakeTunnels) List(_ context.Context, parentID string) ([]*tunnelpb.Tunnel, error) {
	out := []*tunnelpb.Tunnel{}
	for _, t := range f.items {
		if t.GetMetadata().GetParentId() == parentID {
			out = append(out, proto.CloneOf(t))
		}
	}
	return out, nil
}

func (f *fakeTunnels) Create(_ context.Context, t *tunnelpb.Tunnel) error {
	if f.index(t.GetMetadata().GetId()) >= 0 {
		return errs.AlreadyExists(t.GetMetadata().GetId(), "exists")
	}
	f.items = append(f.items, proto.CloneOf(t))
	return nil
}

func (f *fakeTunnels) Update(_ context.Context, t *tunnelpb.Tunnel) error {
	i := f.index(t.GetMetadata().GetId())
	if i < 0 {
		return errs.NotFound(t.GetMetadata().GetId())
	}
	f.items[i] = proto.CloneOf(t)
	return nil
}

func (f *fakeTunnels) Delete(_ context.Context, id string) error {
	i := f.index(id)
	if i < 0 {
		return errs.NotFound(id)
	}
	f.items = slices.Delete(f.items, i, i+1)
	return nil
}

// fakeHierarchy knows one project besides the seeded one.
type fakeHierarchy struct{}

func (fakeHierarchy) Parent(_ context.Context, id string) (string, error) {
	if id != otherProjectID {
		return "", errs.NotFound(id)
	}
	return "tenant-e00mock000000000default", nil
}

// inlineOps runs Prepare and Work at Submit.
type inlineOps struct{}

func (inlineOps) Submit(ctx context.Context, s operations.Spec) (*operations.Operation, error) {
	if s.Prepare != nil {
		if err := s.Prepare(ctx); err != nil {
			return nil, err
		}
	}
	op := &operations.Operation{ID: "op-" + s.ResourceID, ResourceID: s.ResourceID, Request: s.Request, CreatedAt: t0}
	if s.Work != nil {
		op.Err = s.Work(ctx)
	}
	return op, nil
}

type env struct {
	svc     *domain.Service
	tunnels *fakeTunnels
	clk     *clock.Manual
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ids, err := nid.NewGenerator(nid.Sequential, "e00", 0)
	if err != nil {
		t.Fatalf("nid.NewGenerator: %v", err)
	}
	e := &env{tunnels: &fakeTunnels{}, clk: clock.NewManual(t0)}
	e.svc, err = domain.New(domain.Deps{
		Tunnels: e.tunnels, Hierarchy: fakeHierarchy{}, Ops: inlineOps{}, IDs: ids, Clock: e.clk,
		Config: domain.Config{ProjectID: projectID},
	})
	if err != nil {
		t.Fatalf("domain.New: %v", err)
	}
	return e
}

// newTunnel creates a tunnel in the seeded project and returns it.
func (e *env) newTunnel(t *testing.T, name, title, description string) *tunnelpb.Tunnel {
	t.Helper()
	op, err := e.svc.Create(t.Context(), domain.CreateCmd{
		Metadata: &commonpb.ResourceMetadata{ParentId: projectID, Name: name, Labels: map[string]string{"env": "dev"}},
		Spec:     &tunnelpb.TunnelSpec{Title: title, Description: description},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := e.svc.Get(t.Context(), op.ResourceID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	return got
}

func wantKind(t *testing.T, what string, err error, want errs.Kind) {
	t.Helper()
	if got := errs.KindOf(err); got != want {
		t.Fatalf("%s: kind = %v, want %v (err: %v)", what, got, want, err)
	}
}
