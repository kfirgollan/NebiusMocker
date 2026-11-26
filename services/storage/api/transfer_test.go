package api

import (
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/testing/protocmp"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"
	storagev1alpha1pb "github.com/nebius/gosdk/proto/nebius/storage/v1alpha1"

	"github.com/kfirgollan/NebiusMocker/services/storage/domain"
)

func TestStatusV1(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	running := &domain.Iteration{Seq: 2, State: domain.IterationInProgress, Start: start, Transferred: 3}
	got := statusV1(domain.TransferStatus{State: domain.TransferDeleting, LastIteration: running})
	want := &storagepb.TransferStatus{
		State:           storagepb.TransferStatus_DELETING,
		SuspensionState: storagepb.TransferStatus_NOT_SUSPENDED,
		LastIteration: &storagepb.TransferIteration{
			SequenceNumber: 2, State: storagepb.TransferIteration_IN_PROGRESS, StartTime: timestamppb.New(start), ObjectsTransferredCount: 3,
		},
	}
	if !proto.Equal(got, want) {
		t.Errorf("running = %v, want %v", got, want)
	}
	if got.GetLastIteration().GetEndTime() != nil {
		t.Error("a running iteration must have no end_time")
	}

	failed := domain.TransferStatus{
		State: domain.TransferFailed, Error: &domain.TransferError{Origin: domain.OriginSource, Code: "AccessDenied", Message: "no"},
		LastIteration: &domain.Iteration{
			Seq: 1, State: domain.IterationFailed, Start: start, End: start.Add(time.Second), Deleted: 4, TransferredBytes: 9, ThroughputBytes: 9,
			Error: &domain.TransferError{Origin: domain.OriginDestination, Code: "NoSuchBucket", Message: "gone"},
		},
	}
	got = statusV1(failed)
	if got.GetState() != storagepb.TransferStatus_FAILED || got.GetError().GetOrigin() != storagepb.TransferError_SOURCE ||
		got.GetError().GetCode() != "AccessDenied" || got.GetError().GetMessage() != "no" {
		t.Errorf("failed status = %v", got)
	}
	it := got.GetLastIteration()
	if it.GetState() != storagepb.TransferIteration_FAILED || it.GetError().GetOrigin() != storagepb.TransferError_DESTINATION ||
		it.GetEndTime().AsTime() != start.Add(time.Second) || it.GetObjectsDeletedCount() != 4 || it.GetObjectsTransferredSize() != 9 || it.GetAverageThroughputBytes() != 9 {
		t.Errorf("failed iteration = %v", it)
	}
}

func TestStatusAlpha(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	got := statusAlpha(domain.TransferStatus{
		State: domain.TransferDeleting,
		Error: &domain.TransferError{Code: "AccessDenied", Message: "no"},
		LastIteration: &domain.Iteration{
			Seq: 1, State: domain.IterationInterrupted, Start: start, End: start, Discovered: 5, Transferred: 3, Skipped: 2, TransferredBytes: 7,
			Error: &domain.TransferError{Message: "stopped"},
		},
	})
	if got.GetState() != storagev1alpha1pb.TransferStatus_STOPPING {
		t.Errorf("DELETING shows %v, want STOPPING", got.GetState())
	}
	if got.GetError() != "AccessDenied: no" {
		t.Errorf("error = %q", got.GetError())
	}
	it := got.GetLastIteration()
	if it.GetState() != storagev1alpha1pb.TransferIteration_STOPPED || it.GetError() != "stopped" ||
		it.GetObjectsDiscoveredCount() != 5 || it.GetObjectsMigratedCount() != 3 || it.GetObjectsSkippedCount() != 2 || it.GetObjectsMigratedSize() != 7 {
		t.Errorf("iteration = %v", it)
	}
}

func alphaSpec() *storagev1alpha1pb.TransferSpec {
	return &storagev1alpha1pb.TransferSpec{
		Source: &storagev1alpha1pb.TransferSpec_SourceBucket{
			Endpoint: "https://s3.example.com", BucketName: "src", Region: "us-east-1", Prefix: "in/",
			Limiters: &storagev1alpha1pb.TransferSpec_Limiters{BandwidthBytesPerSecond: 10, RequestsPerSecond: 20},
		},
		Destination:   &storagev1alpha1pb.TransferSpec_DestinationBucket{BucketName: "dst", Prefix: "out/"},
		StopCondition: &storagev1alpha1pb.TransferSpec_AfterNEmptyIterations{AfterNEmptyIterations: &storagev1alpha1pb.TransferSpec_StopConditionAfterNEmptyIterations{EmptyIterationsThreshold: 2}},

		InterIterationInterval: durationpb.New(time.Minute),
		OverwriteStrategy:      storagev1alpha1pb.TransferSpec_IF_NEWER,
	}
}

