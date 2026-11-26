package domain_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/nebius/gosdk/proto/fieldmask/mask"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"
	storagev1alpha1pb "github.com/nebius/gosdk/proto/nebius/storage/v1alpha1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/services/storage/domain"
)

func TestTransferCreateV1Validation(t *testing.T) {
	t.Parallel()
	azure := func(s *storagepb.TransferSpec) {
		s.Source.Provider = &storagepb.TransferSource_AzureBlobStorage{AzureBlobStorage: &storagepb.TransferSource_AzureBlobStorageProvider{
			Endpoint: "https://acc.blob.core.windows.net", ContainerName: "c",
			Credentials: &storagepb.TransferSource_AzureBlobStorageProvider_AzureStorageAccount{
				AzureStorageAccount: &storagepb.TransferCredentialsAzureStorageAccount{AccountName: "acc"},
			},
		}}
	}
	s3 := func(s *storagepb.TransferSpec) *storagepb.TransferSource_S3CompatibleProvider {
		return s.GetSource().GetS3Compatible()
	}
	dst := func(s *storagepb.TransferSpec) *storagepb.TransferDestination_NebiusProvider {
		return s.GetDestination().GetNebius()
	}
	for name, tc := range map[string]struct {
		mutate func(*storagepb.TransferSpec)
		field  string
	}{
		"no source":               {func(s *storagepb.TransferSpec) { s.Source = nil }, "spec.source"},
		"no source provider":      {func(s *storagepb.TransferSpec) { s.Source.Provider = nil }, "spec.source.provider"},
		"endpoint scheme":         {func(s *storagepb.TransferSpec) { s3(s).Endpoint = "ftp://s3.example.com" }, "spec.source.s3_compatible.endpoint"},
		"endpoint trailing slash": {func(s *storagepb.TransferSpec) { s3(s).Endpoint = "https://s3.example.com/" }, "spec.source.s3_compatible.endpoint"},
		"endpoint query":          {func(s *storagepb.TransferSpec) { s3(s).Endpoint = "https://s3.example.com?a=b" }, "spec.source.s3_compatible.endpoint"},
		"endpoint userinfo":       {func(s *storagepb.TransferSpec) { s3(s).Endpoint = "https://u:p@s3.example.com" }, "spec.source.s3_compatible.endpoint"},
		"endpoint relative":       {func(s *storagepb.TransferSpec) { s3(s).Endpoint = "s3.example.com" }, "spec.source.s3_compatible.endpoint"},
		"endpoint missing":        {func(s *storagepb.TransferSpec) { s3(s).Endpoint = "" }, "spec.source.s3_compatible.endpoint"},
		"source region":           {func(s *storagepb.TransferSpec) { s3(s).Region = "" }, "spec.source.s3_compatible.region"},
		"source bucket too long":  {func(s *storagepb.TransferSpec) { s3(s).BucketName = strings.Repeat("b", 101) }, "spec.source.s3_compatible.bucket_name"},
		"source credentials":      {func(s *storagepb.TransferSpec) { s3(s).Credentials = nil }, "spec.source.s3_compatible.credentials"},
		"source secret": {func(s *storagepb.TransferSpec) {
			s3(s).GetAccessKey().SecretAccessKey = ""
		}, "spec.source.s3_compatible.access_key.secret_access_key"},
		"source prefix too long": {func(s *storagepb.TransferSpec) { s.Source.Prefix = strings.Repeat("p", 1001) }, "spec.source.prefix"},
		"azure access key":       {azure, "spec.source.azure_blob_storage.azure_storage_account.access_key"},
		"no destination":         {func(s *storagepb.TransferSpec) { s.Destination = nil }, "spec.destination"},
		"destination region":     {func(s *storagepb.TransferSpec) { dst(s).Region = "us-central1" }, "spec.destination.nebius.region"},
		"destination credentials": {func(s *storagepb.TransferSpec) {
			dst(s).Credentials = nil
		}, "spec.destination.nebius.credentials"},
		"destination bucket missing": {func(s *storagepb.TransferSpec) { dst(s).BucketName = "nope" }, "spec.destination.nebius.bucket_name"},
		"destination bucket foreign": {func(s *storagepb.TransferSpec) { dst(s).BucketName = "foreign-bucket" }, "spec.destination.nebius.bucket_name"},
		"destination bucket creating": {func(s *storagepb.TransferSpec) {
			dst(s).BucketName = "new-bucket"
		}, "spec.destination.nebius.bucket_name"},
		"external to external": {func(s *storagepb.TransferSpec) {
			s.Destination.Provider = &storagepb.TransferDestination_S3Compatible{S3Compatible: &storagepb.TransferDestination_S3CompatibleProvider{
				Endpoint: "https://other.example.com", Region: "r", BucketName: "b",
				Credentials: &storagepb.TransferDestination_S3CompatibleProvider_Anonymous{Anonymous: &storagepb.TransferCredentialsAnonymous{}},
			}}
		}, "spec.destination"},
		"source nebius bucket foreign": {func(s *storagepb.TransferSpec) {
			s.Source.Provider = &storagepb.TransferSource_Nebius{Nebius: &storagepb.TransferSource_NebiusProvider{
				Region: trRegion, BucketName: "foreign-bucket",
				Credentials: &storagepb.TransferSource_NebiusProvider_Anonymous{Anonymous: &storagepb.TransferCredentialsAnonymous{}},
			}}
			s.Destination.Provider = &storagepb.TransferDestination_S3Compatible{S3Compatible: &storagepb.TransferDestination_S3CompatibleProvider{
				Endpoint: "https://other.example.com", Region: "r", BucketName: "b",
				Credentials: &storagepb.TransferDestination_S3CompatibleProvider_Anonymous{Anonymous: &storagepb.TransferCredentialsAnonymous{}},
			}}
		}, "spec.source.nebius.bucket_name"},
		"no stop condition": {func(s *storagepb.TransferSpec) { s.StopCondition = nil }, "spec.stop_condition"},
		"zero threshold": {func(s *storagepb.TransferSpec) {
			s.StopCondition = &storagepb.TransferSpec_AfterNEmptyIterations{AfterNEmptyIterations: &storagepb.TransferSpec_StopConditionAfterNEmptyIterations{}}
		}, "spec.after_n_empty_iterations.empty_iterations_threshold"},
		"negative interval": {func(s *storagepb.TransferSpec) {
			s.InterIterationInterval = durationpb.New(-time.Second)
		}, "spec.inter_iteration_interval"},
		"no overwrite strategy": {func(s *storagepb.TransferSpec) {
			s.OverwriteStrategy = storagepb.TransferSpec_OVERWRITE_STRATEGY_UNSPECIFIED
		}, "spec.overwrite_strategy"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newTrFixture(t, 0)
			spec := trV1Spec()
			tc.mutate(spec)
			_, err := f.svc.CreateV1(t.Context(), domain.CreateTransferV1Cmd{
				Metadata: &commonpb.ResourceMetadata{ParentId: projectID, Name: "t"}, Spec: spec,
			})
			trWantViolation(t, err, tc.field)
		})
	}
}

