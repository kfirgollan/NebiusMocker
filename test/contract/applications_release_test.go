package contract

import (
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/nebius/gosdk/operations"
	applicationsv1alpha1pb "github.com/nebius/gosdk/proto/nebius/applications/v1alpha1"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	mk8spb "github.com/nebius/gosdk/proto/nebius/mk8s/v1"
	applicationssvc "github.com/nebius/gosdk/services/nebius/applications/v1alpha1"
	mk8ssvc "github.com/nebius/gosdk/services/nebius/mk8s/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore/memory"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/applications"
	"github.com/kfirgollan/NebiusMocker/services/compute"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/services/mk8s"
	"github.com/kfirgollan/NebiusMocker/services/vpc"
)

const appsMissingCluster = "mk8scluster-e00test000000000009"

type appsEnv struct {
	inst     *testkit.Instance
	releases applicationssvc.K8SReleaseService
	clusters mk8ssvc.ClusterService
	running  string // a RUNNING mk8s cluster in the seeded project
}

// startApplications runs the real iam, vpc, compute, mk8s and applications
// modules under enforce, with one RUNNING cluster created through the mk8s API.
func startApplications(t *testing.T) *appsEnv {
	t.Helper()
	inst := testkit.StartT(t,
		[]mocker.Module{iam.New(), vpc.New(), compute.New(), mk8s.New(), applications.New()},
		testkit.WithAuthz(authz.ModeEnforce), testkit.WithAuthnRequired(),
		testkit.WithObjStore(memory.New(clock.Real{})), testkit.WithOperationDelay(200*time.Millisecond),
	)
	sdk := inst.SDK(t)
	e := &appsEnv{inst: inst, releases: applicationssvc.NewK8SReleaseService(sdk), clusters: mk8ssvc.NewClusterService(sdk)}
	e.running = e.createCluster(t, "running")
	return e
}

// createCluster creates an mk8s cluster in the seeded project and waits
// until it is RUNNING.
func (e *appsEnv) createCluster(t *testing.T, name string) string {
	t.Helper()
	op, err := e.startCluster(t, name)
	return mustOp(t, op, err)
}

// startCluster starts creating a cluster without waiting: it is PROVISIONING
// until the operation completes.
func (e *appsEnv) startCluster(t *testing.T, name string) (operations.Operation, error) {
	t.Helper()
	return e.clusters.Create(t.Context(), &mk8spb.CreateClusterRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: e.inst.ProjectID, Name: name},
		Spec:     &mk8spb.ClusterSpec{ControlPlane: &mk8spb.ControlPlaneSpec{}},
	})
}

func appsSpec(cluster string) *applicationsv1alpha1pb.K8SReleaseSpec {
	return &applicationsv1alpha1pb.K8SReleaseSpec{
		ClusterId: cluster, ProductSlug: "nebius/mlflow", Namespace: "mlflow", ApplicationName: "tracking",
		Values: "replicas: 2\n", Set: map[string]string{"image.tag": "v1"},
	}
}

func (e *appsEnv) create(t *testing.T, sp *applicationsv1alpha1pb.K8SReleaseSpec) *applicationsv1alpha1pb.K8SRelease {
	t.Helper()
	op, err := e.releases.Create(t.Context(), &applicationsv1alpha1pb.CreateK8SReleaseRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: e.inst.ProjectID, Labels: map[string]string{"env": "dev"}}, Spec: sp,
	})
	id := mustOp(t, op, err)
	return e.get(t, id)
}

func (e *appsEnv) get(t *testing.T, id string) *applicationsv1alpha1pb.K8SRelease {
	t.Helper()
	r, err := e.releases.Get(t.Context(), &applicationsv1alpha1pb.GetK8SReleaseRequest{Id: id})
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	return r
}

func appsWantCode(t *testing.T, what string, err error, want codes.Code) {
	t.Helper()
	if got := status.Code(err); got != want {
		t.Fatalf("%s: code = %v, want %v (err: %v)", what, got, want, err)
	}
}

