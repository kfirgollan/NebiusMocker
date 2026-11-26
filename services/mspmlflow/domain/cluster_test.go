package domain_test

import (
	"errors"
	"strings"
	"testing"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	mlflowpb "github.com/nebius/gosdk/proto/nebius/msp/mlflow/v1alpha1"
	mspv1alpha1pb "github.com/nebius/gosdk/proto/nebius/msp/v1alpha1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	"github.com/kfirgollan/NebiusMocker/services/mspmlflow/domain"
)

const adminPassword = "s3cret-Passw0rd"

func validSpec() *mlflowpb.ClusterSpec {
	return &mlflowpb.ClusterSpec{AdminUsername: "admin", AdminPassword: adminPassword, ServiceAccountId: saID}
}

func createCmd(name string, spec *mlflowpb.ClusterSpec) domain.CreateClusterCmd {
	md := &commonpb.ResourceMetadata{ParentId: projectID, Name: name, Labels: map[string]string{"team": "ml"}}
	return domain.CreateClusterCmd{Metadata: md, Spec: spec, Request: &mlflowpb.CreateClusterRequest{Metadata: md, Spec: spec}}
}

// create creates a cluster and fails the test unless the operation succeeds.
func (e *env) create(t *testing.T, name string, spec *mlflowpb.ClusterSpec) *mlflowpb.Cluster {
	t.Helper()
	op, err := e.svc.Create(t.Context(), createCmd(name, spec))
	if err != nil {
		t.Fatalf("Create(%s): %v", name, err)
	}
	if op.Err != nil {
		t.Fatalf("Create(%s) operation: %v", name, op.Err)
	}
	return e.get(t, op.ResourceID)
}

