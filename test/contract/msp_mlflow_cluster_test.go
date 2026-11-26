package contract

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iamv2pb "github.com/nebius/gosdk/proto/nebius/iam/v2"
	mlflowpb "github.com/nebius/gosdk/proto/nebius/msp/mlflow/v1alpha1"
	mspv1alpha1pb "github.com/nebius/gosdk/proto/nebius/msp/v1alpha1"
	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"
	iamsvc "github.com/nebius/gosdk/services/nebius/iam/v1"
	iamv2svc "github.com/nebius/gosdk/services/nebius/iam/v2"
	mlflowsvc "github.com/nebius/gosdk/services/nebius/msp/mlflow/v1alpha1"
	storagesvc "github.com/nebius/gosdk/services/nebius/storage/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore/memory"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/services/mspmlflow"
	"github.com/kfirgollan/NebiusMocker/services/storage"
	"github.com/kfirgollan/NebiusMocker/services/vpc"
)

const mlflowPassword = "Sup3r-secret-pw"

// mlflowEnv is a mocker with IAM, VPC, Object Storage and Managed MLflow
// (mock backend).
type mlflowEnv struct {
	inst     *testkit.Instance
	project  string
	other    string // a second project
	sa       string // a service account of the project
	otherSA  string // a service account of the other project
	clusters mlflowsvc.ClusterService
	buckets  storagesvc.BucketService
	keys     iamv2svc.AccessKeyService
}

func startMLflow(t *testing.T, opts ...testkit.Option) *mlflowEnv {
	t.Helper()
	opts = append([]testkit.Option{
		testkit.WithObjStore(memory.New(clock.Real{})), testkit.WithOperationDelay(20 * time.Millisecond),
		testkit.WithProjects(config.ProjectSeed{Name: "other"}),
	}, opts...)
	inst := testkit.StartT(t, []mocker.Module{iam.New(), vpc.New(), storage.New(storage.WithS3Listen("127.0.0.1:0")), mspmlflow.New()}, opts...)
	sdk := inst.SDK(t)
	sas := iamsvc.NewServiceAccountService(sdk)
	e := &mlflowEnv{
		inst: inst, project: inst.ProjectID, other: inst.Projects[1].ID,
		clusters: mlflowsvc.NewClusterService(sdk), buckets: storagesvc.NewBucketService(sdk), keys: iamv2svc.NewAccessKeyService(sdk),
	}
	e.sa = createServiceAccount(t, sas, e.project, "mlflow-sa").GetMetadata().GetId()
	e.otherSA = createServiceAccount(t, sas, e.other, "foreign-sa").GetMetadata().GetId()
	return e
}

func (e *mlflowEnv) spec() *mlflowpb.ClusterSpec {
	return &mlflowpb.ClusterSpec{AdminUsername: "admin", AdminPassword: mlflowPassword, ServiceAccountId: e.sa}
}

func (e *mlflowEnv) createReq(name string, spec *mlflowpb.ClusterSpec) *mlflowpb.CreateClusterRequest {
	return &mlflowpb.CreateClusterRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: e.project, Name: name, Labels: map[string]string{"team": "ml"}},
		Spec:     spec,
	}
}

// create creates a cluster and waits for the operation.
func (e *mlflowEnv) create(t *testing.T, name string, spec *mlflowpb.ClusterSpec) *mlflowpb.Cluster {
	t.Helper()
	id := alphaWait(t)(e.clusters.Create(t.Context(), e.createReq(name, spec)))
	return e.get(t, id)
}

func (e *mlflowEnv) get(t *testing.T, id string) *mlflowpb.Cluster {
	t.Helper()
	c, err := e.clusters.Get(t.Context(), &mlflowpb.GetClusterRequest{Id: id})
	if err != nil {
		t.Fatalf("Get %s: %v", id, err)
	}
	return c
}

func mlflowPhase(c *mlflowpb.Cluster) string {
	return c.GetStatus().GetPhase().String() + "/" + c.GetStatus().GetState().String()
}