func TestTransferCreateMetadataValidation(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		md    *commonpb.ResourceMetadata
		field string
	}{
		"no parent":      {&commonpb.ResourceMetadata{Name: "t"}, "metadata.parent_id"},
		"tenant parent":  {&commonpb.ResourceMetadata{ParentId: "tenant-e00test0000000000001", Name: "t"}, "metadata.parent_id"},
		"no name":        {&commonpb.ResourceMetadata{ParentId: projectID}, "metadata.name"},
		"id on create":   {&commonpb.ResourceMetadata{Id: "storagetransfer-x", ParentId: projectID, Name: "t"}, "metadata.id"},
		"unknown parent": {&commonpb.ResourceMetadata{ParentId: "project-e00unknown", Name: "t"}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newTrFixture(t, 0)
			_, err := f.svc.CreateV1(t.Context(), domain.CreateTransferV1Cmd{Metadata: tc.md, Spec: trV1Spec()})
			if tc.field == "" {
				if !errs.IsKind(err, errs.KindNotFound) {
					t.Fatalf("err = %v, want NotFound", err)
				}
				return
			}
			trWantViolation(t, err, tc.field)
		})
	}
}

func TestTransferCreateV1Alpha1Validation(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		mutate func(*storagev1alpha1pb.TransferSpec)
		field  string
	}{
		"no source":       {func(s *storagev1alpha1pb.TransferSpec) { s.Source = nil }, "spec.source"},
		"source endpoint": {func(s *storagev1alpha1pb.TransferSpec) { s.Source.Endpoint = "https://x/" }, "spec.source.endpoint"},
		"source region":   {func(s *storagev1alpha1pb.TransferSpec) { s.Source.Region = "" }, "spec.source.region"},
		"source bucket":   {func(s *storagev1alpha1pb.TransferSpec) { s.Source.BucketName = "" }, "spec.source.bucket_name"},
		"source secret":   {func(s *storagev1alpha1pb.TransferSpec) { s.Source.Credentials = trAlphaCreds("K", "") }, "spec.source.credentials.access_key.secret_access_key"},
		"source empty oneof": {func(s *storagev1alpha1pb.TransferSpec) {
			s.Source.Credentials = &storagev1alpha1pb.TransferSpec_BucketCredentials{}
		}, "spec.source.credentials"},
		"dest credentials":    {func(s *storagev1alpha1pb.TransferSpec) { s.Destination.Credentials = nil }, "spec.destination.credentials"},
		"dest bucket":         {func(s *storagev1alpha1pb.TransferSpec) { s.Destination.BucketName = "" }, "spec.destination.bucket_name"},
		"dest bucket missing": {func(s *storagev1alpha1pb.TransferSpec) { s.Destination.BucketName = "nope" }, "spec.destination.bucket_name"},
		"dest region":         {func(s *storagev1alpha1pb.TransferSpec) { s.Destination.Region = "us-central1" }, "spec.destination.region"},
		"dest endpoint":       {func(s *storagev1alpha1pb.TransferSpec) { s.Destination.Endpoint = "https://other.example.com" }, "spec.destination.endpoint"},
		"dest azure": {func(s *storagev1alpha1pb.TransferSpec) {
			s.Destination.Credentials = &storagev1alpha1pb.TransferSpec_BucketCredentials{Credentials: &storagev1alpha1pb.TransferSpec_BucketCredentials_AzureAccessKey_{
				AzureAccessKey: &storagev1alpha1pb.TransferSpec_BucketCredentials_AzureAccessKey{AccountName: "a", AccessKey: "k"},
			}}
		}, "spec.destination.credentials.azure_access_key"},
		"dest anonymous": {func(s *storagev1alpha1pb.TransferSpec) {
			s.Destination.Credentials = &storagev1alpha1pb.TransferSpec_BucketCredentials{Credentials: &storagev1alpha1pb.TransferSpec_BucketCredentials_Anonymous{
				Anonymous: &storagev1alpha1pb.TransferSpec_BucketCredentials_CredentialsAnonymous{},
			}}
		}, "spec.destination.credentials"},
		"no stop condition": {func(s *storagev1alpha1pb.TransferSpec) { s.StopCondition = nil }, "spec.stop_condition"},
		"no overwrite": {func(s *storagev1alpha1pb.TransferSpec) {
			s.OverwriteStrategy = storagev1alpha1pb.TransferSpec_OVERWRITE_STRATEGY_UNSPECIFIED
		}, "spec.overwrite_strategy"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newTrFixture(t, 0)
			spec := trAlphaSpec()
			tc.mutate(spec)
			_, err := f.svc.CreateV1Alpha1(t.Context(), domain.CreateTransferV1Alpha1Cmd{
				Metadata: &commonpb.ResourceMetadata{ParentId: projectID, Name: "t"}, Spec: spec,
			})
			trWantViolation(t, err, tc.field)
		})
	}
}