func TestApplicationsReleaseCRUD(t *testing.T) {
	t.Parallel()
	e := startApplications(t)
	ctx := t.Context()

	r := e.create(t, appsSpec(e.running))
	id := r.GetMetadata().GetId()
	if r.GetStatus().GetState() != applicationsv1alpha1pb.K8SReleaseStatus_DEPLOYED {
		t.Errorf("state = %v, want DEPLOYED", r.GetStatus().GetState())
	}
	if r.GetSpec().GetValues() != "" || len(r.GetSpec().GetSet()) != 0 {
		t.Errorf("Get returned input-only fields: %v", r.GetSpec())
	}
	if r.GetSpec().GetNamespace() != "mlflow" || r.GetMetadata().GetLabels()["env"] != "dev" {
		t.Errorf("release = %v", r)
	}

	// An unnamed resource: a second release with the same (empty) name is fine.
	second := e.create(t, appsSpec(e.running))
	l, err := e.releases.List(ctx, &applicationsv1alpha1pb.ListK8SReleasesRequest{ParentId: e.inst.ProjectID, ClusterId: e.running, PageSize: 1})
	if err != nil || len(l.GetItems()) != 1 || l.GetNextPageToken() == "" {
		t.Fatalf("List page 1 = %v, %v", l, err)
	}
	l, err = e.releases.List(ctx, &applicationsv1alpha1pb.ListK8SReleasesRequest{ParentId: e.inst.ProjectID, ClusterId: e.running, PageToken: l.GetNextPageToken()})
	if err != nil || len(l.GetItems()) != 1 || l.GetItems()[0].GetMetadata().GetId() != second.GetMetadata().GetId() {
		t.Fatalf("List page 2 = %v, %v", l, err)
	}
	if v := l.GetItems()[0].GetSpec(); v.GetValues() != "" || len(v.GetSet()) != 0 {
		t.Errorf("List returned input-only fields: %v", v)
	}

	up := appsSpec(e.running)
	up.Values = "replicas: 3\n"
	op, err := e.releases.Update(ctx, &applicationsv1alpha1pb.UpdateK8SReleaseRequest{
		Metadata: &commonpb.ResourceMetadata{Id: id, Labels: map[string]string{"env": "prod"}}, Spec: up,
	})
	mustOp(t, op, err)
	r = e.get(t, id)
	if r.GetMetadata().GetLabels()["env"] != "prod" || r.GetStatus().GetState() != applicationsv1alpha1pb.K8SReleaseStatus_DEPLOYED {
		t.Errorf("after update = %v", r)
	}

	op, err = e.releases.Delete(ctx, &applicationsv1alpha1pb.DeleteK8SReleaseRequest{Id: id})
	mustOp(t, op, err)
	_, err = e.releases.Get(ctx, &applicationsv1alpha1pb.GetK8SReleaseRequest{Id: id})
	appsWantCode(t, "get after delete", err, codes.NotFound)
}

func TestApplicationsReleaseValidation(t *testing.T) {
	t.Parallel()
	e := startApplications(t)
	ctx := t.Context()
	cop, err := e.startCluster(t, "creating") // still PROVISIONING
	if err != nil {
		t.Fatalf("start cluster: %v", err)
	}
	creating := cop.ResourceID()
	tests := []struct {
		name string
		edit func(*applicationsv1alpha1pb.K8SReleaseSpec)
		code codes.Code
	}{
		{"unknown product", func(s *applicationsv1alpha1pb.K8SReleaseSpec) { s.ProductSlug = "nope/nothing" }, codes.InvalidArgument},
		{"bad namespace", func(s *applicationsv1alpha1pb.K8SReleaseSpec) { s.Namespace = "Not_Valid" }, codes.InvalidArgument},
		{"bad application name", func(s *applicationsv1alpha1pb.K8SReleaseSpec) { s.ApplicationName = "-x" }, codes.InvalidArgument},
		{"bad values", func(s *applicationsv1alpha1pb.K8SReleaseSpec) { s.Values = "a: [b" }, codes.InvalidArgument},
		{"missing cluster", func(s *applicationsv1alpha1pb.K8SReleaseSpec) { s.ClusterId = appsMissingCluster }, codes.NotFound},
		{"cluster not running", func(s *applicationsv1alpha1pb.K8SReleaseSpec) { s.ClusterId = creating }, codes.FailedPrecondition},
	}
	for _, tt := range tests {
		sp := appsSpec(e.running)
		tt.edit(sp)
		op, err := e.releases.Create(ctx, &applicationsv1alpha1pb.CreateK8SReleaseRequest{
			Metadata: &commonpb.ResourceMetadata{ParentId: e.inst.ProjectID}, Spec: sp,
		})
		appsWantCode(t, tt.name, kmsWait(ctx, op, err), tt.code)
	}

	r := e.create(t, appsSpec(e.running))
	for name, edit := range map[string]func(*applicationsv1alpha1pb.K8SReleaseSpec){
		"cluster_id":       func(s *applicationsv1alpha1pb.K8SReleaseSpec) { s.ClusterId = creating },
		"product_slug":     func(s *applicationsv1alpha1pb.K8SReleaseSpec) { s.ProductSlug = "nebius/kuberay" },
		"namespace":        func(s *applicationsv1alpha1pb.K8SReleaseSpec) { s.Namespace = "other" },
		"application_name": func(s *applicationsv1alpha1pb.K8SReleaseSpec) { s.ApplicationName = "other" },
	} {
		sp := appsSpec(e.running)
		edit(sp)
		op, err := e.releases.Update(ctx, &applicationsv1alpha1pb.UpdateK8SReleaseRequest{
			Metadata: &commonpb.ResourceMetadata{Id: r.GetMetadata().GetId()}, Spec: sp,
		})
		appsWantCode(t, "update "+name, kmsWait(ctx, op, err), codes.InvalidArgument)
	}

	_, err = e.releases.List(ctx, &applicationsv1alpha1pb.ListK8SReleasesRequest{ParentId: e.inst.ProjectID, ClusterId: e.running, Filter: "x"})
	appsWantCode(t, "list filter", err, codes.InvalidArgument)
	_, err = e.releases.List(ctx, &applicationsv1alpha1pb.ListK8SReleasesRequest{ParentId: e.inst.ProjectID, ClusterId: appsMissingCluster})
	appsWantCode(t, "list unknown cluster", err, codes.NotFound)
}

