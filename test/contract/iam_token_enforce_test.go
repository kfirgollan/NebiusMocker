package contract

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"

	"github.com/nebius/gosdk"
	"github.com/nebius/gosdk/auth"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"
	kmspb "github.com/nebius/gosdk/proto/nebius/kms/v1"
	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"
	vpcpb "github.com/nebius/gosdk/proto/nebius/vpc/v1"
	iamsvc "github.com/nebius/gosdk/services/nebius/iam/v1"
	kmssvc "github.com/nebius/gosdk/services/nebius/kms/v1"
	storagesvc "github.com/nebius/gosdk/services/nebius/storage/v1"
	vpcsvc "github.com/nebius/gosdk/services/nebius/vpc/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore/memory"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/services/kms"
	"github.com/kfirgollan/NebiusMocker/services/storage"
	"github.com/kfirgollan/NebiusMocker/services/vpc"
)

// saAuthCall is one API call made with a given client; name is the resource
// name for creates.
type saAuthCall struct {
	label string
	do    func(ctx context.Context, sdk *gosdk.SDK, name string) error
}

func saAuthWrites(tenant, project string) []saAuthCall {
	return []saAuthCall{
		{"iam group", func(ctx context.Context, sdk *gosdk.SDK, name string) error {
			_, err := iamsvc.NewGroupService(sdk).Create(ctx, &iampb.CreateGroupRequest{
				Metadata: &commonpb.ResourceMetadata{ParentId: tenant, Name: name}, Spec: &iampb.GroupSpec{},
			})
			return err
		}},
		{"vpc network", func(ctx context.Context, sdk *gosdk.SDK, name string) error {
			_, err := vpcsvc.NewNetworkService(sdk).Create(ctx, &vpcpb.CreateNetworkRequest{
				Metadata: md(project, name), Spec: &vpcpb.NetworkSpec{},
			})
			return err
		}},
		{"kms symmetric key", func(ctx context.Context, sdk *gosdk.SDK, name string) error {
			_, err := kmssvc.NewSymmetricKeyService(sdk).Create(ctx, &kmspb.CreateSymmetricKeyRequest{
				Metadata: md(project, name), Spec: &kmspb.SymmetricKeySpec{Algorithm: kmspb.SymmetricAlgorithm_AES_256},
			})
			return err
		}},
		{"storage bucket", func(ctx context.Context, sdk *gosdk.SDK, name string) error {
			_, err := storagesvc.NewBucketService(sdk).Create(ctx, &storagepb.CreateBucketRequest{
				Metadata: md(project, name), Spec: &storagepb.BucketSpec{},
			})
			return err
		}},
	}
}

func saAuthReads(tenant, project string) []saAuthCall {
	return []saAuthCall{
		{"iam groups", func(ctx context.Context, sdk *gosdk.SDK, _ string) error {
			_, err := iamsvc.NewGroupService(sdk).List(ctx, &iampb.ListGroupsRequest{ParentId: tenant})
			return err
		}},
		{"vpc networks", func(ctx context.Context, sdk *gosdk.SDK, _ string) error {
			_, err := vpcsvc.NewNetworkService(sdk).List(ctx, &vpcpb.ListNetworksRequest{ParentId: project})
			return err
		}},
		{"kms symmetric keys", func(ctx context.Context, sdk *gosdk.SDK, _ string) error {
			_, err := kmssvc.NewSymmetricKeyService(sdk).List(ctx, &kmspb.ListSymmetricKeysRequest{ParentId: project})
			return err
		}},
		{"storage buckets", func(ctx context.Context, sdk *gosdk.SDK, _ string) error {
			_, err := storagesvc.NewBucketService(sdk).List(ctx, &storagepb.ListBucketsRequest{ParentId: project})
			return err
		}},
		{"profile", func(ctx context.Context, sdk *gosdk.SDK, _ string) error {
			_, err := iamsvc.NewProfileService(sdk).Get(ctx, &iampb.GetProfileRequest{})
			return err
		}},
	}
}

// TestServiceAccountAuthViewerEnforced authenticates a viewers-only service
// account through its credentials file and checks the viewer role applies to it.
func TestServiceAccountAuthViewerEnforced(t *testing.T) {
	t.Parallel()
	env := saAuthStart(t,
		[]mocker.Module{iam.New(), vpc.New(), kms.New(), storage.New(storage.WithS3Listen("127.0.0.1:0"))},
		testkit.WithAuthz(authz.ModeEnforce), testkit.WithObjStore(memory.New(clock.Real{})))

	saID := env.newSA(t, "viewer-sa")
	env.addToGroup(t, saID, "viewers")
	key := env.newKey(t, saID, nil)
	saSDK := saAuthSDK(t, env.inst, auth.NewServiceAccountCredentialsFileParser(nil, key.credentialsFile(t)))
	saAuthWantProfile(t, saSDK, saID)

	for _, w := range saAuthWrites(env.inst.TenantID, env.inst.ProjectID) {
		t.Run("write "+w.label, func(t *testing.T) {
			t.Parallel()
			if err := w.do(t.Context(), env.admin, "by-admin"); err != nil {
				t.Fatalf("admin %s create (the request must be valid): %v", w.label, err)
			}
			assertCode(t, w.do(t.Context(), saSDK, "by-viewer-sa"), codes.PermissionDenied)
		})
	}
	for _, r := range saAuthReads(env.inst.TenantID, env.inst.ProjectID) {
		t.Run("read "+r.label, func(t *testing.T) {
			t.Parallel()
			if err := r.do(t.Context(), saSDK, ""); err != nil {
				t.Errorf("viewer service account %s: %v", r.label, err)
			}
		})
	}
}