func TestTransferCreateDefaultsAndPlan(t *testing.T) {
	t.Parallel()
	f := newTrFixture(t, 0)
	id := f.createV1(t, "t", trV1Spec())
	rec := f.get(t, id)
	if got := rec.V1.GetSpec().GetInterIterationInterval().AsDuration(); got != domain.DefaultTransferInterval {
		t.Errorf("interval = %s, want the 15m default", got)
	}
	if rec.Status.State != domain.TransferActive || rec.Status.LastIteration != nil || !rec.NextRun.Equal(t0) {
		t.Errorf("new transfer status = %+v next %s, want ACTIVE due now without iterations", rec.Status, rec.NextRun)
	}
	md := rec.V1.GetMetadata()
	if md.GetResourceVersion() != 1 || md.GetLabels()["k"] != "v" || md.GetParentId() != projectID {
		t.Errorf("metadata = %v", md)
	}
	stored, _ := f.repo.Get(t.Context(), id)
	plan, err := f.svc.PlanOf(stored)
	if err != nil {
		t.Fatal(err)
	}
	want := domain.Plan{
		Source: domain.Side{
			Kind: domain.ProviderS3Compatible, Endpoint: trSrcEndpoint, Region: "us-east-1", Bucket: "src", Prefix: "in/",
			Creds: domain.Credentials{AccessKeyID: "SRCKEY", Secret: "SRCSECRET"},
		},
		Destination: domain.Side{
			Kind: domain.ProviderNebius, Region: trRegion, Bucket: trDstBucket, Prefix: "out/",
			Creds: domain.Credentials{AccessKeyID: "DSTKEY", Secret: "DSTSECRET"},
		},
		Overwrite:         domain.OverwriteNever,
		RequestsPerSecond: domain.DefaultTransferRequestsPerSecond, BandwidthBytesPerSecond: domain.DefaultTransferBandwidth,
		Stop:     domain.StopCondition{Kind: domain.StopAfterOneIteration},
		Interval: domain.DefaultTransferInterval,
	}
	if plan != want {
		t.Errorf("PlanOf = %+v\nwant %+v", plan, want)
	}
}