func (e *env) get(t *testing.T, id string) *mlflowpb.Cluster {
	t.Helper()
	c, err := e.svc.Get(t.Context(), id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	return c
}

func (e *env) op(t *testing.T) func(*operations.Operation, error) *operations.Operation {
	t.Helper()
	return func(op *operations.Operation, err error) *operations.Operation {
		t.Helper()
		if err != nil {
			t.Fatalf("operation not accepted: %v", err)
		}
		if op.Err != nil {
			t.Fatalf("operation failed: %v", op.Err)
		}
		return op
	}
}

func phaseState(c *mlflowpb.Cluster) (mspv1alpha1pb.ClusterStatus_Phase, mspv1alpha1pb.ClusterStatus_State) {
	return c.GetStatus().GetPhase(), c.GetStatus().GetState()
}

func TestCreateRunsAClusterWithDefaults(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := e.create(t, "ml", validSpec())
	id := c.GetMetadata().GetId()

	if p, s := phaseState(c); p != mspv1alpha1pb.ClusterStatus_PHASE_RUNNING || s != mspv1alpha1pb.ClusterStatus_STATE_FINISHED {
		t.Fatalf("phase/state = %v/%v, want RUNNING/FINISHED", p, s)
	}
	md := c.GetMetadata()
	if md.GetParentId() != projectID || md.GetName() != "ml" || md.GetResourceVersion() < 1 || md.GetCreatedAt() == nil || md.GetLabels()["team"] != "ml" {
		t.Errorf("metadata = %v", md)
	}
	spec := c.GetSpec()
	if spec.GetSize() != "medium" {
		t.Errorf("size = %q, want the smallest catalog size", spec.GetSize())
	}
	if spec.GetNetworkId() != defaultNetwork {
		t.Errorf("network_id = %q, want the project's default network", spec.GetNetworkId())
	}
	// Optional fields without a server default are echoed exactly.
	if spec.GetStorageBucketName() != "" || spec.GetDescription() != "" || spec.GetPublicAccess() {
		t.Errorf("optional fields were filled: %v", spec)
	}
	st := c.GetStatus()
	if st.GetMlflowVersion() != "2.22.0" {
		t.Errorf("mlflow_version = %q", st.GetMlflowVersion())
	}
	if st.GetEffectiveStorageBucketName() != id {
		t.Errorf("effective bucket = %q", st.GetEffectiveStorageBucketName())
	}
	if st.GetTrackingEndpoints().GetPrivate() != id+".private.test:443" || st.GetTrackingEndpoints().GetPublic() != "" ||
		st.GetTrackingEndpoint() != st.GetTrackingEndpoints().GetPrivate() {
		t.Errorf("endpoints = %v / %q", st.GetTrackingEndpoints(), st.GetTrackingEndpoint())
	}
	if e.peers.networks.bound[id] != defaultNetwork {
		t.Errorf("network bindings = %v, want %s bound to %s", e.peers.networks.bound, id, defaultNetwork)
	}
}

func TestCreateNeverStoresOrReturnsThePassword(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	spec := validSpec()
	op := e.op(t)(e.svc.Create(t.Context(), createCmd("ml", spec)))

	stored, err := e.clusters.Get(t.Context(), op.ResourceID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.GetSpec().GetAdminPassword() != "" || strings.Contains(stored.String(), adminPassword) {
		t.Errorf("the stored cluster holds the password: %v", stored)
	}
	if got := e.get(t, op.ResourceID); strings.Contains(got.String(), adminPassword) {
		t.Errorf("Get returns the password: %v", got)
	}
	if req, ok := op.Request.(*mlflowpb.CreateClusterRequest); !ok || req.GetSpec().GetAdminPassword() != "" || strings.Contains(req.String(), adminPassword) {
		t.Errorf("the operation request holds the password: %v", op.Request)
	}
	if spec.GetAdminPassword() != adminPassword {
		t.Error("Create modified the caller's request")
	}
	if len(e.backend.ensured) != 1 || e.backend.ensured[0].AdminPassword != adminPassword || e.backend.ensured[0].AdminUsername != "admin" {
		t.Errorf("backend got %+v, want the password from the request", e.backend.ensured)
	}
	for _, in := range e.internal.m {
		if strings.Contains(in.Secret+in.AWSAccessKeyID+in.AccessKeyID+in.NetworkID, adminPassword) {
			t.Errorf("internals hold the password: %+v", in)
		}
	}
}

func TestCreateValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*mlflowpb.ClusterSpec)
		want   errs.Kind
		field  string
	}{
		{"no admin username", func(s *mlflowpb.ClusterSpec) { s.AdminUsername = "" }, errs.KindBadRequest, "spec.admin_username"},
		{"admin username too long", func(s *mlflowpb.ClusterSpec) { s.AdminUsername = strings.Repeat("a", 64) }, errs.KindBadRequest, "spec.admin_username"},
		{"no admin password", func(s *mlflowpb.ClusterSpec) { s.AdminPassword = "" }, errs.KindBadRequest, "spec.admin_password"},
		{"short admin password", func(s *mlflowpb.ClusterSpec) { s.AdminPassword = "twelve-chars" }, errs.KindBadRequest, "spec.admin_password"},
		{"no service account", func(s *mlflowpb.ClusterSpec) { s.ServiceAccountId = "" }, errs.KindBadRequest, "spec.service_account_id"},
		{"service account of the wrong kind", func(s *mlflowpb.ClusterSpec) { s.ServiceAccountId = "user-e00x" }, errs.KindBadRequest, "spec.service_account_id"},
		{"unknown service account", func(s *mlflowpb.ClusterSpec) { s.ServiceAccountId = "serviceaccount-e00nope" }, errs.KindNotFound, ""},
		{"service account of another project", func(s *mlflowpb.ClusterSpec) { s.ServiceAccountId = otherSAID }, errs.KindBadRequest, "spec.service_account_id"},
		{"unknown size", func(s *mlflowpb.ClusterSpec) { s.Size = "huge" }, errs.KindBadRequest, "spec.size"},
		{"missing bucket", func(s *mlflowpb.ClusterSpec) { s.StorageBucketName = "nope" }, errs.KindNotFound, ""},
		{"unknown network", func(s *mlflowpb.ClusterSpec) { s.NetworkId = "vpcnetwork-e00nope" }, errs.KindNotFound, ""},
		{"network of another project", func(s *mlflowpb.ClusterSpec) { s.NetworkId = otherNetwork }, errs.KindBadRequest, "spec.network_id"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			spec := validSpec()
			tt.mutate(spec)
			_, err := e.svc.Create(t.Context(), createCmd("ml", spec))
			if errs.KindOf(err) != tt.want {
				t.Fatalf("kind = %v, want %v (%v)", errs.KindOf(err), tt.want, err)
			}
			if tt.field != "" && !strings.Contains(err.Error(), tt.field) {
				t.Errorf("error %q does not name %s", err, tt.field)
			}
			if len(e.clusters.items) != 0 || len(e.peers.networks.bound) != 0 || len(e.backend.ensured) != 0 {
				t.Error("a rejected create left state behind")
			}
		})
	}
}

