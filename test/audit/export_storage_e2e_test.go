//go:build e2e_storage

package audit_test

import (
	"crypto/rand"
	"encoding/hex"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/nebius/gosdk"
	"github.com/nebius/gosdk/operations"
	auditv2pb "github.com/nebius/gosdk/proto/nebius/audit/v2"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"
	iamv2pb "github.com/nebius/gosdk/proto/nebius/iam/v2"
	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"
	auditsvc "github.com/nebius/gosdk/services/nebius/audit/v2"
	iamsvc "github.com/nebius/gosdk/services/nebius/iam/v1"
	iamv2svc "github.com/nebius/gosdk/services/nebius/iam/v2"
	storagesvc "github.com/nebius/gosdk/services/nebius/storage/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	auditmod "github.com/kfirgollan/NebiusMocker/services/audit"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/services/storage"
	"github.com/kfirgollan/NebiusMocker/test/rustfsfixture"
)

func TestMain(m *testing.M) { os.Exit(rustfsfixture.Main(m)) }

func uniqueName(prefix string) string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return prefix + "-" + hex.EncodeToString(b)
}

// waitFor returns a function that waits for a started operation to succeed:
// waitFor(t)(svc.Create(ctx, req)).
func waitFor(t *testing.T) func(operations.Operation, error) operations.Operation {
	t.Helper()
	return func(op operations.Operation, err error) operations.Operation {
		t.Helper()
		if err != nil {
			t.Fatalf("start operation: %v", err)
		}
		done, err := op.Wait(t.Context())
		if err != nil {
			t.Fatalf("operation %s: %v", op.ID(), err)
		}
		return done
	}
}

// s3Key creates a service account with the editor role on the project and an
// access key for it, all through the Nebius Go SDK.
func s3Key(t *testing.T, sdk *gosdk.SDK, tenantID, projectID string) (keyID, secret string) {
	t.Helper()
	ctx := t.Context()
	wait := waitFor(t)
	sa := wait(iamsvc.NewServiceAccountService(sdk).Create(ctx, &iampb.CreateServiceAccountRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: projectID, Name: uniqueName("sa")}, Spec: &iampb.ServiceAccountSpec{},
	})).ResourceID()
	group := wait(iamsvc.NewGroupService(sdk).Create(ctx, &iampb.CreateGroupRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: tenantID, Name: uniqueName("g")}, Spec: &iampb.GroupSpec{},
	})).ResourceID()
	wait(iamsvc.NewGroupMembershipService(sdk).Create(ctx, &iampb.CreateGroupMembershipRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: group}, Spec: &iampb.GroupMembershipSpec{MemberId: sa},
	}))
	wait(iamsvc.NewAccessPermitService(sdk).Create(ctx, &iampb.CreateAccessPermitRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: group},
		Spec:     &iampb.AccessPermitSpec{ResourceId: projectID, Role: "editor"},
	}))
	keys := iamv2svc.NewAccessKeyService(sdk)
	id := wait(keys.Create(ctx, &iamv2pb.CreateAccessKeyRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: projectID},
		Spec: &iamv2pb.AccessKeySpec{
			Account:            &iampb.Account{Type: &iampb.Account_ServiceAccount_{ServiceAccount: &iampb.Account_ServiceAccount{Id: sa}}},
			SecretDeliveryMode: iamv2pb.SecretDeliveryMode_EXPLICIT,
		},
	})).ResourceID()
	sec, err := keys.GetSecret(ctx, &iamv2pb.GetAccessKeySecretRequest{Id: id})
	if err != nil {
		t.Fatalf("GetSecret: %v", err)
	}
	return sec.GetAwsAccessKeyId(), sec.GetSecret()
}