// Deleting the cluster through the mk8s API removes the releases installed
// into it, and only those.
func TestApplicationsClusterDeleteCascade(t *testing.T) {
	t.Parallel()
	e := startApplications(t)
	ctx := t.Context()
	other := e.createCluster(t, "other")
	id := e.create(t, appsSpec(e.running)).GetMetadata().GetId()
	keep := e.create(t, appsSpec(other)).GetMetadata().GetId()

	op, err := e.clusters.Delete(ctx, &mk8spb.DeleteClusterRequest{Id: e.running})
	mustOp(t, op, err)
	_, err = e.releases.Get(ctx, &applicationsv1alpha1pb.GetK8SReleaseRequest{Id: id})
	appsWantCode(t, "get after cluster delete", err, codes.NotFound)
	e.get(t, keep)
}

func TestApplicationsAuthz(t *testing.T) {
	t.Parallel()
	e := startApplications(t)
	ctx := t.Context()
	id := e.create(t, appsSpec(e.running)).GetMetadata().GetId()
	sdk := e.inst.SDKAs(t, config.DefaultViewerToken)
	viewer := applicationssvc.NewK8SReleaseService(sdk)

	if _, err := viewer.Get(ctx, &applicationsv1alpha1pb.GetK8SReleaseRequest{Id: id}); err != nil {
		t.Errorf("viewer Get: %v", err)
	}
	if l, err := viewer.List(ctx, &applicationsv1alpha1pb.ListK8SReleasesRequest{ParentId: e.inst.ProjectID, ClusterId: e.running}); err != nil || len(l.GetItems()) != 1 {
		t.Errorf("viewer List = %v, %v", l, err)
	}
	op, err := viewer.Create(ctx, &applicationsv1alpha1pb.CreateK8SReleaseRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: e.inst.ProjectID}, Spec: appsSpec(e.running),
	})
	appsWantCode(t, "viewer Create", kmsWait(ctx, op, err), codes.PermissionDenied)
	op, err = viewer.Delete(ctx, &applicationsv1alpha1pb.DeleteK8SReleaseRequest{Id: id})
	appsWantCode(t, "viewer Delete", kmsWait(ctx, op, err), codes.PermissionDenied)

	// The VM app template catalog is not served: callers see UNIMPLEMENTED,
	// not PERMISSION_DENIED.
	templates := applicationssvc.NewVmAppTemplateService(sdk)
	_, err = templates.Get(ctx, &applicationsv1alpha1pb.GetVmAppTemplateRequest{Id: "vmapptemplate-1"})
	appsWantCode(t, "template Get", err, codes.Unimplemented)
	_, err = templates.List(ctx, &applicationsv1alpha1pb.ListVmAppTemplatesRequest{ParentId: e.inst.ProjectID})
	appsWantCode(t, "template List", err, codes.Unimplemented)
}