func TestCreateMetadataAndParentValidation(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	for name, tt := range map[string]struct {
		md   *commonpb.ResourceMetadata
		want errs.Kind
	}{
		"id set":               {&commonpb.ResourceMetadata{Id: "mlflow-x", ParentId: projectID, Name: "a"}, errs.KindBadRequest},
		"parent not a project": {&commonpb.ResourceMetadata{ParentId: "tenant-e00x", Name: "a"}, errs.KindBadRequest},
		"unknown project":      {&commonpb.ResourceMetadata{ParentId: "project-e00nope", Name: "a"}, errs.KindNotFound},
	} {
		_, err := e.svc.Create(t.Context(), domain.CreateClusterCmd{Metadata: tt.md, Spec: validSpec()})
		if errs.KindOf(err) != tt.want {
			t.Errorf("%s: kind %v, want %v (%v)", name, errs.KindOf(err), tt.want, err)
		}
	}
}

func TestCreateDuplicateName(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	first := e.create(t, "ml", validSpec())
	_, err := e.svc.Create(t.Context(), createCmd("ml", validSpec()))
	if !errs.IsKind(err, errs.KindAlreadyExists) {
		t.Fatalf("duplicate name: %v", err)
	}
	if n := len(e.peers.networks.bound); n != 1 {
		t.Errorf("%d network bindings after a rejected duplicate, want 1 (the first cluster's)", n)
	}
	if _, ok := e.peers.networks.bound[first.GetMetadata().GetId()]; !ok {
		t.Error("the first cluster lost its binding")
	}
}

func TestCreateNetwork(t *testing.T) {
	t.Parallel()
	t.Run("explicit", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t)
		spec := validSpec()
		spec.NetworkId = defaultNetwork
		if c := e.create(t, "ml", spec); c.GetSpec().GetNetworkId() != defaultNetwork {
			t.Errorf("network_id = %q", c.GetSpec().GetNetworkId())
		}
	})
	t.Run("no default network", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t)
		e.peers.networks.noDefult = true
		_, err := e.svc.Create(t.Context(), createCmd("ml", validSpec()))
		if !errs.IsKind(err, errs.KindBadRequest) || !strings.Contains(err.Error(), "spec.network_id") {
			t.Fatalf("err = %v, want BadRequest naming spec.network_id", err)
		}
	})
	t.Run("vpc disabled", func(t *testing.T) {
		t.Parallel()
		e := newEnvWith(t, func(p fakePeers) fakePeers { p.networks = nil; return p })
		_, err := e.svc.Create(t.Context(), createCmd("ml", validSpec()))
		if !errs.IsKind(err, errs.KindBadRequest) || !strings.Contains(err.Error(), "vpc") {
			t.Fatalf("err = %v, want BadRequest naming the vpc service", err)
		}
	})
}

func TestCreateSize(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	spec := validSpec()
	spec.Size = "large"
	c := e.create(t, "ml", spec)
	if c.GetSpec().GetSize() != "large" {
		t.Errorf("size = %q", c.GetSpec().GetSize())
	}
	got := e.backend.ensured[0]
	if got.CPUs != 16 || got.MemoryBytes != 64<<30 {
		t.Errorf("backend resources = %d CPUs, %d bytes; want 16 and 64 GiB", got.CPUs, got.MemoryBytes)
	}
}

