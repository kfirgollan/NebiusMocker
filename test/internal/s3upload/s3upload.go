// Package s3upload puts files into Nebius Object Storage the way the Nebius
// docs tell users to: a bucket created through BucketService, an access key of
// a service account (iam.v2 AccessKeyService), and the AWS SDK for Go v2
// talking to the mocker's S3 front. End-to-end suites use it instead of
// writing into the object store directly.
package s3upload

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/nebius/gosdk"
	"github.com/nebius/gosdk/operations"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"
	iamv2pb "github.com/nebius/gosdk/proto/nebius/iam/v2"
	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"
	iamsvc "github.com/nebius/gosdk/services/nebius/iam/v1"
	iamv2svc "github.com/nebius/gosdk/services/nebius/iam/v2"
	storagesvc "github.com/nebius/gosdk/services/nebius/storage/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
)

// Region is the region the S3 clients sign for (the seeded project's).
const Region = "eu-north1"

// Unique returns prefix plus a random suffix, for bucket and account names.
func Unique(prefix string) string {
	b := make([]byte, 4)
	_, _ = rand.Read(b) // crypto/rand.Read never fails on supported platforms
	return prefix + hex.EncodeToString(b)
}

// wait returns a function that waits for a started operation to succeed:
// wait(t)(svc.Create(ctx, req)).
func wait(t *testing.T) func(operations.Operation, error) operations.Operation {
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

// AccessKey creates a service account in the project, adds it to a group
// holding role on the project, creates an access key for it and returns the
// AWS-style key ID and secret, all through the official Nebius Go SDK.
func AccessKey(t *testing.T, sdk *gosdk.SDK, inst *testkit.Instance, role string) (keyID, secret string) {
	t.Helper()
	ctx := t.Context()
	sa := wait(t)(iamsvc.NewServiceAccountService(sdk).Create(ctx, &iampb.CreateServiceAccountRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: inst.ProjectID, Name: Unique("images-sa-")},
		Spec:     &iampb.ServiceAccountSpec{},
	})).ResourceID()
	group := wait(t)(iamsvc.NewGroupService(sdk).Create(ctx, &iampb.CreateGroupRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: inst.TenantID, Name: Unique("images-")}, Spec: &iampb.GroupSpec{},
	})).ResourceID()
	wait(t)(iamsvc.NewGroupMembershipService(sdk).Create(ctx, &iampb.CreateGroupMembershipRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: group}, Spec: &iampb.GroupMembershipSpec{MemberId: sa},
	}))
	wait(t)(iamsvc.NewAccessPermitService(sdk).Create(ctx, &iampb.CreateAccessPermitRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: group},
		Spec:     &iampb.AccessPermitSpec{ResourceId: inst.ProjectID, Role: role},
	}))
	keys := iamv2svc.NewAccessKeyService(sdk)
	id := wait(t)(keys.Create(ctx, &iamv2pb.CreateAccessKeyRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: inst.ProjectID},
		Spec: &iamv2pb.AccessKeySpec{
			Account:            &iampb.Account{Type: &iampb.Account_ServiceAccount_{ServiceAccount: &iampb.Account_ServiceAccount{Id: sa}}},
			SecretDeliveryMode: iamv2pb.SecretDeliveryMode_EXPLICIT,
		},
	})).ResourceID()
	sec, err := keys.GetSecret(ctx, &iamv2pb.GetAccessKeySecretRequest{Id: id})
	if err != nil {
		t.Fatalf("GetSecret(%s): %v", id, err)
	}
	return sec.GetAwsAccessKeyId(), sec.GetSecret()
}

// Client is the AWS SDK for Go v2 S3 client configured as Nebius documents
// (endpoint, region, access key), pointed at the mocker's S3 front.
func Client(s3Addr, keyID, secret string) *s3.Client {
	return s3.New(s3.Options{
		Region:       Region,
		BaseEndpoint: aws.String("http://" + s3Addr),
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider(keyID, secret, ""),
	})
}

// Uploader creates buckets with BucketService and uploads objects with the
// AWS SDK through the S3 front.
type Uploader struct {
	buckets   storagesvc.BucketService
	s3        *s3.Client
	projectID string

	mu      sync.Mutex
	created map[string]bool
}

// New returns an Uploader authenticated with a fresh editor access key.
func New(t *testing.T, inst *testkit.Instance, s3Addr string) *Uploader {
	t.Helper()
	sdk := inst.SDK(t)
	keyID, secret := AccessKey(t, sdk, inst, "editor")
	return &Uploader{
		buckets: storagesvc.NewBucketService(sdk), s3: Client(s3Addr, keyID, secret),
		projectID: inst.ProjectID, created: map[string]bool{},
	}
}

// Upload creates the bucket on first use and puts data at object. It has the
// signature of imagescenario.Uploader.
func (u *Uploader) Upload(t *testing.T, bucket, object string, data []byte) {
	t.Helper()
	u.mu.Lock()
	defer u.mu.Unlock()
	if !u.created[bucket] {
		wait(t)(u.buckets.Create(t.Context(), &storagepb.CreateBucketRequest{
			Metadata: &commonpb.ResourceMetadata{ParentId: u.projectID, Name: bucket},
			Spec:     &storagepb.BucketSpec{},
		}))
		u.created[bucket] = true
	}
	_, err := u.s3.PutObject(t.Context(), &s3.PutObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(object), Body: bytes.NewReader(data),
		ContentLength: aws.Int64(int64(len(data))),
	})
	if err != nil {
		t.Fatalf("S3 PutObject s3://%s/%s: %v", bucket, object, err)
	}
}
