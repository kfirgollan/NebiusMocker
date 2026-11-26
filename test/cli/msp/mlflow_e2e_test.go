//go:build e2e

package msp

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	iamv2pb "github.com/nebius/gosdk/proto/nebius/iam/v2"
	mlflowpb "github.com/nebius/gosdk/proto/nebius/msp/mlflow/v1alpha1"
	mspv1alpha1pb "github.com/nebius/gosdk/proto/nebius/msp/v1alpha1"
	iamv2svc "github.com/nebius/gosdk/services/nebius/iam/v2"
	mlflowsvc "github.com/nebius/gosdk/services/nebius/msp/mlflow/v1alpha1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore/memory"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/services/mspmlflow"
	"github.com/kfirgollan/NebiusMocker/services/storage"
	"github.com/kfirgollan/NebiusMocker/services/vpc"
	"github.com/kfirgollan/NebiusMocker/test/internal/nebiuscli"
)

const mlflowPassword = "mlflow-Secret-pw-1234"

type mlflowCluster struct {
	Metadata struct {
		ID       string `json:"id"`
		ParentID string `json:"parent_id"`
		Name     string `json:"name"`
	} `json:"metadata"`
	Spec   map[string]any `json:"spec"`
	Status struct {
		Phase                      string `json:"phase"`
		State                      string `json:"state"`
		TrackingEndpoint           string `json:"tracking_endpoint"`
		EffectiveStorageBucketName string `json:"effective_storage_bucket_name"`
		MlflowVersion              string `json:"mlflow_version"`
	} `json:"status"`
}