func TestArtifactBucket(t *testing.T) {
	t.Parallel()
	t.Run("created for the cluster", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t)
		c := e.create(t, "ml", validSpec())
		want := c.GetMetadata().GetId()
		if len(e.peers.buckets.created) != 1 || e.peers.buckets.created[0] != want {
			t.Fatalf("created buckets = %v, want [%s]", e.peers.buckets.created, want)
		}
		if got := e.backend.ensured[0].Artifacts; got.Bucket != want || got.Region != "eu-north1" {
			t.Errorf("artifact store = %+v", got)
		}
		if c.GetSpec().GetStorageBucketName() != "" {
			t.Error("the auto-created bucket leaked into spec.storage_bucket_name")
		}
	})
	t.Run("named bucket is used, not created", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t)
		e.peers.buckets.add(projectID, "my-artifacts")
		spec := validSpec()
		spec.StorageBucketName = "my-artifacts"
		c := e.create(t, "ml", spec)
		if len(e.peers.buckets.created) != 0 {
			t.Errorf("created buckets %v although one was named", e.peers.buckets.created)
		}
		if c.GetSpec().GetStorageBucketName() != "my-artifacts" || c.GetStatus().GetEffectiveStorageBucketName() != "my-artifacts" ||
			e.backend.ensured[0].Artifacts.Bucket != "my-artifacts" {
			t.Errorf("bucket = spec %q, status %q, backend %q", c.GetSpec().GetStorageBucketName(),
				c.GetStatus().GetEffectiveStorageBucketName(), e.backend.ensured[0].Artifacts.Bucket)
		}
	})
	t.Run("existing own bucket is reused", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t)
		e.peers.buckets.racing = true // CreateBucket finds the name taken
		c := e.create(t, "ml", validSpec())
		if want := c.GetMetadata().GetId(); c.GetStatus().GetEffectiveStorageBucketName() != want {
			t.Errorf("effective bucket = %q, want %q", c.GetStatus().GetEffectiveStorageBucketName(), want)
		}
	})
	t.Run("storage disabled", func(t *testing.T) {
		t.Parallel()
		e := newEnvWith(t, func(p fakePeers) fakePeers { p.buckets = nil; return p })
		_, err := e.svc.Create(t.Context(), createCmd("ml", validSpec()))
		if !errs.IsKind(err, errs.KindBadRequest) || !strings.Contains(err.Error(), "storage") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestAccessKeyLifecycle(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := e.create(t, "ml", validSpec())
	id := c.GetMetadata().GetId()

	if len(e.peers.keys.specs) != 1 {
		t.Fatalf("%d keys created, want 1", len(e.peers.keys.specs))
	}
	spec := e.peers.keys.specs[0]
	if spec.ParentID != projectID || spec.Name != id || spec.AccountID != saID || !strings.Contains(spec.Description, "ml") {
		t.Errorf("key spec = %+v", spec)
	}
	art := e.backend.ensured[0].Artifacts
	if art.AccessKeyID != "NMKTEST1" || art.Secret != "secret1" {
		t.Errorf("backend credentials = %+v, want the created key's", art)
	}
	if in := e.internal.m[id]; in.AccessKeyID != "accesskey-e00k1" || in.AWSAccessKeyID != "NMKTEST1" || in.NetworkID != defaultNetwork {
		t.Errorf("internals = %+v", in)
	}

	// Stop and start reuse the key.
	e.op(t)(e.svc.Stop(t.Context(), id, nil))
	e.op(t)(e.svc.Start(t.Context(), id, nil))
	if len(e.peers.keys.specs) != 1 {
		t.Errorf("start created another key (%d keys)", len(e.peers.keys.specs))
	}
	if got := e.backend.ensured[1].Artifacts; got.AccessKeyID != "NMKTEST1" {
		t.Errorf("restart credentials = %+v", got)
	}

	e.op(t)(e.svc.Delete(t.Context(), id, nil))
	if len(e.peers.keys.live) != 0 {
		t.Errorf("keys left after delete: %v", e.peers.keys.live)
	}
}

func TestCreateFailures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		edit func(*env)
		want string
	}{
		{"backend", func(e *env) { e.backend.ensureErr = errs.Internal("docker is down") }, "docker is down"},
		{"access key", func(e *env) { e.peers.keys.failNew = errs.NotFound(saID) }, "artifact access key"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			tt.edit(e)
			op, err := e.svc.Create(t.Context(), createCmd("ml", validSpec()))
			if err != nil {
				t.Fatal(err)
			}
			if op.Err == nil || !strings.Contains(op.Err.Error(), tt.want) {
				t.Fatalf("operation error = %v, want %q", op.Err, tt.want)
			}
			c := e.get(t, op.ResourceID)
			if p, s := phaseState(c); p != mspv1alpha1pb.ClusterStatus_PHASE_PROVISIONING || s != mspv1alpha1pb.ClusterStatus_STATE_ERROR {
				t.Errorf("phase/state = %v/%v, want PROVISIONING/ERROR", p, s)
			}
			// The failed cluster can still be deleted.
			e.op(t)(e.svc.Delete(t.Context(), op.ResourceID, nil))
			if _, err := e.svc.Get(t.Context(), op.ResourceID); !errs.IsKind(err, errs.KindNotFound) {
				t.Errorf("after delete: %v", err)
			}
		})
	}
}