func TestTransferPlanOfV1Alpha1(t *testing.T) {
	t.Parallel()
	f := newTrFixture(t, 0)
	spec := trAlphaSpec()
	spec.Source.Credentials = &storagev1alpha1pb.TransferSpec_BucketCredentials{Credentials: &storagev1alpha1pb.TransferSpec_BucketCredentials_AzureAccessKey_{
		AzureAccessKey: &storagev1alpha1pb.TransferSpec_BucketCredentials_AzureAccessKey{AccountName: "acc", AccessKey: "AZKEY"},
	}}
	spec.Source.Limiters = &storagev1alpha1pb.TransferSpec_Limiters{RequestsPerSecond: 7}
	id := f.createAlpha(t, "t", spec)
	stored, _ := f.repo.Get(t.Context(), id)
	plan, err := f.svc.PlanOf(stored)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Source.Kind != domain.ProviderAzureBlob || plan.Source.Creds.AzureKey != "AZKEY" || plan.Source.Bucket != "src" {
		t.Errorf("source = %+v, want Azure with the key", plan.Source)
	}
	if plan.Destination.Kind != domain.ProviderNebius || plan.Destination.Region != trRegion || plan.Destination.Creds.Secret != "DSTSECRET" {
		t.Errorf("destination = %+v, want the Nebius bucket in the served region", plan.Destination)
	}
	if !plan.TouchUnmanaged || plan.DeleteInDestination || plan.Overwrite != domain.OverwriteAlways {
		t.Errorf("plan flags = %+v", plan)
	}
	if plan.RequestsPerSecond != 7 || plan.BandwidthBytesPerSecond != domain.DefaultTransferBandwidth {
		t.Errorf("limiters = %d rps %d B/s", plan.RequestsPerSecond, plan.BandwidthBytesPerSecond)
	}
	// Absent source credentials are anonymous.
	spec = trAlphaSpec()
	spec.Source.Credentials = nil
	stored, _ = f.repo.Get(t.Context(), f.createAlpha(t, "anon", spec))
	if plan, _ := f.svc.PlanOf(stored); !plan.Source.Creds.Anonymous || plan.Source.Kind != domain.ProviderS3Compatible {
		t.Errorf("anonymous source = %+v", plan.Source)
	}
}