func TestMLflowClusterLifecycle(t *testing.T) {
	t.Parallel()
	e := startMLflow(t)
	ctx := t.Context()
	wait := alphaWait(t)

	opID := ""
	op, err := e.clusters.Create(ctx, e.createReq("tracking", e.spec()))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	opID = op.ID()
	if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("Create operation %s: %v", opID, err)
	}
	id := op.ResourceID()
	if !strings.HasPrefix(id, "mlflow-") {
		t.Fatalf("cluster id = %q, want the mlflow- prefix", id)
	}

	c := e.get(t, id)
	if got := mlflowPhase(c); got != "PHASE_RUNNING/STATE_FINISHED" {
		t.Fatalf("phase/state = %s", got)
	}
	md, spec, st := c.GetMetadata(), c.GetSpec(), c.GetStatus()
	if md.GetParentId() != e.project || md.GetName() != "tracking" || md.GetLabels()["team"] != "ml" || md.GetCreatedAt() == nil || md.GetResourceVersion() < 1 {
		t.Errorf("metadata = %v", md)
	}
	if spec.GetAdminUsername() != "admin" || spec.GetServiceAccountId() != e.sa || spec.GetSize() != "medium" || spec.GetNetworkId() == "" ||
		spec.GetStorageBucketName() != "" || spec.GetPublicAccess() || spec.GetDescription() != "" {
		t.Errorf("spec = %v: size and network are filled, optional fields echo the request", spec)
	}
	if st.GetMlflowVersion() == "" || st.GetEffectiveStorageBucketName() != id || st.GetTrackingEndpoints().GetPrivate() == "" ||
		st.GetTrackingEndpoints().GetPublic() != "" || st.GetTrackingEndpoint() != st.GetTrackingEndpoints().GetPrivate() {
		t.Errorf("status = %v", st)
	}
	if strings.Contains(c.String(), mlflowPassword) || spec.GetAdminPassword() != "" {
		t.Errorf("Get returns the admin password: %v", c)
	}

	// The artifact bucket is an ordinary bucket, the key an ordinary access key of the service account.
	b, err := e.buckets.GetByName(ctx, &storagepb.GetBucketByNameRequest{ParentId: e.project, Name: st.GetEffectiveStorageBucketName()})
	if err != nil || b.GetStatus().GetState() != storagepb.BucketStatus_ACTIVE {
		t.Fatalf("artifact bucket = %v, %v", b, err)
	}
	keys, err := e.keys.List(ctx, &iamv2pb.ListAccessKeysRequest{ParentId: e.project})
	if err != nil || len(keys.GetItems()) != 1 {
		t.Fatalf("access keys = %v, %v; want the cluster's one key", keys, err)
	}
	if k := keys.GetItems()[0]; k.GetMetadata().GetName() != id || k.GetSpec().GetAccount().GetServiceAccount().GetId() != e.sa ||
		k.GetStatus().GetState() != iamv2pb.AccessKeyStatus_ACTIVE || !strings.Contains(k.GetSpec().GetDescription(), "tracking") {
		t.Errorf("access key = %v", k)
	}

	// Reads.
	byName, err := e.clusters.GetByName(ctx, &mlflowpb.GetClusterByNameRequest{ParentId: e.project, Name: "tracking"})
	if err != nil || byName.GetMetadata().GetId() != id {
		t.Errorf("GetByName = %v, %v", byName, err)
	}
	list, err := e.clusters.List(ctx, &mlflowpb.ListClustersRequest{ParentId: e.project})
	if err != nil || len(list.GetItems()) != 1 || list.GetItems()[0].GetMetadata().GetId() != id || list.GetNextPageToken() != "" {
		t.Errorf("List = %v, %v", list, err)
	}
	if list, err := e.clusters.List(ctx, &mlflowpb.ListClustersRequest{ParentId: e.other}); err != nil || len(list.GetItems()) != 0 {
		t.Errorf("List(other project) = %v, %v", list, err)
	}

	// Stop: there is no STOPPED phase, a stopped cluster is STOPPING and finished.
	wait(e.clusters.Stop(ctx, &mlflowpb.StopClusterRequest{Id: id}))
	if got := mlflowPhase(e.get(t, id)); got != "PHASE_STOPPING/STATE_FINISHED" {
		t.Errorf("after Stop: %s", got)
	}
	_, err = e.clusters.Stop(ctx, &mlflowpb.StopClusterRequest{Id: id})
	wantFailure(t, "Stop of a stopped cluster", err, codes.FailedPrecondition, "tracking")

	wait(e.clusters.Start(ctx, &mlflowpb.StartClusterRequest{Id: id}))
	if got := mlflowPhase(e.get(t, id)); got != "PHASE_RUNNING/STATE_FINISHED" {
		t.Errorf("after Start: %s", got)
	}
	_, err = e.clusters.Start(ctx, &mlflowpb.StartClusterRequest{Id: id})
	wantFailure(t, "Start of a running cluster", err, codes.FailedPrecondition, "tracking")
	if keys, err := e.keys.List(ctx, &iamv2pb.ListAccessKeysRequest{ParentId: e.project}); err != nil || len(keys.GetItems()) != 1 {
		t.Errorf("access keys after restart = %v, %v; want still one", keys, err)
	}

	// Delete: the key goes, the bucket stays, the name is free.
	wait(e.clusters.Delete(ctx, &mlflowpb.DeleteClusterRequest{Id: id}))
	_, err = e.clusters.Get(ctx, &mlflowpb.GetClusterRequest{Id: id})
	wantFailure(t, "Get after Delete", err, codes.NotFound)
	if keys, err := e.keys.List(ctx, &iamv2pb.ListAccessKeysRequest{ParentId: e.project}); err != nil || len(keys.GetItems()) != 0 {
		t.Errorf("access keys after Delete = %v, %v; want none", keys, err)
	}
	if _, err := e.buckets.GetByName(ctx, &storagepb.GetBucketByNameRequest{ParentId: e.project, Name: id}); err != nil {
		t.Errorf("the artifact bucket must outlive the cluster: %v", err)
	}
	_, err = e.clusters.Delete(ctx, &mlflowpb.DeleteClusterRequest{Id: id})
	wantFailure(t, "second Delete", err, codes.NotFound)
	e.create(t, "tracking", e.spec())
}