func TestMLflowClusterFlows(t *testing.T) {
	inst := testkit.StartT(t, []mocker.Module{iam.New(), vpc.New(), storage.New(storage.WithS3Listen("127.0.0.1:0")), mspmlflow.New()},
		testkit.WithObjStore(memory.New(clock.Real{})), testkit.WithOperationDelay(100*time.Millisecond),
		testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce))
	cli := nebiuscli.New(t, nebiuscli.Profile{Addr: inst.Addr, Token: inst.Token, ParentID: inst.ProjectID, TenantID: inst.TenantID})
	sdk := inst.SDK(t)
	clusters := mlflowsvc.NewClusterService(sdk)
	keys := iamv2svc.NewAccessKeyService(sdk)
	ctx := t.Context()

	var net struct {
		Metadata struct {
			ID string `json:"id"`
		} `json:"metadata"`
	}
	cli.JSON(t, &net, "vpc", "network", "get-by-name", "--name", "default-network")
	var sa struct {
		Metadata struct {
			ID string `json:"id"`
		} `json:"metadata"`
	}
	cli.JSON(t, &sa, "iam", "service-account", "create", "--parent-id", inst.ProjectID, "--name", "mlflow-sa")
	if sa.Metadata.ID == "" {
		t.Fatal("no service account")
	}
	// The artifacts bucket, created the way a user does.
	cli.JSON(t, &struct{}{}, "storage", "bucket", "create", "--parent-id", inst.ProjectID, "--name", "cli-artifacts")

	var c mlflowCluster
	cli.JSON(t, &c, "msp", "mlflow", "v1alpha1", "cluster", "create",
		"--parent-id", inst.ProjectID, "--name", "cli-mlflow", "--description", "from the cli", "--network-id", net.Metadata.ID,
		"--size", "medium", "--public-access=false", "--admin-username", "admin", "--admin-password", mlflowPassword,
		"--service-account-id", sa.Metadata.ID, "--storage-bucket-name", "cli-artifacts")
	id := c.Metadata.ID
	if id == "" || c.Metadata.Name != "cli-mlflow" || c.Metadata.ParentID != inst.ProjectID {
		t.Fatalf("create = %+v", c)
	}

	sdkCheck := func(step string, wantPhase mspv1alpha1pb.ClusterStatus_Phase) *mlflowpb.Cluster {
		t.Helper()
		got, err := clusters.Get(ctx, &mlflowpb.GetClusterRequest{Id: id})
		if err != nil {
			t.Fatalf("%s: sdk get: %v", step, err)
		}
		sp := got.GetSpec()
		if got.GetStatus().GetPhase() != wantPhase || sp.GetAdminPassword() != "" || sp.GetAdminUsername() != "admin" || sp.GetPublicAccess() ||
			sp.GetServiceAccountId() != sa.Metadata.ID || sp.GetStorageBucketName() != "cli-artifacts" || sp.GetDescription() != "from the cli" ||
			sp.GetSize() != "medium" || sp.GetNetworkId() != net.Metadata.ID {
			t.Errorf("%s: sdk cluster = %v, want phase %v", step, got, wantPhase)
		}
		return got
	}
	sdkCheck("create", mspv1alpha1pb.ClusterStatus_PHASE_RUNNING)

	var got mlflowCluster
	cli.JSON(t, &got, "msp", "mlflow", "v1alpha1", "cluster", "get", id)
	if got.Status.Phase != "PHASE_RUNNING" || got.Status.State != "STATE_FINISHED" || got.Status.EffectiveStorageBucketName != "cli-artifacts" ||
		got.Status.MlflowVersion == "" || !strings.HasPrefix(got.Status.TrackingEndpoint, id+".") {
		t.Errorf("get status = %+v", got.Status)
	}
	if _, leaked := got.Spec["admin_password"]; leaked {
		t.Errorf("cli output carries the admin password: %v", got.Spec)
	}
	if k, err := keys.List(ctx, &iamv2pb.ListAccessKeysRequest{ParentId: inst.ProjectID}); err != nil || len(k.GetItems()) != 1 {
		t.Errorf("access keys while the cluster lives = %v, %v", k, err)
	}

	// get-by-name with a jsonpath, as a script would read the endpoint.
	res, err := cli.Run(ctx, "msp", "mlflow", "v1alpha1", "cluster", "get-by-name", "--parent-id", inst.ProjectID, "--name", "cli-mlflow",
		"--format", "jsonpath={.status.tracking_endpoint}")
	if err != nil {
		t.Fatalf("get-by-name: %v", err)
	}
	if ep := strings.TrimSpace(res.Stdout); ep != got.Status.TrackingEndpoint {
		t.Errorf("jsonpath tracking_endpoint = %q, want %q", ep, got.Status.TrackingEndpoint)
	}

	var list struct {
		Clusters []mlflowCluster `json:"items"`
	}
	cli.JSON(t, &list, "msp", "mlflow", "v1alpha1", "cluster", "list", "--parent-id", inst.ProjectID)
	if len(list.Clusters) != 1 || list.Clusters[0].Metadata.ID != id {
		t.Fatalf("list = %+v", list)
	}

	// stop / start
	cli.JSON(t, &got, "msp", "mlflow", "v1alpha1", "cluster", "stop", id)
	cli.JSON(t, &got, "msp", "mlflow", "v1alpha1", "cluster", "get", id)
	if got.Status.Phase != "PHASE_STOPPING" || got.Status.State != "STATE_FINISHED" {
		t.Errorf("after stop: %+v", got.Status)
	}
	sdkCheck("stop", mspv1alpha1pb.ClusterStatus_PHASE_STOPPING)
	cli.JSON(t, &got, "msp", "mlflow", "v1alpha1", "cluster", "start", id)
	cli.JSON(t, &got, "msp", "mlflow", "v1alpha1", "cluster", "get", id)
	if got.Status.Phase != "PHASE_RUNNING" {
		t.Errorf("after start: %+v", got.Status)
	}
	sdkCheck("start", mspv1alpha1pb.ClusterStatus_PHASE_RUNNING)

	// Refusals: duplicate name, unknown bucket.
	for want, extra := range map[string][]string{
		"AlreadyExists": {"--name", "cli-mlflow"},
		"no-such":       {"--name", "other", "--storage-bucket-name", "no-such-bucket"},
	} {
		args := append([]string{
			"msp", "mlflow", "v1alpha1", "cluster", "create", "--parent-id", inst.ProjectID, "--admin-username", "admin",
			"--admin-password", mlflowPassword, "--service-account-id", sa.Metadata.ID,
		}, extra...)
		if _, err := cli.Run(ctx, args...); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("create %v: err = %v, want one mentioning %q", extra, err, want)
		}
	}

	cli.JSON(t, &struct{}{}, "msp", "mlflow", "v1alpha1", "cluster", "delete", id)
	if _, err := clusters.Get(ctx, &mlflowpb.GetClusterRequest{Id: id}); status.Code(err) != codes.NotFound {
		t.Errorf("sdk get after delete: err = %v, want NotFound", err)
	}
	if k, err := keys.List(ctx, &iamv2pb.ListAccessKeysRequest{ParentId: inst.ProjectID}); err != nil || len(k.GetItems()) != 0 {
		t.Errorf("access keys after delete = %v, %v", k, err)
	}
	list.Clusters = nil
	cli.JSON(t, &list, "msp", "mlflow", "v1alpha1", "cluster", "list", "--parent-id", inst.ProjectID)
	if len(list.Clusters) != 0 {
		t.Errorf("list after delete = %+v", list)
	}
}