func TestPublicAccessEndpoint(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	spec := validSpec()
	spec.PublicAccess, spec.Description = true, "team tracking"
	c := e.create(t, "ml", spec)
	st := c.GetStatus()
	if st.GetTrackingEndpoints().GetPublic() == "" || st.GetTrackingEndpoint() != st.GetTrackingEndpoints().GetPublic() {
		t.Errorf("endpoints = %v / %q, want the public one", st.GetTrackingEndpoints(), st.GetTrackingEndpoint())
	}
	if !c.GetSpec().GetPublicAccess() || c.GetSpec().GetDescription() != "team tracking" {
		t.Errorf("spec = %v", c.GetSpec())
	}
}

func TestStopStartPhases(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	id := e.create(t, "ml", validSpec()).GetMetadata().GetId()

	if _, err := e.svc.Start(t.Context(), id, nil); !errs.IsKind(err, errs.KindBadState) || !strings.Contains(err.Error(), "ml") {
		t.Fatalf("Start on a running cluster: %v, want BadState naming the cluster", err)
	}
	e.op(t)(e.svc.Stop(t.Context(), id, nil))
	c := e.get(t, id)
	// There is no STOPPED phase: a stopped cluster is STOPPING and finished.
	if p, s := phaseState(c); p != mspv1alpha1pb.ClusterStatus_PHASE_STOPPING || s != mspv1alpha1pb.ClusterStatus_STATE_FINISHED {
		t.Fatalf("stopped phase/state = %v/%v", p, s)
	}
	if len(e.backend.stopped) != 1 || e.backend.stopped[0] != id {
		t.Errorf("backend stops = %v", e.backend.stopped)
	}
	if _, err := e.svc.Stop(t.Context(), id, nil); !errs.IsKind(err, errs.KindBadState) {
		t.Fatalf("Stop on a stopped cluster: %v, want BadState", err)
	}
	before := c.GetMetadata().GetResourceVersion()

	e.op(t)(e.svc.Start(t.Context(), id, nil))
	c = e.get(t, id)
	if p, s := phaseState(c); p != mspv1alpha1pb.ClusterStatus_PHASE_RUNNING || s != mspv1alpha1pb.ClusterStatus_STATE_FINISHED {
		t.Fatalf("started phase/state = %v/%v", p, s)
	}
	if c.GetMetadata().GetResourceVersion() <= before {
		t.Error("resource_version did not advance")
	}
	if got := e.backend.ensured[1]; got.AdminPassword != "" || got.ID != id {
		t.Errorf("restart backend call = %+v, want no password", got)
	}
	if c.GetStatus().GetTrackingEndpoints().GetPrivate() == "" {
		t.Error("restarted cluster has no endpoint")
	}
}