func TestTransferSecretsNeverReturned(t *testing.T) {
	t.Parallel()
	f := newTrFixture(t, 0)
	v1 := f.createV1(t, "v1", trV1Spec())
	alpha := f.createAlpha(t, "alpha", trAlphaSpec())
	ctx := t.Context()
	byName, err := f.svc.GetByName(ctx, projectID, "v1", domain.TransferAPIV1)
	if err != nil {
		t.Fatal(err)
	}
	page, err := f.svc.List(ctx, projectID, domain.TransferAPIV1, 0, "")
	if err != nil || len(page.Items) != 2 {
		t.Fatalf("List = %d items, %v", len(page.Items), err)
	}
	for _, rec := range append([]*domain.TransferRecord{f.get(t, v1), f.get(t, alpha), byName}, page.Items...) {
		if spec := rec.V1.GetSpec(); spec != nil {
			src, dst := spec.GetSource().GetS3Compatible().GetAccessKey(), spec.GetDestination().GetNebius().GetAccessKey()
			if src.GetSecretAccessKey() != "" || dst.GetSecretAccessKey() != "" {
				t.Errorf("v1 secrets returned: %v / %v", src, dst)
			}
			if src.GetAccessKeyId() != "SRCKEY" || dst.GetAccessKeyId() != "DSTKEY" {
				t.Errorf("access key IDs must stay: %v / %v", src, dst)
			}
		}
		if spec := rec.Alpha.GetSpec(); spec != nil && (spec.GetSource().GetCredentials() != nil || spec.GetDestination().GetCredentials() != nil) {
			t.Errorf("v1alpha1 credentials returned: %v", spec)
		}
	}
	// The store keeps them for the engine.
	stored, _ := f.repo.Get(ctx, v1)
	if stored.V1.GetSpec().GetSource().GetS3Compatible().GetAccessKey().GetSecretAccessKey() != "SRCSECRET" {
		t.Error("the stored secret was lost")
	}
}

func TestTransferNamesAndVisibility(t *testing.T) {
	t.Parallel()
	f := newTrFixture(t, 0)
	ctx := t.Context()
	v1 := f.createV1(t, "same", trV1Spec())
	_, err := f.svc.CreateV1Alpha1(ctx, domain.CreateTransferV1Alpha1Cmd{Metadata: &commonpb.ResourceMetadata{ParentId: projectID, Name: "same"}, Spec: trAlphaSpec()})
	if !errs.IsKind(err, errs.KindAlreadyExists) {
		t.Fatalf("same name through v1alpha1: %v, want AlreadyExists", err)
	}
	alpha := f.createAlpha(t, "alpha", trAlphaSpec())
	if _, err := f.svc.Get(ctx, v1, domain.TransferAPIV1Alpha1); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("v1alpha1 Get of a v1 transfer = %v, want NotFound", err)
	}
	if _, err := f.svc.GetByName(ctx, projectID, "same", domain.TransferAPIV1Alpha1); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("v1alpha1 GetByName of a v1 transfer = %v, want NotFound", err)
	}
	if rec, err := f.svc.Get(ctx, alpha, domain.TransferAPIV1); err != nil || rec.API != domain.TransferAPIV1Alpha1 {
		t.Errorf("v1 Get of a v1alpha1 transfer = %v, %v", rec, err)
	}
	page, err := f.svc.List(ctx, projectID, domain.TransferAPIV1Alpha1, 0, "")
	if err != nil || len(page.Items) != 1 || page.Items[0].Metadata().GetId() != alpha {
		t.Errorf("v1alpha1 List = %v, %v; want only %s", page.Items, err, alpha)
	}
	if _, err := f.svc.List(ctx, projectID, domain.TransferAPIV1, 1001, ""); !errs.IsKind(err, errs.KindBadRequest) {
		t.Errorf("page_size 1001: %v, want BadRequest", err)
	}
	for i := range 11 {
		f.createV1(t, "more"+strings.Repeat("x", i), trV1Spec())
	}
	if page, _ := f.svc.List(ctx, projectID, domain.TransferAPIV1, 0, ""); len(page.Items) != 10 || page.NextPageToken == "" {
		t.Errorf("default page = %d items, token %q; want 10 and a token", len(page.Items), page.NextPageToken)
	}
}

