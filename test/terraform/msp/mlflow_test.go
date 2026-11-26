//go:build e2e

package msp

import (
	"bytes"
	"reflect"
	"testing"
	"text/template"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	iamv2pb "github.com/nebius/gosdk/proto/nebius/iam/v2"
	mlflowpb "github.com/nebius/gosdk/proto/nebius/msp/mlflow/v1alpha1"
	mspv1alpha1pb "github.com/nebius/gosdk/proto/nebius/msp/v1alpha1"
	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"
	iamv2svc "github.com/nebius/gosdk/services/nebius/iam/v2"
	mlflowsvc "github.com/nebius/gosdk/services/nebius/msp/mlflow/v1alpha1"
	storagesvc "github.com/nebius/gosdk/services/nebius/storage/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore/memory"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/services/mspmlflow"
	"github.com/kfirgollan/NebiusMocker/services/storage"
	"github.com/kfirgollan/NebiusMocker/services/vpc"
)

const (
	mlflowPassword = "mlflow-Secret-pw-1234"
	namedBucket    = "my-artifacts"
)

func startMLflow(t *testing.T) *testkit.Instance {
	t.Helper()
	return testkit.StartT(t, []mocker.Module{iam.New(), vpc.New(), storage.New(storage.WithS3Listen("127.0.0.1:0")), mspmlflow.New()},
		mockerOptions(testkit.WithObjStore(memory.New(clock.Real{})), testkit.WithOperationDelay(200*time.Millisecond),
			testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce))...)
}

type mlflowCfg struct {
	Proj        string
	DescAuto    string // description of the cluster with the auto-created bucket
	DataSources bool
	PublicNamed bool
}

var mlflowTmpl = template.Must(template.New("mlflow").Parse(`
resource "nebius_iam_v1_service_account" "sa" {
  parent_id = "{{.Proj}}"
  name      = "mlflow-sa"
}

resource "nebius_storage_v1_bucket" "artifacts" {
  parent_id = "{{.Proj}}"
  name      = "` + namedBucket + `"
}

# No storage_bucket_name: the service creates the bucket.
resource "nebius_msp_mlflow_v1alpha1_cluster" "auto" {
  parent_id          = "{{.Proj}}"
  name               = "tf-mlflow-auto"
  description        = "{{.DescAuto}}"
  admin_username     = "admin"
  admin_password     = "` + mlflowPassword + `"
  service_account_id = nebius_iam_v1_service_account.sa.id
}

resource "nebius_msp_mlflow_v1alpha1_cluster" "named" {
  parent_id           = "{{.Proj}}"
  name                = "tf-mlflow-named"
  public_access       = {{.PublicNamed}}
  size                = "medium"
  admin_username      = "admin"
  admin_password      = "` + mlflowPassword + `"
  service_account_id  = nebius_iam_v1_service_account.sa.id
  storage_bucket_name = nebius_storage_v1_bucket.artifacts.name
}
{{if .DataSources}}
data "nebius_msp_mlflow_v1alpha1_cluster" "by_id" {
  id = nebius_msp_mlflow_v1alpha1_cluster.named.id
}
data "nebius_msp_mlflow_v1alpha1_cluster" "by_name" {
  parent_id = "{{.Proj}}"
  name      = nebius_msp_mlflow_v1alpha1_cluster.named.name
}
{{end}}`))

func (c *mlflowCfg) render(t *testing.T) string {
	t.Helper()
	var b bytes.Buffer
	if err := mlflowTmpl.Execute(&b, c); err != nil {
		t.Fatalf("render: %v", err)
	}
	return b.String()
}

// exposure is whether a cluster has a public tracking endpoint.
type exposure int

const (
	privateOnly exposure = iota
	publicEndpoint
)

type mlflowEnv struct {
	inst     *testkit.Instance
	clusters mlflowsvc.ClusterService
	buckets  storagesvc.BucketService
	keys     iamv2svc.AccessKeyService
}