func TestStopStartFailures(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	id := e.create(t, "ml", validSpec()).GetMetadata().GetId()

	e.backend.stopErr = errors.New("cannot stop")
	op, err := e.svc.Stop(t.Context(), id, nil)
	if err != nil || op.Err == nil {
		t.Fatalf("Stop = %v, %v; want a failed operation", op, err)
	}
	if p, s := phaseState(e.get(t, id)); p != mspv1alpha1pb.ClusterStatus_PHASE_STOPPING || s != mspv1alpha1pb.ClusterStatus_STATE_ERROR {
		t.Fatalf("phase/state = %v/%v, want STOPPING/ERROR", p, s)
	}
	// A failed stop can be retried.
	e.backend.stopErr = nil
	e.op(t)(e.svc.Stop(t.Context(), id, nil))

	e.backend.ensureErr = errors.New("cannot start")
	if op, err := e.svc.Start(t.Context(), id, nil); err != nil || op.Err == nil {
		t.Fatalf("Start = %v, %v; want a failed operation", op, err)
	}
	if p, s := phaseState(e.get(t, id)); p != mspv1alpha1pb.ClusterStatus_PHASE_RESUMING || s != mspv1alpha1pb.ClusterStatus_STATE_ERROR {
		t.Fatalf("phase/state = %v/%v, want RESUMING/ERROR", p, s)
	}
	e.backend.ensureErr = nil
	e.op(t)(e.svc.Start(t.Context(), id, nil))
	if p, _ := phaseState(e.get(t, id)); p != mspv1alpha1pb.ClusterStatus_PHASE_RUNNING {
		t.Errorf("phase = %v after the retried start", p)
	}
}

func TestStopStartUnknownCluster(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if _, err := e.svc.Stop(t.Context(), "mlflow-e00nope", nil); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("Stop: %v", err)
	}
	if _, err := e.svc.Start(t.Context(), "mlflow-e00nope", nil); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("Start: %v", err)
	}
	if _, err := e.svc.Delete(t.Context(), "mlflow-e00nope", nil); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("Delete: %v", err)
	}
}

func TestDelete(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := e.create(t, "ml", validSpec())
	id := c.GetMetadata().GetId()
	bucket := c.GetStatus().GetEffectiveStorageBucketName()

	e.op(t)(e.svc.Delete(t.Context(), id, nil))
	if _, err := e.svc.Get(t.Context(), id); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("Get after delete: %v", err)
	}
	if len(e.backend.deleted) != 1 || e.backend.deleted[0] != id {
		t.Errorf("backend deletes = %v", e.backend.deleted)
	}
	if len(e.peers.networks.bound) != 0 {
		t.Errorf("network still bound: %v", e.peers.networks.bound)
	}
	if _, ok := e.internal.m[id]; ok {
		t.Error("internals were not removed")
	}
	if _, err := e.peers.buckets.BucketByName(t.Context(), projectID, bucket); err != nil {
		t.Errorf("the artifact bucket must stay: %v", err)
	}
	// The name is free again.
	e.create(t, "ml", validSpec())
}

func TestDeleteFailureKeepsTheCluster(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	id := e.create(t, "ml", validSpec()).GetMetadata().GetId()
	e.backend.deleteErr = errors.New("docker is down")
	op, err := e.svc.Delete(t.Context(), id, nil)
	if err != nil || op.Err == nil {
		t.Fatalf("Delete = %v, %v; want a failed operation", op, err)
	}
	if p, s := phaseState(e.get(t, id)); p != mspv1alpha1pb.ClusterStatus_PHASE_DELETING || s != mspv1alpha1pb.ClusterStatus_STATE_ERROR {
		t.Errorf("phase/state = %v/%v, want DELETING/ERROR", p, s)
	}
	e.backend.deleteErr = nil
	e.op(t)(e.svc.Delete(t.Context(), id, nil))
	if _, err := e.svc.Get(t.Context(), id); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("Get after the retried delete: %v", err)
	}
}

