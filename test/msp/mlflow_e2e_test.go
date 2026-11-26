//go:build e2e_msp

package msp

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"
	iamv2pb "github.com/nebius/gosdk/proto/nebius/iam/v2"
	mlflowpb "github.com/nebius/gosdk/proto/nebius/msp/mlflow/v1alpha1"
	mspv1alpha1pb "github.com/nebius/gosdk/proto/nebius/msp/v1alpha1"
	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"
	iamv2svc "github.com/nebius/gosdk/services/nebius/iam/v2"
	mlflowsvc "github.com/nebius/gosdk/services/nebius/msp/mlflow/v1alpha1"
	storagesvc "github.com/nebius/gosdk/services/nebius/storage/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/test/internal/awscli"
	"github.com/kfirgollan/NebiusMocker/test/internal/tfharness"
)

const (
	mlAdminUser = "admin"
	mlAdminPass = "Adm1n-password-123"
	mlBucket    = "mlflow-artifacts"
	noteText    = "hello from the e2e artifact\n"
)

// mlflowHCL is what the provider documents for Managed MLflow: a service
// account (granted access by joining the project's editors group), a bucket
// for the artifacts, and the cluster using both. The second cluster lets the
// service create its own bucket.
func mlflowHCL(project, tenant string) string {
	return fmt.Sprintf(`
data "nebius_vpc_v1_network" "default" {
  parent_id = %[1]q
  name      = "default-network"
}

data "nebius_iam_v1_group" "editors" {
  parent_id = %[2]q
  name      = "editors"
}

resource "nebius_iam_v1_service_account" "mlflow" {
  parent_id = %[1]q
  name      = "mlflow-sa"
}

resource "nebius_iam_v1_group_membership" "mlflow" {
  parent_id = data.nebius_iam_v1_group.editors.id
  member_id = nebius_iam_v1_service_account.mlflow.id
}

resource "nebius_storage_v1_bucket" "artifacts" {
  parent_id = %[1]q
  name      = %[3]q
}

resource "nebius_msp_mlflow_v1alpha1_cluster" "ml" {
  parent_id           = %[1]q
  name                = "e2e-mlflow"
  network_id          = data.nebius_vpc_v1_network.default.id
  service_account_id  = nebius_iam_v1_service_account.mlflow.id
  storage_bucket_name = nebius_storage_v1_bucket.artifacts.name
  admin_username      = %[4]q
  admin_password      = %[5]q
  depends_on          = [nebius_iam_v1_group_membership.mlflow]
}

resource "nebius_msp_mlflow_v1alpha1_cluster" "managed_bucket" {
  parent_id          = %[1]q
  name               = "e2e-mlflow-own-bucket"
  network_id         = data.nebius_vpc_v1_network.default.id
  service_account_id = nebius_iam_v1_service_account.mlflow.id
  admin_username     = %[4]q
  admin_password     = %[5]q
  depends_on         = [nebius_iam_v1_group_membership.mlflow]
}

output "tracking_endpoint" { value = nebius_msp_mlflow_v1alpha1_cluster.ml.status.tracking_endpoint }
output "own_bucket" { value = nebius_msp_mlflow_v1alpha1_cluster.managed_bucket.status.effective_storage_bucket_name }
`, project, tenant, mlBucket, mlAdminUser, mlAdminPass)
}

// trainScript is the user's code: the MLflow Python client, as in the docs.
const trainScript = `
import mlflow
from mlflow.tracking import MlflowClient

mlflow.set_experiment("e2e-experiment")
with mlflow.start_run() as run:
    mlflow.log_param("learning_rate", "0.1")
    mlflow.log_metric("loss", 0.25)
    mlflow.log_artifact("/work/note.txt")
    print("RUN_ID=" + run.info.run_id)
    print("EXPERIMENT_ID=" + run.info.experiment_id)

back = MlflowClient().get_run(run.info.run_id)
print("PARAM=" + back.data.params["learning_rate"])
print("METRIC=" + str(back.data.metrics["loss"]))
`

