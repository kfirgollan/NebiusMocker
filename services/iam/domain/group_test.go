package domain_test

import (
	"context"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/nebius/gosdk/proto/fieldmask/mask"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	"github.com/kfirgollan/NebiusMocker/services/iam/domain"
)

const tenantID = "tenant-e00test0000000000001"

// fakeGroups is a map-backed domain.GroupRepository.
type fakeGroups struct{ byID map[string]*iampb.Group }

func (f *fakeGroups) Get(_ context.Context, id string) (*iampb.Group, error) {
	g, ok := f.byID[id]
	if !ok {
		return nil, errs.NotFound(id)
	}
	return proto.CloneOf(g), nil
}

func (f *fakeGroups) GetByName(_ context.Context, parentID, name string) (*iampb.Group, error) {
	for _, g := range f.byID {
		if g.GetMetadata().GetParentId() == parentID && g.GetMetadata().GetName() == name {
			return proto.CloneOf(g), nil
		}
	}
	return nil, errs.NotFound(name)
}

func (f *fakeGroups) List(context.Context, string, int, string) (domain.GroupPage, error) {
	return domain.GroupPage{}, nil
}

func (f *fakeGroups) Create(ctx context.Context, g *iampb.Group) error {
	if existing, err := f.GetByName(ctx, g.GetMetadata().GetParentId(), g.GetMetadata().GetName()); err == nil {
		return errs.AlreadyExists(existing.GetMetadata().GetId(), "name taken")
	}
	f.byID[g.GetMetadata().GetId()] = proto.CloneOf(g)
	return nil
}

func (f *fakeGroups) Update(_ context.Context, g *iampb.Group) error {
	f.byID[g.GetMetadata().GetId()] = proto.CloneOf(g)
	return nil
}

func (f *fakeGroups) Delete(_ context.Context, id string) error {
	delete(f.byID, id)
	return nil
}

type fakeTenants struct{}

func (fakeTenants) Get(_ context.Context, id string) (*iampb.Container, error) {
	if id != tenantID {
		return nil, errs.NotFound(id)
	}
	return &iampb.Container{Metadata: &commonpb.ResourceMetadata{Id: id}}, nil
}

func (fakeTenants) Ensure(context.Context, *iampb.Container) error { return nil }

// inlineOps runs Prepare and Work immediately, like a zero-delay engine.
type inlineOps struct{}

func (inlineOps) Submit(ctx context.Context, s operations.Spec) (*operations.Operation, error) {
	if s.Prepare != nil {
		if err := s.Prepare(ctx); err != nil {
			return nil, err
		}
	}
	op := &operations.Operation{ID: "operation-1", ResourceID: s.ResourceID}
	if s.Work != nil {
		op.Err = s.Work(ctx)
	}
	return op, nil
}

type seqIDs struct{ n int }

func (s *seqIDs) New(typ string) (string, error) {
	s.n++
	return typ + "-e00test" + string(rune('0'+s.n)), nil
}

func newService(t *testing.T) (*domain.GroupService, *fakeGroups, *clock.Manual) {
	t.Helper()
	groups := &fakeGroups{byID: map[string]*iampb.Group{}}
	clk := clock.NewManual(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	svc, err := domain.NewGroupService(domain.GroupDeps{
		Groups: groups, Tenants: fakeTenants{}, Memberships: newFakeMemberships(), Permits: newFakePermits(),
		Ops: inlineOps{}, IDs: &seqIDs{}, Clock: clk,
	})
	if err != nil {
		t.Fatal(err)
	}
	return svc, groups, clk
}

func create(t *testing.T, svc *domain.GroupService, name string, labels map[string]string) *iampb.Group {
	t.Helper()
	op, err := svc.Create(t.Context(), domain.CreateGroupCmd{
		Metadata: &commonpb.ResourceMetadata{ParentId: tenantID, Name: name, Labels: labels},
	})
	if err != nil {
		t.Fatalf("Create(%q): %v", name, err)
	}
	g, err := svc.Get(t.Context(), op.ResourceID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	return g
}

func TestCreateValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		md   *commonpb.ResourceMetadata
		want errs.Kind
	}{
		{"ok", &commonpb.ResourceMetadata{ParentId: tenantID, Name: "a"}, errs.KindUnknown},
		{"client id", &commonpb.ResourceMetadata{Id: "group-x", ParentId: tenantID}, errs.KindBadRequest},
		{"no parent", &commonpb.ResourceMetadata{Name: "a"}, errs.KindBadRequest},
		{"unknown parent", &commonpb.ResourceMetadata{ParentId: "tenant-e00other", Name: "a"}, errs.KindNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			svc, _, _ := newService(t)
			_, err := svc.Create(t.Context(), domain.CreateGroupCmd{Metadata: tt.md})
			if got := errs.KindOf(err); got != tt.want {
				t.Fatalf("Create kind = %v, want %v (err: %v)", got, tt.want, err)
			}
		})
	}
}