// TestAuditExportToBucketViaS3Front is the documented user path: export audit
// events into an Object Storage bucket, then download them with an S3 client.
func TestAuditExportToBucketViaS3Front(t *testing.T) {
	ctx := t.Context()
	wait := waitFor(t)
	srv := rustfsfixture.Start(t)
	storageMod := storage.New(storage.WithS3Listen("127.0.0.1:0"))
	inst := testkit.StartT(t, []mocker.Module{iam.New(), storageMod, auditmod.New()},
		testkit.WithObjStore(srv.Client(t)), testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce),
		testkit.WithOperationDelay(50*time.Millisecond))
	sdk := inst.SDK(t)

	// A bucket in the project, created through the Nebius Go SDK.
	bucketName := uniqueName("audit-bucket")
	bucketID := wait(storagesvc.NewBucketService(sdk).Create(ctx, &storagepb.CreateBucketRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: inst.ProjectID, Name: bucketName},
		Spec:     &storagepb.BucketSpec{},
	})).ResourceID()

	// S3 client with an access key, set up before the audited activity.
	keyID, secret := s3Key(t, sdk, inst.TenantID, inst.ProjectID)
	s3c := s3.New(s3.Options{
		Region:       "eu-north1",
		BaseEndpoint: aws.String("http://" + storageMod.S3Addr()),
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider(keyID, secret, ""),
	})

	// Audited activity: create and update a group.
	groups := iamsvc.NewGroupService(sdk)
	gName := uniqueName("audited")
	gID := wait(groups.Create(ctx, &iampb.CreateGroupRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: inst.TenantID, Name: gName}, Spec: &iampb.GroupSpec{},
	})).ResourceID()
	wait(groups.Update(ctx, &iampb.UpdateGroupRequest{
		Metadata: &commonpb.ResourceMetadata{Id: gID, ParentId: inst.TenantID, Name: gName, Labels: map[string]string{"k": "v"}},
		Spec:     &iampb.GroupSpec{},
	}))

	const filter = "resource.metadata.type='group'"
	now := clock.Real{}.Now()
	from, to := timestamppb.New(now.Add(-time.Hour)), timestamppb.New(now.Add(time.Hour))

	// What AuditEventService.List says, to compare the files against.
	wantIDs := map[string]bool{}
	for ev, err := range auditsvc.NewAuditEventService(sdk).Filter(ctx, &auditv2pb.ListAuditEventRequest{
		ParentId: inst.TenantID, Start: from, End: to, Filter: filter,
		EventType: auditv2pb.EventType_CONTROL_PLANE, Region: "eu-north1",
	}) {
		if err != nil {
			t.Fatalf("list audit events: %v", err)
		}
		wantIDs[ev.GetId()] = true
	}
	if len(wantIDs) < 2 {
		t.Fatalf("audit events for groups = %d, want at least the create and update", len(wantIDs))
	}

	exports := auditsvc.NewAuditEventExportService(sdk)
	export := func(objectPrefix string) string {
		t.Helper()
		op := wait(exports.Start(ctx, &auditv2pb.StartRequest{
			Metadata: &commonpb.ResourceMetadata{ParentId: inst.TenantID},
			Region:   "eu-north1",
			Spec: &auditv2pb.AuditEventExportSpec{
				Params: &auditv2pb.AuditEventExportParams{From: from, To: to, Filter: filter, EventType: auditv2pb.EventType_CONTROL_PLANE},
				ExportDestination: &auditv2pb.AuditEventExportSpec_NebiusObjectStorage{
					NebiusObjectStorage: &auditv2pb.NebiusObjectStorageDestination{
						ObjectPrefix: objectPrefix,
						Bucket:       &auditv2pb.NebiusObjectStorageDestination_BucketById{BucketById: &auditv2pb.BucketById{Id: bucketID}},
					},
				},
			},
		}))
		exp, err := exports.Get(ctx, &auditv2pb.GetRequest{Id: op.ResourceID()})
		if err != nil {
			t.Fatalf("get export: %v", err)
		}
		if exp.GetStatus().GetState() != auditv2pb.AuditEventExportState_AUDIT_EVENT_EXPORT_STATE_DONE {
			t.Fatalf("export state = %v, want DONE; status: %v", exp.GetStatus().GetState(), exp.GetStatus())
		}
		return exp.GetStatus().GetExportOperationId()
	}

	// download is "aws s3 cp --recursive s3://<bucket>/<prefix> ." through the S3 front.
	download := func(prefix string) map[string][]byte {
		t.Helper()
		files := map[string][]byte{}
		list, err := s3c.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(bucketName), Prefix: aws.String(prefix)})
		if err != nil {
			t.Fatalf("ListObjectsV2 %s: %v", prefix, err)
		}
		for _, o := range list.Contents {
			out, err := s3c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucketName), Key: o.Key})
			if err != nil {
				t.Fatalf("GetObject %s: %v", aws.ToString(o.Key), err)
			}
			b, err := io.ReadAll(out.Body)
			_ = out.Body.Close()
			if err != nil {
				t.Fatalf("read %s: %v", aws.ToString(o.Key), err)
			}
			files[aws.ToString(o.Key)] = b
		}
		if len(files) == 0 {
			t.Fatalf("no objects under s3://%s/%s", bucketName, prefix)
		}
		return files
	}

	opID := export("")
	gotIDs := map[string]bool{}
	for key, b := range download("auditlogs/" + opID + "/") {
		if !strings.HasSuffix(key, ".jsonl") {
			t.Errorf("object %q is not a .jsonl file", key)
		}
		if strings.Contains(string(b), "nmock-dev-admin") || strings.Contains(string(b), inst.Token) {
			t.Errorf("object %s contains the dev token", key)
		}
		for line := range strings.SplitSeq(strings.TrimSpace(string(b)), "\n") {
			var ev auditv2pb.AuditEvent
			if err := protojson.Unmarshal([]byte(line), &ev); err != nil {
				t.Fatalf("%s: decode line %q: %v", key, line, err)
			}
			if !strings.HasPrefix(ev.GetType(), "ai.nebius.iam.group.") {
				t.Errorf("event %s type = %q, want ai.nebius.iam.group.*", ev.GetId(), ev.GetType())
			}
			gotIDs[ev.GetId()] = true
		}
	}
	if !maps.Equal(gotIDs, wantIDs) {
		t.Errorf("exported event ids = %v, want the AuditEventService.List ids %v", slices.Sorted(maps.Keys(gotIDs)), slices.Sorted(maps.Keys(wantIDs)))
	}

	// A custom object_prefix is honoured.
	custom := export("custom/logs")
	for key := range download("custom/logs/" + custom + "/") {
		if !strings.HasPrefix(key, "custom/logs/"+custom+"/audit-events-") {
			t.Errorf("custom export key %q not under custom/logs/%s/", key, custom)
		}
	}
}