func TestMLflowNamedBucketAndPublicAccess(t *testing.T) {
	t.Parallel()
	e := startMLflow(t)
	ctx := t.Context()
	createBucket(t, e.buckets, e.project, "my-artifacts", &storagepb.BucketSpec{})

	spec := e.spec()
	spec.StorageBucketName, spec.PublicAccess, spec.Description, spec.Size = "my-artifacts", true, "shared tracking", "medium"
	c := e.create(t, "public", spec)
	if c.GetSpec().GetStorageBucketName() != "my-artifacts" || c.GetStatus().GetEffectiveStorageBucketName() != "my-artifacts" {
		t.Errorf("bucket: spec %q, status %q", c.GetSpec().GetStorageBucketName(), c.GetStatus().GetEffectiveStorageBucketName())
	}
	if !c.GetSpec().GetPublicAccess() || c.GetSpec().GetDescription() != "shared tracking" {
		t.Errorf("spec = %v", c.GetSpec())
	}
	ep := c.GetStatus().GetTrackingEndpoints()
	if ep.GetPublic() == "" || ep.GetPrivate() == "" || c.GetStatus().GetTrackingEndpoint() != ep.GetPublic() {
		t.Errorf("endpoints = %v, tracking_endpoint %q; want the public one", ep, c.GetStatus().GetTrackingEndpoint())
	}
	list, err := e.buckets.List(ctx, &storagepb.ListBucketsRequest{ParentId: e.project})
	if err != nil || len(list.GetItems()) != 1 {
		t.Errorf("buckets = %v, %v; want only the named one", list, err)
	}
}

func TestMLflowCreateErrors(t *testing.T) {
	t.Parallel()
	e := startMLflow(t)
	ctx := t.Context()
	e.create(t, "taken", e.spec())

	tests := []struct {
		name   string
		meta   *commonpb.ResourceMetadata
		mutate func(*mlflowpb.ClusterSpec)
		code   codes.Code
		field  string
	}{
		{"no admin username", nil, func(s *mlflowpb.ClusterSpec) { s.AdminUsername = "" }, codes.InvalidArgument, "admin_username"},
		{"admin username too long", nil, func(s *mlflowpb.ClusterSpec) { s.AdminUsername = strings.Repeat("a", 64) }, codes.InvalidArgument, "admin_username"},
		{"no admin password", nil, func(s *mlflowpb.ClusterSpec) { s.AdminPassword = "" }, codes.InvalidArgument, "admin_password"},
		{"no service account", nil, func(s *mlflowpb.ClusterSpec) { s.ServiceAccountId = "" }, codes.InvalidArgument, "service_account_id"},
		{"unknown service account", nil, func(s *mlflowpb.ClusterSpec) { s.ServiceAccountId = "serviceaccount-e00nope" }, codes.NotFound, ""},
		{"service account of another project", nil, func(s *mlflowpb.ClusterSpec) { s.ServiceAccountId = e.otherSA }, codes.InvalidArgument, "service_account_id"},
		{"unknown size", nil, func(s *mlflowpb.ClusterSpec) { s.Size = "huge" }, codes.InvalidArgument, "size"},
		{"missing bucket", nil, func(s *mlflowpb.ClusterSpec) { s.StorageBucketName = "no-such-bucket" }, codes.NotFound, "no-such-bucket"},
		{"unknown network", nil, func(s *mlflowpb.ClusterSpec) { s.NetworkId = "vpcnetwork-e00nope" }, codes.NotFound, ""},
		{"duplicate name", &commonpb.ResourceMetadata{ParentId: e.project, Name: "taken"}, nil, codes.AlreadyExists, ""},
		{"unknown project", &commonpb.ResourceMetadata{ParentId: "project-e00nope", Name: "x"}, nil, codes.NotFound, ""},
		{"parent is not a project", &commonpb.ResourceMetadata{ParentId: "tenant-e00nope", Name: "x"}, nil, codes.InvalidArgument, "parent_id"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			spec := e.spec()
			if tt.mutate != nil {
				tt.mutate(spec)
			}
			req := e.createReq("fresh-"+strings.ReplaceAll(tt.name, " ", "-"), spec)
			if tt.meta != nil {
				req.Metadata = tt.meta
			}
			var frags []string
			if tt.field != "" {
				frags = append(frags, tt.field)
			}
			_, err := e.clusters.Create(ctx, req)
			wantFailure(t, tt.name, err, tt.code, frags...)
		})
	}

	// Nothing was left behind by the rejected calls.
	if list, err := e.clusters.List(ctx, &mlflowpb.ListClustersRequest{ParentId: e.project}); err != nil || len(list.GetItems()) != 1 {
		t.Errorf("clusters after rejected creates = %v, %v; want only %q", list, err, "taken")
	}
	if keys, err := e.keys.List(ctx, &iamv2pb.ListAccessKeysRequest{ParentId: e.project}); err != nil || len(keys.GetItems()) != 1 {
		t.Errorf("access keys after rejected creates = %v, %v; want only the first cluster's", keys, err)
	}
}