func TestCreateSetsServerFields(t *testing.T) {
	t.Parallel()
	svc, _, clk := newService(t)
	g := create(t, svc, "fresh", nil)
	md := g.GetMetadata()
	if md.GetId() == "" || md.GetResourceVersion() != 1 || !md.GetCreatedAt().AsTime().Equal(clk.Now()) {
		t.Errorf("metadata = %v, want id, version 1, created_at = clock", md)
	}
	if g.GetStatus().GetState() != iampb.GroupStatus_ACTIVE {
		t.Errorf("state = %v, want ACTIVE", g.GetStatus().GetState())
	}
}

func TestUpdateResetMaskAndVersion(t *testing.T) {
	t.Parallel()
	svc, _, clk := newService(t)
	g := create(t, svc, "a", map[string]string{"k": "v"})
	id := g.GetMetadata().GetId()
	clk.Advance(time.Minute)

	// Name changes; labels are absent but NOT in the mask, so they are kept.
	if _, err := svc.Update(t.Context(), domain.UpdateGroupCmd{
		Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: tenantID, Name: "b"},
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	g, _ = svc.Get(t.Context(), id)
	if g.GetMetadata().GetName() != "b" || g.GetMetadata().GetLabels()["k"] != "v" || g.GetMetadata().GetResourceVersion() != 2 {
		t.Fatalf("after rename: %v, want name b, labels kept, version 2", g.GetMetadata())
	}
	if !g.GetMetadata().GetUpdatedAt().AsTime().Equal(clk.Now()) {
		t.Errorf("updated_at = %v, want %v", g.GetMetadata().GetUpdatedAt().AsTime(), clk.Now())
	}

	// Labels in the reset mask and absent from the request are cleared.
	if _, err := svc.Update(t.Context(), domain.UpdateGroupCmd{
		Metadata:  &commonpb.ResourceMetadata{Id: id, ParentId: tenantID, Name: "b"},
		ResetMask: mask.ParseMust("metadata.labels"),
	}); err != nil {
		t.Fatalf("Update with mask: %v", err)
	}
	g, _ = svc.Get(t.Context(), id)
	if len(g.GetMetadata().GetLabels()) != 0 || g.GetMetadata().GetResourceVersion() != 3 {
		t.Fatalf("after reset: %v, want labels cleared, version 3", g.GetMetadata())
	}

	// A no-op update keeps the version.
	if _, err := svc.Update(t.Context(), domain.UpdateGroupCmd{
		Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: tenantID, Name: "b"},
	}); err != nil {
		t.Fatalf("no-op Update: %v", err)
	}
	g, _ = svc.Get(t.Context(), id)
	if g.GetMetadata().GetResourceVersion() != 3 {
		t.Errorf("no-op update bumped version to %d", g.GetMetadata().GetResourceVersion())
	}
}

func TestUpdateRejections(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		md   func(id string) *commonpb.ResourceMetadata
		want errs.Kind
	}{
		{"parent change", func(id string) *commonpb.ResourceMetadata {
			return &commonpb.ResourceMetadata{Id: id, ParentId: "tenant-e00other"}
		}, errs.KindBadRequest},
		{"stale version", func(id string) *commonpb.ResourceMetadata {
			return &commonpb.ResourceMetadata{Id: id, ParentId: tenantID, ResourceVersion: 7}
		}, errs.KindConflict},
		{"missing", func(string) *commonpb.ResourceMetadata {
			return &commonpb.ResourceMetadata{Id: "group-e00nope", ParentId: tenantID}
		}, errs.KindNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			svc, _, _ := newService(t)
			g := create(t, svc, "a", nil)
			_, err := svc.Update(t.Context(), domain.UpdateGroupCmd{Metadata: tt.md(g.GetMetadata().GetId())})
			if got := errs.KindOf(err); got != tt.want {
				t.Fatalf("Update kind = %v, want %v (err: %v)", got, tt.want, err)
			}
		})
	}
}

func TestDelete(t *testing.T) {
	t.Parallel()
	svc, groups, _ := newService(t)
	g := create(t, svc, "doomed", nil)
	if _, err := svc.Delete(t.Context(), g.GetMetadata().GetId(), nil); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(groups.byID) != 0 {
		t.Fatalf("group still stored after delete")
	}
	_, err := svc.Delete(t.Context(), g.GetMetadata().GetId(), nil)
	if !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("second Delete err = %v, want NotFound", err)
	}
}