func (f *trFixture) updateV1(t *testing.T, id string, md *commonpb.ResourceMetadata, spec *storagepb.TransferSpec) error {
	t.Helper()
	if md == nil {
		md = &commonpb.ResourceMetadata{}
	}
	md.Id = id
	op, err := f.svc.UpdateV1(t.Context(), domain.UpdateTransferV1Cmd{Metadata: md, Spec: spec, ResetMask: mask.New()})
	if err != nil {
		return err
	}
	return op.Err
}

func TestTransferUpdateV1KeepsSecrets(t *testing.T) {
	t.Parallel()
	f := newTrFixture(t, 0)
	ctx := t.Context()
	id := f.createV1(t, "t", trV1Spec())
	// Clients send back what Get returned: secrets empty.
	spec := f.get(t, id).V1.GetSpec()
	spec.Limiters = &storagepb.TransferSpec_Limiters{RequestsPerSecond: 10}
	if err := f.updateV1(t, id, &commonpb.ResourceMetadata{Name: "renamed", Labels: map[string]string{"x": "y"}}, spec); err != nil {
		t.Fatal(err)
	}
	stored, _ := f.repo.Get(ctx, id)
	sspec := stored.V1.GetSpec()
	if sspec.GetSource().GetS3Compatible().GetAccessKey().GetSecretAccessKey() != "SRCSECRET" ||
		sspec.GetDestination().GetNebius().GetAccessKey().GetSecretAccessKey() != "DSTSECRET" {
		t.Errorf("secrets not kept: %v", sspec)
	}
	md := stored.V1.GetMetadata()
	if md.GetResourceVersion() != 2 || md.GetName() != "renamed" || md.GetLabels()["x"] != "y" || !md.GetUpdatedAt().AsTime().Equal(t0) {
		t.Errorf("metadata after update = %v", md)
	}
	if sspec.GetLimiters().GetRequestsPerSecond() != 10 {
		t.Errorf("limiters not applied: %v", sspec.GetLimiters())
	}
	// Switching to anonymous and back needs the secret again.
	spec = f.get(t, id).V1.GetSpec()
	spec.Source.GetS3Compatible().Credentials = &storagepb.TransferSource_S3CompatibleProvider_Anonymous{Anonymous: &storagepb.TransferCredentialsAnonymous{}}
	if err := f.updateV1(t, id, nil, spec); err != nil {
		t.Fatal(err)
	}
	spec = f.get(t, id).V1.GetSpec()
	spec.Source.GetS3Compatible().Credentials = &storagepb.TransferSource_S3CompatibleProvider_AccessKey{AccessKey: trAccessKey("NEW", "")}
	trWantViolation(t, f.updateV1(t, id, nil, spec), "spec.source.s3_compatible.access_key.secret_access_key")
	// A no-op update keeps the version.
	before := f.get(t, id).V1.GetMetadata().GetResourceVersion()
	if err := f.updateV1(t, id, nil, f.get(t, id).V1.GetSpec()); err != nil {
		t.Fatal(err)
	}
	if after := f.get(t, id).V1.GetMetadata().GetResourceVersion(); after != before {
		t.Errorf("no-op update bumped resource_version %d -> %d", before, after)
	}
}

