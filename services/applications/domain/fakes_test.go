package domain_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	applicationsv1alpha1pb "github.com/nebius/gosdk/proto/nebius/applications/v1alpha1"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/nid"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	"github.com/kfirgollan/NebiusMocker/services/applications/domain"
)

const (
	projectID      = "project-e00mock000000000default"
	otherProjectID = "project-e00mock000000000other"
	runningID      = "mk8scluster-e00test000000000001"
	creatingID     = "mk8scluster-e00test000000000002"
	foreignID      = "mk8scluster-e00test000000000003"
	missingID      = "mk8scluster-e00test000000000009"
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// fakeReleases is a slice-backed domain.ReleaseRepository (creation order).
type fakeReleases struct {
	items []*applicationsv1alpha1pb.K8SRelease
}

func (f *fakeReleases) index(id string) int {
	return slices.IndexFunc(f.items, func(r *applicationsv1alpha1pb.K8SRelease) bool { return r.GetMetadata().GetId() == id })
}

func (f *fakeReleases) Get(_ context.Context, id string) (*applicationsv1alpha1pb.K8SRelease, error) {
	i := f.index(id)
	if i < 0 {
		return nil, errs.NotFound(id)
	}
	return proto.CloneOf(f.items[i]), nil
}

func (f *fakeReleases) List(_ context.Context, parentID string) ([]*applicationsv1alpha1pb.K8SRelease, error) {
	out := []*applicationsv1alpha1pb.K8SRelease{}
	for _, r := range f.items {
		if r.GetMetadata().GetParentId() == parentID {
			out = append(out, proto.CloneOf(r))
		}
	}
	return out, nil
}

func (f *fakeReleases) All(context.Context) ([]*applicationsv1alpha1pb.K8SRelease, error) {
	out := make([]*applicationsv1alpha1pb.K8SRelease, 0, len(f.items))
	for _, r := range f.items {
		out = append(out, proto.CloneOf(r))
	}
	return out, nil
}

func (f *fakeReleases) Create(_ context.Context, r *applicationsv1alpha1pb.K8SRelease) error {
	if f.index(r.GetMetadata().GetId()) >= 0 {
		return errs.AlreadyExists(r.GetMetadata().GetId(), "exists")
	}
	f.items = append(f.items, proto.CloneOf(r))
	return nil
}

func (f *fakeReleases) Update(_ context.Context, r *applicationsv1alpha1pb.K8SRelease) error {
	i := f.index(r.GetMetadata().GetId())
	if i < 0 {
		return errs.NotFound(r.GetMetadata().GetId())
	}
	f.items[i] = proto.CloneOf(r)
	return nil
}

func (f *fakeReleases) Delete(_ context.Context, id string) error {
	i := f.index(id)
	if i < 0 {
		return errs.NotFound(id)
	}
	f.items = slices.Delete(f.items, i, i+1)
	return nil
}

// fakeClusters resolves a fixed set of clusters.
type fakeClusters map[string]domain.Cluster

func (f fakeClusters) Cluster(_ context.Context, id string) (domain.Cluster, error) {
	c, ok := f[id]
	if !ok {
		return domain.Cluster{}, errs.NotFound(id)
	}
	return c, nil
}

type fakeHierarchy struct{}

func (fakeHierarchy) Parent(_ context.Context, id string) (string, error) {
	if id != otherProjectID {
		return "", errs.NotFound(id)
	}
	return "tenant-e00mock000000000default", nil
}

// stagedOps runs Prepare at Submit and keeps Work until finish, so tests can
// observe the state between the two.
type stagedOps struct{ pending []func() }

func (o *stagedOps) Submit(ctx context.Context, s operations.Spec) (*operations.Operation, error) {
	if s.Prepare != nil {
		if err := s.Prepare(ctx); err != nil {
			return nil, err
		}
	}
	op := &operations.Operation{ID: "op-" + s.ResourceID, ResourceID: s.ResourceID, Request: s.Request, CreatedAt: t0}
	if s.Work != nil {
		o.pending = append(o.pending, func() { op.Err = s.Work(ctx) })
	}
	return op, nil
}

// finish runs every deferred Work.
func (o *stagedOps) finish() {
	for _, w := range o.pending {
		w()
	}
	o.pending = nil
}

type env struct {
	svc      *domain.Service
	releases *fakeReleases
	ops      *stagedOps
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ids, err := nid.NewGenerator(nid.Sequential, "e00", 0)
	if err != nil {
		t.Fatalf("nid.NewGenerator: %v", err)
	}
	e := &env{releases: &fakeReleases{}, ops: &stagedOps{}}
	e.svc, err = domain.New(domain.Deps{
		Releases: e.releases,
		Clusters: fakeClusters{
			runningID:  {ID: runningID, ProjectID: projectID, Running: true},
			creatingID: {ID: creatingID, ProjectID: projectID},
			foreignID:  {ID: foreignID, ProjectID: otherProjectID, Running: true},
		},
		Hierarchy: fakeHierarchy{}, Ops: e.ops, IDs: ids, Clock: clock.NewManual(t0),
		Config: domain.Config{ProjectID: projectID, Products: []string{"nebius/mlflow", "nebius/kuberay"}},
	})
	if err != nil {
		t.Fatalf("domain.New: %v", err)
	}
	return e
}

func spec() *applicationsv1alpha1pb.K8SReleaseSpec {
	return &applicationsv1alpha1pb.K8SReleaseSpec{
		ClusterId: runningID, ProductSlug: "nebius/mlflow", Namespace: "mlflow", ApplicationName: "tracking",
		Values: "replicas: 2\n", Set: map[string]string{"image.tag": "v1"},
	}
}

func (e *env) create(ctx context.Context, sp *applicationsv1alpha1pb.K8SReleaseSpec) (*operations.Operation, error) {
	return e.svc.Create(ctx, domain.CreateCmd{Metadata: &commonpb.ResourceMetadata{ParentId: projectID}, Spec: sp})
}

// newRelease creates a release and returns its ID.
func (e *env) newRelease(t *testing.T, sp *applicationsv1alpha1pb.K8SReleaseSpec) string {
	t.Helper()
	op, err := e.create(t.Context(), sp)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return op.ResourceID
}

func (e *env) state(t *testing.T, id string) applicationsv1alpha1pb.K8SReleaseStatus_State {
	t.Helper()
	r, err := e.svc.Get(t.Context(), id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	return r.GetStatus().GetState()
}

func wantKind(t *testing.T, what string, err error, want errs.Kind) {
	t.Helper()
	if got := errs.KindOf(err); got != want {
		t.Fatalf("%s: kind = %v, want %v (err: %v)", what, got, want, err)
	}
}