func TestExperimentsCountComesFromTheBackend(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := e.create(t, "ml", validSpec())
	id := c.GetMetadata().GetId()
	if c.GetStatus().GetExperimentsCount() != 0 {
		t.Fatalf("experiments_count = %d", c.GetStatus().GetExperimentsCount())
	}

	e.backend.experiments = 7
	if got := e.get(t, id).GetStatus().GetExperimentsCount(); got != 7 {
		t.Errorf("Get experiments_count = %d, want 7", got)
	}
	byName, err := e.svc.GetByName(t.Context(), projectID, "ml")
	if err != nil || byName.GetStatus().GetExperimentsCount() != 7 {
		t.Errorf("GetByName = %v, %v", byName, err)
	}
	page, err := e.svc.List(t.Context(), projectID, 0, "")
	if err != nil || len(page.Items) != 1 || page.Items[0].GetStatus().GetExperimentsCount() != 7 {
		t.Errorf("List = %v, %v", page, err)
	}

	// A backend error keeps the stored value instead of failing the read.
	e.backend.expErr = errors.New("unreachable")
	if got, err := e.svc.Get(t.Context(), id); err != nil || got.GetStatus().GetExperimentsCount() != 0 {
		t.Errorf("Get with a failing backend = %v, %v; want the stored count", got, err)
	}

	// Only running clusters are asked.
	e.backend.expErr = nil
	e.op(t)(e.svc.Stop(t.Context(), id, nil))
	if got := e.get(t, id).GetStatus().GetExperimentsCount(); got != 0 {
		t.Errorf("stopped cluster experiments_count = %d, want 0", got)
	}
}

func TestListAndGetByName(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	for _, n := range []string{"a", "b", "c"} {
		e.create(t, n, validSpec())
	}
	page, err := e.svc.List(t.Context(), projectID, 2, "")
	if err != nil || len(page.Items) != 2 || page.NextPageToken == "" || page.Items[0].GetMetadata().GetName() != "a" {
		t.Fatalf("first page = %v, %v", page, err)
	}
	page, err = e.svc.List(t.Context(), projectID, 2, page.NextPageToken)
	if err != nil || len(page.Items) != 1 || page.NextPageToken != "" || page.Items[0].GetMetadata().GetName() != "c" {
		t.Fatalf("second page = %v, %v", page, err)
	}
	if page, err := e.svc.List(t.Context(), otherProjectID, 0, ""); err != nil || len(page.Items) != 0 {
		t.Errorf("other project = %v, %v", page, err)
	}
	for _, size := range []int64{-1, 1001} {
		if _, err := e.svc.List(t.Context(), projectID, size, ""); !errs.IsKind(err, errs.KindBadRequest) {
			t.Errorf("page_size %d: %v", size, err)
		}
	}
	if _, err := e.svc.GetByName(t.Context(), projectID, "zzz"); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("GetByName(missing) = %v", err)
	}
	if got, err := e.svc.GetByName(t.Context(), projectID, "b"); err != nil || got.GetMetadata().GetName() != "b" {
		t.Errorf("GetByName(b) = %v, %v", got, err)
	}
}

func TestNewRejectsBadDeps(t *testing.T) {
	t.Parallel()
	if _, err := domain.New(domain.Deps{}); err == nil {
		t.Error("New accepted empty deps")
	}
	e := newEnv(t)
	_, err := domain.New(domain.Deps{
		Clusters: e.clusters, Internals: e.internal, Backend: e.backend, Peers: e.peers, Hierarchy: fakeHierarchy{},
		Ops: inlineOps{}, IDs: &seqIDs{}, Clock: e.clk,
		Config: domain.Config{SizeOrder: []string{"medium"}, Sizes: map[string]string{"medium": "bogus"}},
	})
	if err == nil {
		t.Error("New accepted a size without resources")
	}
}