// keyNames are the names of the project's access keys: the service creates
// one per cluster, named after the cluster ID.
func (e *mlflowEnv) keyNames(t *testing.T) []string {
	t.Helper()
	res, err := e.keys.List(t.Context(), &iamv2pb.ListAccessKeysRequest{ParentId: e.inst.ProjectID})
	if err != nil {
		t.Fatalf("list access keys: %v", err)
	}
	var names []string
	for _, k := range res.GetItems() {
		names = append(names, k.GetMetadata().GetName())
	}
	return names
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// check verifies a RUNNING cluster through the Go SDK.
func (e *mlflowEnv) check(t *testing.T, step, id, name string, wantBucket func(string) string, access exposure, saID string) *mlflowpb.Cluster {
	t.Helper()
	public := access == publicEndpoint
	c, err := e.clusters.Get(t.Context(), &mlflowpb.GetClusterRequest{Id: id})
	if err != nil {
		t.Fatalf("%s: get cluster %s: %v", step, id, err)
	}
	sp, st := c.GetSpec(), c.GetStatus()
	if st.GetPhase() != mspv1alpha1pb.ClusterStatus_PHASE_RUNNING || st.GetState() != mspv1alpha1pb.ClusterStatus_STATE_FINISHED {
		t.Errorf("%s: status = %v, want RUNNING/FINISHED", step, st)
	}
	if c.GetMetadata().GetName() != name || c.GetMetadata().GetParentId() != e.inst.ProjectID {
		t.Errorf("%s: metadata = %v", step, c.GetMetadata())
	}
	if sp.GetAdminUsername() != "admin" || sp.GetAdminPassword() != "" || sp.GetServiceAccountId() != saID || sp.GetPublicAccess() != public {
		t.Errorf("%s: spec = %v (admin_password must never be returned)", step, sp)
	}
	ep := st.GetTrackingEndpoints()
	wantEP := ep.GetPrivate()
	if public {
		wantEP = ep.GetPublic()
	}
	if ep.GetPrivate() == "" || (ep.GetPublic() != "") != public || st.GetTrackingEndpoint() == "" || st.GetTrackingEndpoint() != wantEP {
		t.Errorf("%s: tracking endpoints = %v, tracking_endpoint %q, public=%v", step, ep, st.GetTrackingEndpoint(), public)
	}
	if st.GetMlflowVersion() == "" || st.GetEffectiveStorageBucketName() != wantBucket(id) {
		t.Errorf("%s: mlflow_version %q, effective bucket %q, want %q", step, st.GetMlflowVersion(), st.GetEffectiveStorageBucketName(), wantBucket(id))
	}
	if b, err := e.buckets.GetByName(t.Context(), &storagepb.GetBucketByNameRequest{ParentId: e.inst.ProjectID, Name: wantBucket(id)}); err != nil ||
		b.GetStatus().GetState() != storagepb.BucketStatus_ACTIVE {
		t.Errorf("%s: bucket %s = %v, %v", step, wantBucket(id), b, err)
	}
	if !contains(e.keyNames(t), id) {
		t.Errorf("%s: no access key for cluster %s while it lives (keys: %v)", step, id, e.keyNames(t))
	}
	return c
}

func TestMLflowClusterLifecycle(t *testing.T) {
	inst := startMLflow(t)
	sdk := inst.SDK(t)
	e := &mlflowEnv{inst: inst, clusters: mlflowsvc.NewClusterService(sdk), buckets: storagesvc.NewBucketService(sdk), keys: iamv2svc.NewAccessKeyService(sdk)}
	tf := newTerraform(t, inst)
	c := mlflowCfg{Proj: inst.ProjectID, DescAuto: "auto bucket"}
	autoBucket := func(id string) string { return id }
	fixedBucket := func(string) string { return namedBucket }

	tf.writeConfig(t, c.render(t))
	tf.init(t)
	tf.expectPlan(t, map[string]string{"sa": "create", "artifacts": "create", "auto": "create", "named": "create"})
	tf.apply(t)
	tf.expectNoChanges(t)
	ids := tf.stateIDs(t)
	auto1, named, sa := ids["auto"], ids["named"], ids["sa"]
	if auto1 == "" || named == "" || sa == "" {
		t.Fatalf("state ids = %v", ids)
	}
	ca := e.check(t, "create auto", auto1, "tf-mlflow-auto", autoBucket, privateOnly, sa)
	if ca.GetSpec().GetStorageBucketName() != "" || ca.GetSpec().GetDescription() != "auto bucket" {
		t.Errorf("create auto: spec = %v, want the bucket name left unset as configured", ca.GetSpec())
	}
	cn := e.check(t, "create named", named, "tf-mlflow-named", fixedBucket, privateOnly, sa)
	if cn.GetSpec().GetStorageBucketName() != namedBucket {
		t.Errorf("create named: storage_bucket_name = %q", cn.GetSpec().GetStorageBucketName())
	}
	if len(e.keyNames(t)) != 2 {
		t.Errorf("access keys = %v, want one per cluster", e.keyNames(t))
	}

	// Replacement (there is no Update RPC): a description change on the first cluster.
	c.DescAuto = "auto bucket v2"
	tf.writeConfig(t, c.render(t))
	tf.expectPlan(t, map[string]string{"sa": "no-op", "artifacts": "no-op", "named": "no-op", "auto": "replace"})
	tf.apply(t)
	tf.expectNoChanges(t)
	auto2 := tf.stateIDs(t)["auto"]
	if auto2 == "" || auto2 == auto1 {
		t.Fatalf("description change did not replace the cluster: %s -> %s", auto1, auto2)
	}
	if _, err := e.clusters.Get(t.Context(), &mlflowpb.GetClusterRequest{Id: auto1}); status.Code(err) != codes.NotFound {
		t.Errorf("replaced cluster %s: get err = %v, want NotFound", auto1, err)
	}
	if contains(e.keyNames(t), auto1) {
		t.Errorf("the replaced cluster's access key is still there: %v", e.keyNames(t))
	}
	if got := e.check(t, "replace auto", auto2, "tf-mlflow-auto", autoBucket, privateOnly, sa); got.GetSpec().GetDescription() != "auto bucket v2" {
		t.Errorf("replaced cluster description = %q", got.GetSpec().GetDescription())
	}
	// The first cluster's auto-created bucket outlives it.
	if _, err := e.buckets.GetByName(t.Context(), &storagepb.GetBucketByNameRequest{ParentId: inst.ProjectID, Name: autoBucket(auto1)}); err != nil {
		t.Errorf("bucket of the replaced cluster: %v", err)
	}

	// Replacement through public_access on the named cluster.
	c.PublicNamed = true
	tf.writeConfig(t, c.render(t))
	tf.expectPlan(t, map[string]string{"sa": "no-op", "artifacts": "no-op", "named": "replace", "auto": "no-op"})
	tf.apply(t)
	tf.expectNoChanges(t)
	named2 := tf.stateIDs(t)["named"]
	if named2 == "" || named2 == named {
		t.Fatalf("public_access change did not replace the cluster: %s -> %s", named, named2)
	}
	e.check(t, "replace named", named2, "tf-mlflow-named", fixedBucket, publicEndpoint, sa)

	// Data sources by id and by name.
	c.DataSources = true
	tf.writeConfig(t, c.render(t))
	tf.apply(t)
	tf.expectNoChanges(t)
	state := tf.stateAttrs(t)
	byID, byName := state["data.nebius_msp_mlflow_v1alpha1_cluster.by_id"], state["data.nebius_msp_mlflow_v1alpha1_cluster.by_name"]
	if byID == nil || byName == nil {
		t.Fatalf("data sources missing from the state: %v", state)
	}
	if byID["id"] != named2 || byName["id"] != named2 {
		t.Errorf("data source ids = %v / %v, want %s", byID["id"], byName["id"], named2)
	}
	if !reflect.DeepEqual(byID, byName) {
		t.Errorf("data sources by id and by name differ:\n%v\n%v", byID, byName)
	}
	res := state["nebius_msp_mlflow_v1alpha1_cluster.named"]
	for _, k := range []string{"name", "parent_id", "public_access", "admin_username", "service_account_id", "storage_bucket_name", "network_id", "size", "status"} {
		if !reflect.DeepEqual(byID[k], res[k]) {
			t.Errorf("data source %s = %v, resource has %v", k, byID[k], res[k])
		}
	}
	if pw := byID["admin_password"]; pw != nil {
		t.Errorf("data source exposes admin_password: %v", pw)
	}

	// Destroy: clusters and their keys are gone, the auto-created buckets stay, the named one is Terraform's and goes.
	tf.destroy(t)
	for _, id := range []string{auto2, named2} {
		if _, err := e.clusters.Get(t.Context(), &mlflowpb.GetClusterRequest{Id: id}); status.Code(err) != codes.NotFound {
			t.Errorf("after destroy: get %s err = %v, want NotFound", id, err)
		}
	}
	if names := e.keyNames(t); len(names) != 0 {
		t.Errorf("access keys after destroy = %v, want none", names)
	}
	for _, b := range []string{autoBucket(auto1), autoBucket(auto2)} {
		if _, err := e.buckets.GetByName(t.Context(), &storagepb.GetBucketByNameRequest{ParentId: inst.ProjectID, Name: b}); err != nil {
			t.Errorf("auto-created bucket %s after destroy: %v", b, err)
		}
	}
	if b, err := e.buckets.GetByName(t.Context(), &storagepb.GetBucketByNameRequest{ParentId: inst.ProjectID, Name: namedBucket}); err == nil &&
		b.GetStatus().GetState() == storagepb.BucketStatus_ACTIVE {
		t.Errorf("Terraform's bucket %s is still ACTIVE after destroy", namedBucket)
	}
}