func TestTransferUpdateV1Immutables(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		mutate func(*storagepb.TransferSpec)
		field  string
	}{
		"endpoint":      {func(s *storagepb.TransferSpec) { s.Source.GetS3Compatible().Endpoint = "https://other.example.com" }, "spec.source.s3_compatible.endpoint"},
		"source bucket": {func(s *storagepb.TransferSpec) { s.Source.GetS3Compatible().BucketName = "other" }, "spec.source.s3_compatible.bucket_name"},
		"source prefix": {func(s *storagepb.TransferSpec) { s.Source.Prefix = "x/" }, "spec.source.prefix"},
		"dest prefix":   {func(s *storagepb.TransferSpec) { s.Destination.Prefix = "other/" }, "spec.destination.prefix"},
		"dest bucket":   {func(s *storagepb.TransferSpec) { s.Destination.GetNebius().BucketName = "src-bucket" }, "spec.destination.nebius.bucket_name"},
		"overwrite":     {func(s *storagepb.TransferSpec) { s.OverwriteStrategy = storagepb.TransferSpec_IF_NEWER }, "spec.overwrite_strategy"},
		"deletes":       {func(s *storagepb.TransferSpec) { s.EnableDeletesInDestination = true }, "spec.enable_deletes_in_destination"},
		"touch":         {func(s *storagepb.TransferSpec) { s.TouchUnmanaged = true }, "spec.touch_unmanaged"},
		"source provider": {func(s *storagepb.TransferSpec) {
			s.Source.Provider = &storagepb.TransferSource_AzureBlobStorage{AzureBlobStorage: &storagepb.TransferSource_AzureBlobStorageProvider{
				Endpoint: "https://acc.blob.core.windows.net", ContainerName: "c",
				Credentials: &storagepb.TransferSource_AzureBlobStorageProvider_Anonymous{Anonymous: &storagepb.TransferCredentialsAnonymous{}},
			}}
		}, "spec.source.provider"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newTrFixture(t, 0)
			id := f.createV1(t, "t", trV1Spec())
			spec := f.get(t, id).V1.GetSpec()
			tc.mutate(spec)
			err := f.updateV1(t, id, nil, spec)
			trWantViolation(t, err, tc.field)
			e, _ := errors.AsType[*errs.Error](err)
			for _, v := range e.Violations {
				if v.Message != "immutable" || v.Field != tc.field {
					t.Errorf("extra violation %v", v)
				}
			}
		})
	}
}

func TestTransferUpdateRules(t *testing.T) {
	t.Parallel()
	f := newTrFixture(t, 0)
	ctx := t.Context()
	id := f.createV1(t, "t", trV1Spec())
	f.createV1(t, "taken", trV1Spec())
	alpha := f.createAlpha(t, "alpha", trAlphaSpec())
	if err := f.updateV1(t, id, &commonpb.ResourceMetadata{ResourceVersion: 7}, trV1Spec()); !errs.IsKind(err, errs.KindConflict) {
		t.Errorf("stale resource_version: %v, want Conflict", err)
	}
	if err := f.updateV1(t, id, &commonpb.ResourceMetadata{Name: "taken"}, trV1Spec()); !errs.IsKind(err, errs.KindAlreadyExists) {
		t.Errorf("rename to a taken name: %v, want AlreadyExists", err)
	}
	if err := f.updateV1(t, alpha, nil, trV1Spec()); !errs.IsKind(err, errs.KindBadState) || !strings.Contains(err.Error(), "v1alpha1") {
		t.Errorf("v1 update of a v1alpha1 transfer: %v, want BadState naming v1alpha1", err)
	}
	_, err := f.svc.UpdateV1Alpha1(ctx, domain.UpdateTransferV1Alpha1Cmd{Metadata: &commonpb.ResourceMetadata{Id: id}, Spec: trAlphaSpec()})
	if !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("v1alpha1 update of a v1 transfer: %v, want NotFound", err)
	}
	// Clearing an immutable field through the reset mask is a change too.
	spec := f.get(t, id).V1.GetSpec()
	spec.Source.Prefix = ""
	_, err = f.svc.UpdateV1(ctx, domain.UpdateTransferV1Cmd{
		Metadata: &commonpb.ResourceMetadata{Id: id}, Spec: spec, ResetMask: mask.NewFieldPath("spec", "source", "prefix").ToMask(),
	})
	trWantViolation(t, err, "spec.source.prefix")
	if _, err := f.svc.UpdateV1(ctx, domain.UpdateTransferV1Cmd{Metadata: &commonpb.ResourceMetadata{}, Spec: trV1Spec()}); !errs.IsKind(err, errs.KindBadRequest) {
		t.Errorf("update without id: %v, want BadRequest", err)
	}
}