func TestAlphaSpecAsV1(t *testing.T) {
	t.Parallel()
	got := alphaSpecAsV1(alphaSpec(), domain.ProviderS3Compatible, "eu-north1")
	want := &storagepb.TransferSpec{
		Source: &storagepb.TransferSource{
			Prefix: "in/",
			Provider: &storagepb.TransferSource_S3Compatible{S3Compatible: &storagepb.TransferSource_S3CompatibleProvider{
				Endpoint: "https://s3.example.com", Region: "us-east-1", BucketName: "src",
			}},
		},
		Destination: &storagepb.TransferDestination{
			Prefix: "out/",
			Provider: &storagepb.TransferDestination_Nebius{Nebius: &storagepb.TransferDestination_NebiusProvider{
				Region: "eu-north1", BucketName: "dst",
			}},
		},
		Limiters:               &storagepb.TransferSpec_Limiters{BandwidthBytesPerSecond: 10, RequestsPerSecond: 20},
		StopCondition:          &storagepb.TransferSpec_AfterNEmptyIterations{AfterNEmptyIterations: &storagepb.TransferSpec_StopConditionAfterNEmptyIterations{EmptyIterationsThreshold: 2}},
		InterIterationInterval: durationpb.New(time.Minute),
		OverwriteStrategy:      storagepb.TransferSpec_IF_NEWER,
		TouchUnmanaged:         true,
	}
	if d := cmp.Diff(want, got, protocmp.Transform()); d != "" {
		t.Errorf("v1 view mismatch (-want +got):\n%s", d)
	}
}

func TestAlphaSpecAsV1Variants(t *testing.T) {
	t.Parallel()
	spec := alphaSpec()
	spec.OverwriteStrategy = storagev1alpha1pb.TransferSpec_ALWAYS
	spec.Source.Endpoint = "https://acc.blob.core.windows.net"
	spec.Destination.Endpoint, spec.Destination.Region = "https://dst.example.com", "us-east-1"
	spec.StopCondition = &storagev1alpha1pb.TransferSpec_Infinite{Infinite: &storagev1alpha1pb.TransferSpec_StopConditionInfinite{}}
	got := alphaSpecAsV1(spec, domain.ProviderAzureBlob, "eu-north1")
	if got.GetOverwriteStrategy() != storagepb.TransferSpec_OverwriteStrategy(3) {
		t.Errorf("ALWAYS = %v, want raw enum 3", got.GetOverwriteStrategy())
	}
	if az := got.GetSource().GetAzureBlobStorage(); az.GetContainerName() != "src" || az.GetEndpoint() != spec.GetSource().GetEndpoint() {
		t.Errorf("source = %v, want azure_blob_storage with container src", got.GetSource())
	}
	if s3 := got.GetDestination().GetS3Compatible(); s3.GetEndpoint() != "https://dst.example.com" || s3.GetRegion() != "us-east-1" || s3.GetBucketName() != "dst" {
		t.Errorf("destination = %v, want s3_compatible", got.GetDestination())
	}
	if got.GetInfinite() == nil {
		t.Errorf("stop condition = %v, want infinite", got.GetStopCondition())
	}

	// v1alpha1 credentials are input-only: the v1 view never shows any.
	got = alphaSpecAsV1(alphaSpec(), domain.ProviderS3Compatible, "eu-north1")
	if got.GetSource().GetS3Compatible().GetCredentials() != nil || got.GetDestination().GetNebius().GetCredentials() != nil {
		t.Errorf("credentials shown: %v", got)
	}
}

func TestErrorAlpha(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		in   *domain.TransferError
		want string
	}{
		"nil":          {nil, ""},
		"code":         {&domain.TransferError{Code: "C"}, "C"},
		"message":      {&domain.TransferError{Message: "m"}, "m"},
		"code message": {&domain.TransferError{Code: "C", Message: "m"}, "C: m"},
	} {
		if got := errorAlpha(tc.in); got != tc.want {
			t.Errorf("%s: %q, want %q", name, got, tc.want)
		}
	}
}