func TestMLflowScenario(t *testing.T) {
	e := startEnv(t)
	tf := tfharness.New(t, e.addr, e.inst.Token)
	tf.UseTLSWithoutVerify()
	tf.Write(t, mlflowHCL(e.inst.ProjectID, e.inst.TenantID))
	tf.Init(t)
	tf.ExpectPlan(t, map[string]string{
		"nebius_iam_v1_service_account.mlflow": "create", "nebius_iam_v1_group_membership.mlflow": "create",
		"nebius_storage_v1_bucket.artifacts": "create", "nebius_msp_mlflow_v1alpha1_cluster.ml": "create",
		"nebius_msp_mlflow_v1alpha1_cluster.managed_bucket": "create",
	})
	tf.Apply(t)
	tf.ExpectNoChanges(t)

	st := tf.State(t)
	clusterID := st["nebius_msp_mlflow_v1alpha1_cluster.ml"]["id"].(string)
	saID := st["nebius_iam_v1_service_account.mlflow"]["id"].(string)
	endpoint := strings.TrimPrefix(tf.Output(t, "tracking_endpoint"), "https://")
	if !strings.HasPrefix(endpoint, "host.docker.internal:") {
		t.Fatalf("tracking endpoint = %q, want a host.docker.internal address", endpoint)
	}

	// The user's machine: the official MLflow image, which has the Python
	// client, trusting the mocker's CA through MLFLOW_TRACKING_SERVER_CERT_PATH.
	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "note.txt"), []byte(noteText), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "train.py"), []byte(trainScript), 0o644); err != nil {
		t.Fatal(err)
	}
	u := startMachine(t, e.d, machineOpts{image: config.Default().MSP.MLflowImage, mounts: []string{
		filepath.Dir(e.mspCA) + ":/msp-ca:ro", work + ":/work:ro",
	}})
	out := u.sh(t, fmt.Sprintf("MLFLOW_TRACKING_URI=https://%s MLFLOW_TRACKING_USERNAME=%s MLFLOW_TRACKING_PASSWORD=%s "+
		"MLFLOW_TRACKING_SERVER_CERT_PATH=/msp-ca/ca.pem python /work/train.py", endpoint, mlAdminUser, mlAdminPass))
	vars := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			vars[k] = v
		}
	}
	runID, expID := vars["RUN_ID"], vars["EXPERIMENT_ID"]
	if runID == "" || vars["PARAM"] != "0.1" || vars["METRIC"] != "0.25" {
		t.Fatalf("the MLflow client did not read its run back: %v\n%s", vars, out)
	}

	// The artifact is in the Nebius bucket, read the way a user does: an
	// access key and the AWS CLI against the S3 front.
	mlflowSvc := mlflowsvc.NewClusterService(e.sdk)
	cluster, err := mlflowSvc.Get(t.Context(), &mlflowpb.GetClusterRequest{Id: clusterID})
	if err != nil {
		t.Fatalf("Cluster.Get: %v", err)
	}
	if cluster.GetStatus().GetPhase() != mspv1alpha1pb.ClusterStatus_PHASE_RUNNING || cluster.GetStatus().GetEffectiveStorageBucketName() != mlBucket {
		t.Fatalf("cluster status = %v", cluster.GetStatus())
	}
	keys := iamv2svc.NewAccessKeyService(e.sdk)
	keyOp, err := keys.Create(t.Context(), &iamv2pb.CreateAccessKeyRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: e.inst.ProjectID, Name: "e2e-mlflow-key"},
		Spec: &iamv2pb.AccessKeySpec{
			Account:            &iampb.Account{Type: &iampb.Account_ServiceAccount_{ServiceAccount: &iampb.Account_ServiceAccount{Id: saID}}},
			SecretDeliveryMode: iamv2pb.SecretDeliveryMode_EXPLICIT,
		},
	})
	if err != nil {
		t.Fatalf("AccessKey.Create: %v", err)
	}
	if keyOp, err = keyOp.Wait(t.Context()); err != nil {
		t.Fatalf("AccessKey.Create wait: %v", err)
	}
	secret, err := keys.GetSecret(t.Context(), &iamv2pb.GetAccessKeySecretRequest{Id: keyOp.ResourceID()})
	if err != nil {
		t.Fatalf("AccessKey.GetSecret: %v", err)
	}
	aws := awscli.New(t)
	aws.Configure(t, secret.GetAwsAccessKeyId(), secret.GetSecret(), "eu-north1", "http://"+e.s3)
	listing, err := aws.Run(t.Context(), "s3", "ls", "s3://"+mlBucket+"/", "--recursive")
	if err != nil {
		t.Fatalf("aws s3 ls: %v", err)
	}
	var key string
	for _, line := range strings.Split(listing, "\n") {
		if f := strings.Fields(line); len(f) == 4 && strings.HasSuffix(f[3], "/artifacts/note.txt") && strings.Contains(f[3], runID) {
			key = f[3]
		}
	}
	if key == "" {
		t.Fatalf("the run's artifact is not in bucket %s:\n%s", mlBucket, listing)
	}
	got, err := aws.Run(t.Context(), "s3", "cp", "s3://"+mlBucket+"/"+key, "-")
	if err != nil || got != noteText {
		t.Fatalf("artifact content = %q, %v; want %q", got, err, noteText)
	}
	t.Logf("artifact s3://%s/%s (experiment %s)", mlBucket, key, expID)

	// status.experiments_count follows the server.
	eventually(t, 2*time.Minute, func() error {
		c, err := mlflowSvc.Get(t.Context(), &mlflowpb.GetClusterRequest{Id: clusterID})
		if err != nil {
			return err
		}
		if n := c.GetStatus().GetExperimentsCount(); n < 1 {
			return fmt.Errorf("experiments_count = %d, want >= 1", n)
		}
		return nil
	})

	// The second cluster got a bucket of its own.
	own := tf.Output(t, "own_bucket")
	if own == "" || own == mlBucket {
		t.Fatalf("own bucket = %q", own)
	}

	// Destroy: the servers go, the artifacts stay in their buckets.
	if _, err := aws.Run(t.Context(), "s3", "rm", "s3://"+mlBucket+"/", "--recursive"); err != nil {
		t.Fatalf("aws s3 rm: %v", err)
	}
	tf.Destroy(t)
	if left := e.d.leftovers(t, "nebius-mocker.backend=msp-mlflow"); len(left) > 0 {
		t.Fatalf("containers left after the clusters were destroyed: %v", left)
	}
	buckets, err := storagesvc.NewBucketService(e.sdk).List(t.Context(), &storagepb.ListBucketsRequest{ParentId: e.inst.ProjectID})
	if err != nil {
		t.Fatalf("Bucket.List: %v", err)
	}
	var found bool
	for _, b := range buckets.GetItems() {
		found = found || b.GetMetadata().GetName() == own
	}
	if !found {
		t.Errorf("bucket %s of the deleted cluster is gone; the docs say it stays", own)
	}
}