func TestMLflowReadAndLifecycleErrors(t *testing.T) {
	t.Parallel()
	e := startMLflow(t)
	ctx := t.Context()
	const ghost = "mlflow-e00nope"

	_, err := e.clusters.Get(ctx, &mlflowpb.GetClusterRequest{Id: ghost})
	wantFailure(t, "Get", err, codes.NotFound)
	_, err = e.clusters.GetByName(ctx, &mlflowpb.GetClusterByNameRequest{ParentId: e.project, Name: "ghost"})
	wantFailure(t, "GetByName", err, codes.NotFound)
	_, err = e.clusters.Stop(ctx, &mlflowpb.StopClusterRequest{Id: ghost})
	wantFailure(t, "Stop", err, codes.NotFound)
	_, err = e.clusters.Start(ctx, &mlflowpb.StartClusterRequest{Id: ghost})
	wantFailure(t, "Start", err, codes.NotFound)
	_, err = e.clusters.Delete(ctx, &mlflowpb.DeleteClusterRequest{Id: ghost})
	wantFailure(t, "Delete", err, codes.NotFound)
	_, err = e.clusters.Get(ctx, &mlflowpb.GetClusterRequest{})
	wantFailure(t, "Get without id", err, codes.InvalidArgument)
	_, err = e.clusters.List(ctx, &mlflowpb.ListClustersRequest{ParentId: e.project, PageSize: -1})
	wantFailure(t, "List with a negative page size", err, codes.InvalidArgument, "page_size")
	_, err = e.clusters.List(ctx, &mlflowpb.ListClustersRequest{ParentId: e.project, PageToken: "???"})
	wantFailure(t, "List with a bad page token", err, codes.InvalidArgument, "page_token")
}

func TestMLflowListPaginates(t *testing.T) {
	t.Parallel()
	e := startMLflow(t)
	ctx := t.Context()
	for _, n := range []string{"a", "b", "c"} {
		e.create(t, n, e.spec())
	}
	var names []string
	token := ""
	for range 5 {
		page, err := e.clusters.List(ctx, &mlflowpb.ListClustersRequest{ParentId: e.project, PageSize: 2, PageToken: token})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		for _, c := range page.GetItems() {
			names = append(names, c.GetMetadata().GetName())
		}
		if token = page.GetNextPageToken(); token == "" {
			break
		}
	}
	if strings.Join(names, ",") != "a,b,c" {
		t.Errorf("listed %v, want a,b,c in creation order", names)
	}
}

func TestMLflowStatusPhasesAreTheSharedMSPEnum(t *testing.T) {
	t.Parallel()
	e := startMLflow(t)
	c := e.create(t, "phases", e.spec())
	if c.GetStatus().GetPhase() != mspv1alpha1pb.ClusterStatus_PHASE_RUNNING || c.GetStatus().GetState() != mspv1alpha1pb.ClusterStatus_STATE_FINISHED {
		t.Errorf("phase/state = %s", mlflowPhase(c))
	}
}