func TestTransferUpdateV1Alpha1(t *testing.T) {
	t.Parallel()
	f := newTrFixture(t, 0)
	ctx := t.Context()
	id := f.createAlpha(t, "t", trAlphaSpec())
	update := func(spec *storagev1alpha1pb.TransferSpec) error {
		op, err := f.svc.UpdateV1Alpha1(ctx, domain.UpdateTransferV1Alpha1Cmd{Metadata: &commonpb.ResourceMetadata{Id: id}, Spec: spec, ResetMask: mask.New()})
		if err != nil {
			return err
		}
		return op.Err
	}
	// Get returns no credentials; sending that back keeps the stored ones.
	spec := f.get(t, id).Alpha.GetSpec()
	spec.OverwriteStrategy = storagev1alpha1pb.TransferSpec_NEVER // mutable in v1alpha1
	if err := update(spec); err != nil {
		t.Fatal(err)
	}
	stored, _ := f.repo.Get(ctx, id)
	if !proto.Equal(stored.Alpha.GetSpec().GetSource().GetCredentials(), trAlphaCreds("SRCKEY", "SRCSECRET")) ||
		stored.Alpha.GetSpec().GetOverwriteStrategy() != storagev1alpha1pb.TransferSpec_NEVER {
		t.Errorf("after update: %v", stored.Alpha.GetSpec())
	}
	for field, mutate := range map[string]func(*storagev1alpha1pb.TransferSpec){
		"spec.source.endpoint":         func(s *storagev1alpha1pb.TransferSpec) { s.Source.Endpoint = "https://other.example.com" },
		"spec.source.region":           func(s *storagev1alpha1pb.TransferSpec) { s.Source.Region = "r2" },
		"spec.source.prefix":           func(s *storagev1alpha1pb.TransferSpec) { s.Source.Prefix = "x/" },
		"spec.destination.bucket_name": func(s *storagev1alpha1pb.TransferSpec) { s.Destination.BucketName = "src-bucket" },
		"spec.destination.prefix":      func(s *storagev1alpha1pb.TransferSpec) { s.Destination.Prefix = "p/" },
	} {
		spec := f.get(t, id).Alpha.GetSpec()
		mutate(spec)
		trWantViolation(t, update(spec), field)
	}
}

func TestTransferStateGuards(t *testing.T) {
	t.Parallel()
	f := newTrFixture(t, 0)
	ctx := t.Context()
	id := f.createV1(t, "t", trV1Spec())
	alpha := f.createAlpha(t, "alpha", trAlphaSpec())
	if _, err := f.svc.Resume(ctx, id, domain.TransferAPIV1, nil); !errs.IsKind(err, errs.KindBadState) {
		t.Errorf("Resume of ACTIVE: %v, want BadState", err)
	}
	if _, err := f.svc.Delete(ctx, id, 9, domain.TransferAPIV1, nil); !errs.IsKind(err, errs.KindBadState) {
		t.Errorf("Delete with a stale resource_version: %v, want BadState", err)
	}
	if _, err := f.svc.Delete(ctx, alpha, 0, domain.TransferAPIV1Alpha1, nil); !errs.IsKind(err, errs.KindBadState) {
		t.Errorf("v1alpha1 Delete of ACTIVE: %v, want BadState", err)
	}
	if _, err := f.svc.Stop(ctx, id, domain.TransferAPIV1Alpha1, nil); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("v1alpha1 Stop of a v1 transfer: %v, want NotFound", err)
	}
	// Without a running iteration Stop is immediate.
	if op, err := f.svc.Stop(ctx, id, domain.TransferAPIV1, nil); err != nil || op.Err != nil {
		t.Fatalf("Stop: %v / %v", err, op)
	}
	if st := f.get(t, id).Status.State; st != domain.TransferStopped {
		t.Errorf("state after Stop = %v, want STOPPED", st)
	}
	if _, err := f.svc.Stop(ctx, id, domain.TransferAPIV1, nil); !errs.IsKind(err, errs.KindBadState) {
		t.Errorf("Stop of STOPPED: %v, want BadState", err)
	}
	if op, err := f.svc.Delete(ctx, id, 1, domain.TransferAPIV1, nil); err != nil || op.Err != nil {
		t.Fatalf("Delete: %v / %v", err, op)
	}
	if _, err := f.svc.Get(ctx, id, domain.TransferAPIV1); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("Get after Delete: %v, want NotFound", err)
	}
	if _, err := f.svc.History(ctx, id, domain.TransferAPIV1, 0, ""); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("History after Delete: %v, want NotFound", err)
	}
}
