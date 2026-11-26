package contract

import (
	"context"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/durationpb"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"
	storagev1alpha1pb "github.com/nebius/gosdk/proto/nebius/storage/v1alpha1"
	storagesvc "github.com/nebius/gosdk/services/nebius/storage/v1"
	storagev1alpha1svc "github.com/nebius/gosdk/services/nebius/storage/v1alpha1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
)

func trAlphaCreds(id, secret string) *storagev1alpha1pb.TransferSpec_BucketCredentials {
	return &storagev1alpha1pb.TransferSpec_BucketCredentials{Credentials: &storagev1alpha1pb.TransferSpec_BucketCredentials_AccessKey{
		AccessKey: &storagev1alpha1pb.TransferSpec_BucketCredentials_CredentialsAccessKey{AccessKeyId: id, SecretAccessKey: secret},
	}}
}

// trAlphaSpec copies src-bucket/in/ to the Nebius bucket under out/.
func trAlphaSpec(stop func(*storagev1alpha1pb.TransferSpec)) *storagev1alpha1pb.TransferSpec {
	spec := &storagev1alpha1pb.TransferSpec{
		Source: &storagev1alpha1pb.TransferSpec_SourceBucket{
			Endpoint: trSrcURL, BucketName: "src-bucket", Region: "us-east-1", Prefix: "in/", Credentials: trAlphaCreds("SRCKEY", trSrcSecret),
		},
		Destination:       &storagev1alpha1pb.TransferSpec_DestinationBucket{BucketName: trDstBucket, Prefix: "out/", Credentials: trAlphaCreds("DSTKEY", trDstSecret)},
		OverwriteStrategy: storagev1alpha1pb.TransferSpec_NEVER,
	}
	stop(spec)
	return spec
}

func trAlphaOnce(s *storagev1alpha1pb.TransferSpec) {
	s.StopCondition = &storagev1alpha1pb.TransferSpec_AfterOneIteration{AfterOneIteration: &storagev1alpha1pb.TransferSpec_StopConditionAfterOneIteration{}}
}

func trAlphaInfinite(s *storagev1alpha1pb.TransferSpec) {
	s.StopCondition = &storagev1alpha1pb.TransferSpec_Infinite{Infinite: &storagev1alpha1pb.TransferSpec_StopConditionInfinite{}}
	s.InterIterationInterval = durationpb.New(time.Minute)
}

func trAlphaCreate(ctx context.Context, svc storagev1alpha1svc.TransferService, parent, name string, spec *storagev1alpha1pb.TransferSpec) (string, error) {
	op, err := svc.Create(ctx, &storagev1alpha1pb.CreateTransferRequest{Metadata: &commonpb.ResourceMetadata{ParentId: parent, Name: name}, Spec: spec})
	if err != nil {
		return "", err
	}
	if _, err := op.Wait(ctx); err != nil {
		return "", err
	}
	return op.ResourceID(), nil
}

func trAlphaMustCreate(t *testing.T, svc storagev1alpha1svc.TransferService, parent, name string, spec *storagev1alpha1pb.TransferSpec) string {
	t.Helper()
	id, err := trAlphaCreate(t.Context(), svc, parent, name, spec)
	if err != nil {
		t.Fatalf("Create v1alpha1 transfer %q: %v", name, err)
	}
	return id
}

func trAlphaGet(t *testing.T, svc storagev1alpha1svc.TransferService, id string) *storagev1alpha1pb.Transfer {
	t.Helper()
	tr, err := svc.Get(t.Context(), &storagev1alpha1pb.GetTransferRequest{Id: id})
	if err != nil {
		t.Fatalf("Get v1alpha1 transfer %s: %v", id, err)
	}
	return tr
}

func (e *trEnv) trAlphaAwait(t *testing.T, svc storagev1alpha1svc.TransferService, id string, state storagev1alpha1pb.TransferStatus_State, seq int64, tick time.Duration) *storagev1alpha1pb.Transfer {
	t.Helper()
	var tr *storagev1alpha1pb.Transfer
	// Time moves slowly (every 10th poll) and only until the awaited iteration
	// started, so a fast runner cannot skip over it.
	var seen int64
	polls := 0
	poll(t, "v1alpha1 transfer "+state.String(), func() {
		polls++
		if tick > 0 && seen < seq && polls%10 == 1 {
			e.clk.Advance(tick)
		}
	}, func() bool {
		var err error
		tr, err = svc.Get(t.Context(), &storagev1alpha1pb.GetTransferRequest{Id: id})
		seen = tr.GetStatus().GetLastIteration().GetSequenceNumber()
		return err == nil && tr.GetStatus().GetState() == state && tr.GetStatus().GetLastIteration().GetSequenceNumber() == seq &&
			tr.GetStatus().GetLastIteration().GetState() != storagev1alpha1pb.TransferIteration_IN_PROGRESS
	})
	return tr
}

func trAlphaUpdate(ctx context.Context, svc storagev1alpha1svc.TransferService, id string, mutate func(*storagev1alpha1pb.Transfer)) error {
	tr, err := svc.Get(ctx, &storagev1alpha1pb.GetTransferRequest{Id: id})
	if err != nil {
		return err
	}
	mutate(tr)
	op, err := svc.Update(ctx, &storagev1alpha1pb.UpdateTransferRequest{Metadata: tr.GetMetadata(), Spec: tr.GetSpec()})
	if err != nil {
		return err
	}
	_, err = op.Wait(ctx)
	return err
}

func TestTransferV1Alpha1Lifecycle(t *testing.T) {
	t.Parallel()
	env := startTransfers(t)
	svc := storagev1alpha1svc.NewTransferService(env.inst.SDK(t))
	ctx := t.Context()
	env.fake.put("in/a.txt", "alpha")
	env.fake.put("in/dir/b.txt", "bravo!")
	env.fake.put("other/c.txt", "skipped")

	id := trAlphaMustCreate(t, svc, env.inst.ProjectID, "tr-alpha", trAlphaSpec(trAlphaOnce))
	tr := env.trAlphaAwait(t, svc, id, storagev1alpha1pb.TransferStatus_STOPPED, 1, 0)
	it := tr.GetStatus().GetLastIteration()
	if it.GetState() != storagev1alpha1pb.TransferIteration_COMPLETED || it.GetObjectsDiscoveredCount() != 2 || it.GetObjectsMigratedCount() != 2 ||
		it.GetObjectsMigratedSize() != int64(len("alpha")+len("bravo!")) || it.GetEndTime() == nil {
		t.Errorf("iteration = %v", it)
	}
	if tr.GetStatus().GetSuspensionState() != storagev1alpha1pb.TransferStatus_NOT_SUSPENDED || tr.GetStatus().GetError() != "" {
		t.Errorf("status = %v", tr.GetStatus())
	}
	want := map[string]string{"out/a.txt": "alpha", "out/dir/b.txt": "bravo!"}
	if got := env.fake.contents(); !maps.Equal(got, want) {
		t.Errorf("destination = %v, want %v", got, want)
	}

	// Credentials are input-only.
	byName, err := svc.GetByName(ctx, &commonpb.GetByNameRequest{ParentId: env.inst.ProjectID, Name: "tr-alpha"})
	if err != nil || byName.GetMetadata().GetId() != id {
		t.Fatalf("GetByName = %v, %v", byName, err)
	}
	list, err := svc.List(ctx, &storagev1alpha1pb.ListTransfersRequest{ParentId: env.inst.ProjectID})
	if err != nil || len(list.GetItems()) != 1 {
		t.Fatalf("List = %v, %v", list, err)
	}
	for _, got := range []*storagev1alpha1pb.Transfer{tr, byName, list.GetItems()[0]} {
		if got.GetSpec().GetSource().GetCredentials() != nil || got.GetSpec().GetDestination().GetCredentials() != nil {
			t.Errorf("credentials returned: %v", got.GetSpec())
		}
		if got.GetSpec().GetSource().GetBucketName() != "src-bucket" || got.GetSpec().GetDestination().GetPrefix() != "out/" {
			t.Errorf("spec = %v", got.GetSpec())
		}
	}

	// The same transfer through the v1 API: an equivalent v1 spec, no secrets.
	v1 := storagesvc.NewTransferService(env.inst.SDK(t))
	v1tr := trGet(t, v1, id)
	s := v1tr.GetSpec()
	if v1tr.GetStatus().GetState() != storagepb.TransferStatus_STOPPED || v1tr.GetStatus().GetLastIteration().GetObjectsTransferredCount() != 2 ||
		s.GetSource().GetS3Compatible().GetEndpoint() != trSrcURL || s.GetSource().GetS3Compatible().GetBucketName() != "src-bucket" ||
		s.GetSource().GetPrefix() != "in/" || s.GetDestination().GetNebius().GetBucketName() != trDstBucket || s.GetDestination().GetNebius().GetRegion() != "eu-north1" ||
		s.GetDestination().GetPrefix() != "out/" || s.GetAfterOneIteration() == nil || s.GetOverwriteStrategy() != storagepb.TransferSpec_NEVER || !s.GetTouchUnmanaged() || s.GetEnableDeletesInDestination() {
		t.Errorf("v1 view = %v", v1tr)
	}
	for _, ak := range []*storagepb.TransferCredentialsAccessKey{s.GetSource().GetS3Compatible().GetAccessKey(), s.GetDestination().GetNebius().GetAccessKey()} {
		if ak.GetSecretAccessKey() != "" {
			t.Errorf("v1 view of a v1alpha1 transfer returned a secret")
		}
	}
	if _, err := svc.Get(ctx, &storagev1alpha1pb.GetTransferRequest{Id: "storagetransfer-e00doesnotexist"}); err == nil {
		t.Error("Get of a missing transfer succeeded")
	}
}

func TestTransferV1Alpha1StopResumeUpdateDelete(t *testing.T) {
	t.Parallel()
	env := startTransfers(t)
	svc := storagev1alpha1svc.NewTransferService(env.inst.SDK(t))
	ctx := t.Context()
	env.fake.put("in/a.txt", "one")
	id := trAlphaMustCreate(t, svc, env.inst.ProjectID, "tr-alpha-inf", trAlphaSpec(trAlphaInfinite))
	env.trAlphaAwait(t, svc, id, storagev1alpha1pb.TransferStatus_ACTIVE, 1, 0)

	// An ACTIVE transfer cannot be deleted through v1alpha1.
	_, err := svc.Delete(ctx, &storagev1alpha1pb.DeleteTransferRequest{Id: id})
	trWantCode(t, "Delete ACTIVE", err, codes.FailedPrecondition)

	// Update: limiters, interval and labels; an empty secret keeps the stored one.
	trOp(t, "Update", func() error {
		return trAlphaUpdate(ctx, svc, id, func(tr *storagev1alpha1pb.Transfer) {
			tr.Metadata.Labels = map[string]string{"env": "test"}
			tr.Spec.Source.Limiters = &storagev1alpha1pb.TransferSpec_Limiters{RequestsPerSecond: 5, BandwidthBytesPerSecond: 1 << 20}
			tr.Spec.InterIterationInterval = durationpb.New(2 * time.Minute)
		})
	})
	tr := trAlphaGet(t, svc, id)
	if tr.GetMetadata().GetResourceVersion() != 2 || tr.GetMetadata().GetLabels()["env"] != "test" || tr.GetSpec().GetSource().GetLimiters().GetRequestsPerSecond() != 5 ||
		tr.GetSpec().GetInterIterationInterval().AsDuration() != 2*time.Minute {
		t.Fatalf("after update = %v", tr)
	}
	env.fake.put("in/b.txt", "two")
	tr = env.trAlphaAwait(t, svc, id, storagev1alpha1pb.TransferStatus_ACTIVE, 2, 2*time.Minute)
	if tr.GetStatus().GetLastIteration().GetState() != storagev1alpha1pb.TransferIteration_COMPLETED || env.fake.contents()["out/b.txt"] != "two" {
		t.Errorf("iteration after the update = %v: the stored secret was lost", tr.GetStatus())
	}
	for name, tc := range map[string]struct {
		mutate func(*storagev1alpha1pb.Transfer)
		field  string
	}{
		"bucket":   {func(tr *storagev1alpha1pb.Transfer) { tr.Spec.Source.BucketName = "other" }, "bucket_name"},
		"prefix":   {func(tr *storagev1alpha1pb.Transfer) { tr.Spec.Destination.Prefix = "x/" }, "prefix"},
		"endpoint": {func(tr *storagev1alpha1pb.Transfer) { tr.Spec.Source.Endpoint = "https://other.example.com" }, "endpoint"},
	} {
		err := trAlphaUpdate(ctx, svc, id, tc.mutate)
		trWantCode(t, "immutable "+name, err, codes.InvalidArgument)
		if err != nil && !strings.Contains(err.Error(), tc.field) {
			t.Errorf("immutable %s: error %q does not name %q", name, err, tc.field)
		}
	}

	stop := func() error {
		op, err := svc.Stop(ctx, &storagev1alpha1pb.StopTransferRequest{Id: id})
		if err != nil {
			return err
		}
		_, err = op.Wait(ctx)
		return err
	}
	trOp(t, "Stop", stop)
	if got := trAlphaGet(t, svc, id).GetStatus().GetState(); got != storagev1alpha1pb.TransferStatus_STOPPED {
		t.Fatalf("after Stop: %v, want STOPPED", got)
	}
	trWantCode(t, "Stop again", stop(), codes.FailedPrecondition)
	trOp(t, "Resume", func() error {
		op, err := svc.Resume(ctx, &storagev1alpha1pb.ResumeTransferRequest{Id: id})
		if err != nil {
			return err
		}
		_, err = op.Wait(ctx)
		return err
	})
	env.trAlphaAwait(t, svc, id, storagev1alpha1pb.TransferStatus_ACTIVE, 3, 0)

	hist, err := svc.GetIterationHistory(ctx, &storagev1alpha1pb.GetIterationHistoryRequest{TransferId: id, PageSize: 2})
	if err != nil || len(hist.GetIterations()) != 2 || hist.GetNextPageToken() == "" {
		t.Fatalf("history page 1 = %v, %v", hist, err)
	}
	rest, err := svc.GetIterationHistory(ctx, &storagev1alpha1pb.GetIterationHistoryRequest{TransferId: id, PageSize: 2, PageToken: hist.GetNextPageToken()})
	if err != nil {
		t.Fatal(err)
	}
	var seqs []int64
	for _, it := range append(hist.GetIterations(), rest.GetIterations()...) {
		seqs = append(seqs, it.GetSequenceNumber())
	}
	if !slices.Equal(seqs, []int64{3, 2, 1}) || rest.GetNextPageToken() != "" {
		t.Errorf("history = %v, want 3,2,1 newest first", seqs)
	}

	trOp(t, "Stop before Delete", stop)
	if _, err := svc.Delete(ctx, &storagev1alpha1pb.DeleteTransferRequest{Id: id, ResourceVersion: 99}); err == nil {
		t.Error("Delete with a wrong resource_version succeeded")
	}
	op, err := svc.Delete(ctx, &storagev1alpha1pb.DeleteTransferRequest{Id: id})
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	_, err = svc.Get(ctx, &storagev1alpha1pb.GetTransferRequest{Id: id})
	trWantCode(t, "Get after delete", err, codes.NotFound)
}

func TestTransferV1Alpha1Validation(t *testing.T) {
	t.Parallel()
	env := startTransfers(t)
	svc := storagev1alpha1svc.NewTransferService(env.inst.SDK(t))
	for name, tc := range map[string]struct {
		mutate func(*storagev1alpha1pb.TransferSpec)
		code   codes.Code
		field  string
	}{
		"external to external": {func(s *storagev1alpha1pb.TransferSpec) {
			s.Destination.Endpoint, s.Destination.Region = "https://dst.example.com", "us-east-1"
		}, codes.InvalidArgument, "destination"},
		"ftp endpoint":      {func(s *storagev1alpha1pb.TransferSpec) { s.Source.Endpoint = "ftp://src.example.com" }, codes.InvalidArgument, "endpoint"},
		"no scheme":         {func(s *storagev1alpha1pb.TransferSpec) { s.Source.Endpoint = "src.example.com" }, codes.InvalidArgument, "endpoint"},
		"region not served": {func(s *storagev1alpha1pb.TransferSpec) { s.Destination.Region = "us-central1" }, codes.InvalidArgument, "region"},
		"missing bucket":    {func(s *storagev1alpha1pb.TransferSpec) { s.Destination.BucketName = "no-such-bucket" }, codes.InvalidArgument, "no-such-bucket"},
		"no destination":    {func(s *storagev1alpha1pb.TransferSpec) { s.Destination = nil }, codes.InvalidArgument, "destination"},
		"no stop condition": {func(s *storagev1alpha1pb.TransferSpec) { s.StopCondition = nil }, codes.InvalidArgument, "stop_condition"},
		"no secret":         {func(s *storagev1alpha1pb.TransferSpec) { s.Source.Credentials = trAlphaCreds("K", "") }, codes.InvalidArgument, "secret_access_key"},
		"no overwrite":      {func(s *storagev1alpha1pb.TransferSpec) { s.OverwriteStrategy = 0 }, codes.InvalidArgument, "overwrite_strategy"},
	} {
		spec := trAlphaSpec(trAlphaOnce)
		tc.mutate(spec)
		_, err := trAlphaCreate(t.Context(), svc, env.inst.ProjectID, "tr-invalid", spec)
		trWantCode(t, name, err, tc.code)
		if err != nil && !strings.Contains(err.Error(), tc.field) {
			t.Errorf("%s: error %q does not mention %q", name, err, tc.field)
		}
	}
}

// TestTransferVersionsShareProject covers the cross-version rules: names are
// unique across both APIs, the v1 API lists and gets transfers created through
// v1alpha1, and v1alpha1 only sees its own.
func TestTransferVersionsShareProject(t *testing.T) {
	t.Parallel()
	env := startTransfers(t)
	v1 := storagesvc.NewTransferService(env.inst.SDK(t))
	alpha := storagev1alpha1svc.NewTransferService(env.inst.SDK(t))
	ctx := t.Context()
	env.fake.setGate(make(chan struct{})) // keep the iterations from running

	alphaID := trAlphaMustCreate(t, alpha, env.inst.ProjectID, "tr-shared-a", trAlphaSpec(trAlphaInfinite))
	v1ID := trMustCreate(t, v1, env.inst.ProjectID, "tr-shared-b", trSpec(trInfinite))
	alphaID2 := trAlphaMustCreate(t, alpha, env.inst.ProjectID, "tr-shared-c", trAlphaSpec(trAlphaInfinite))

	if _, err := trCreate(ctx, v1, env.inst.ProjectID, "tr-shared-a", trSpec(trOnce)); err == nil {
		t.Error("v1 create reused a v1alpha1 transfer's name")
	}
	if _, err := trAlphaCreate(ctx, alpha, env.inst.ProjectID, "tr-shared-b", trAlphaSpec(trAlphaOnce)); err == nil {
		t.Error("v1alpha1 create reused a v1 transfer's name")
	}

	var v1Seen []string
	token := ""
	for {
		resp, err := v1.List(ctx, &storagepb.ListTransfersRequest{ParentId: env.inst.ProjectID, PageSize: 2, PageToken: token})
		if err != nil {
			t.Fatal(err)
		}
		for _, tr := range resp.GetItems() {
			v1Seen = append(v1Seen, tr.GetMetadata().GetId())
		}
		if token = resp.GetNextPageToken(); token == "" {
			break
		}
	}
	if want := []string{alphaID, v1ID, alphaID2}; !slices.Equal(v1Seen, want) {
		t.Errorf("v1 List = %v, want %v", v1Seen, want)
	}
	var alphaSeen []string
	token = ""
	for {
		resp, err := alpha.List(ctx, &storagev1alpha1pb.ListTransfersRequest{ParentId: env.inst.ProjectID, PageSize: 1, PageToken: token})
		if err != nil {
			t.Fatal(err)
		}
		for _, tr := range resp.GetItems() {
			alphaSeen = append(alphaSeen, tr.GetMetadata().GetId())
		}
		if token = resp.GetNextPageToken(); token == "" {
			break
		}
	}
	if want := []string{alphaID, alphaID2}; !slices.Equal(alphaSeen, want) {
		t.Errorf("v1alpha1 List = %v, want %v", alphaSeen, want)
	}

	// v1alpha1 does not see a v1 transfer.
	_, err := alpha.Get(ctx, &storagev1alpha1pb.GetTransferRequest{Id: v1ID})
	trWantCode(t, "v1alpha1 Get of a v1 transfer", err, codes.NotFound)
	_, err = alpha.GetByName(ctx, &commonpb.GetByNameRequest{ParentId: env.inst.ProjectID, Name: "tr-shared-b"})
	trWantCode(t, "v1alpha1 GetByName of a v1 transfer", err, codes.NotFound)
	_, err = alpha.GetIterationHistory(ctx, &storagev1alpha1pb.GetIterationHistoryRequest{TransferId: v1ID})
	trWantCode(t, "v1alpha1 history of a v1 transfer", err, codes.NotFound)

	// v1 gets the v1alpha1 transfer, but updating it is v1alpha1's job.
	byName, err := v1.GetByName(ctx, &commonpb.GetByNameRequest{ParentId: env.inst.ProjectID, Name: "tr-shared-a"})
	if err != nil || byName.GetMetadata().GetId() != alphaID || byName.GetSpec().GetInfinite() == nil {
		t.Errorf("v1 GetByName of a v1alpha1 transfer = %v, %v", byName, err)
	}
	err = trUpdate(ctx, v1, alphaID, func(tr *storagepb.Transfer) {
		tr.Spec.Limiters = &storagepb.TransferSpec_Limiters{RequestsPerSecond: 1}
	})
	trWantCode(t, "v1 Update of a v1alpha1 transfer", err, codes.FailedPrecondition)
	// v1 can stop and delete it.
	trOp(t, "v1 Stop", func() error { return trStop(ctx, v1, alphaID) })
	if got := trAlphaGet(t, alpha, alphaID).GetStatus().GetState(); got != storagev1alpha1pb.TransferStatus_STOPPED {
		t.Errorf("v1alpha1 view after a v1 Stop = %v, want STOPPED", got)
	}
}

func TestTransferV1Alpha1Authz(t *testing.T) {
	t.Parallel()
	env := startTransfers(t, testkit.WithAuthz(authz.ModeEnforce))
	admin := storagev1alpha1svc.NewTransferService(env.inst.SDK(t))
	viewer := storagev1alpha1svc.NewTransferService(env.inst.SDKAs(t, config.DefaultViewerToken))
	editor := storagev1alpha1svc.NewTransferService(env.inst.SDKAs(t, config.DefaultEditorToken))
	ctx := t.Context()
	env.fake.put("in/a.txt", "one")
	id := trAlphaMustCreate(t, admin, env.inst.ProjectID, "tr-alpha-authz", trAlphaSpec(trAlphaInfinite))
	env.trAlphaAwait(t, admin, id, storagev1alpha1pb.TransferStatus_ACTIVE, 1, 0)

	if _, err := viewer.Get(ctx, &storagev1alpha1pb.GetTransferRequest{Id: id}); err != nil {
		t.Errorf("viewer Get: %v", err)
	}
	if _, err := viewer.List(ctx, &storagev1alpha1pb.ListTransfersRequest{ParentId: env.inst.ProjectID}); err != nil {
		t.Errorf("viewer List: %v", err)
	}
	if _, err := viewer.GetIterationHistory(ctx, &storagev1alpha1pb.GetIterationHistoryRequest{TransferId: id}); err != nil {
		t.Errorf("viewer GetIterationHistory: %v", err)
	}
	_, err := viewer.Create(ctx, &storagev1alpha1pb.CreateTransferRequest{Metadata: &commonpb.ResourceMetadata{ParentId: env.inst.ProjectID, Name: "denied"}, Spec: trAlphaSpec(trAlphaOnce)})
	trWantCode(t, "viewer Create", err, codes.PermissionDenied)
	_, err = viewer.Stop(ctx, &storagev1alpha1pb.StopTransferRequest{Id: id})
	trWantCode(t, "viewer Stop", err, codes.PermissionDenied)
	_, err = viewer.Resume(ctx, &storagev1alpha1pb.ResumeTransferRequest{Id: id})
	trWantCode(t, "viewer Resume", err, codes.PermissionDenied)
	_, err = viewer.Delete(ctx, &storagev1alpha1pb.DeleteTransferRequest{Id: id})
	trWantCode(t, "viewer Delete", err, codes.PermissionDenied)

	op, err := editor.Stop(ctx, &storagev1alpha1pb.StopTransferRequest{Id: id})
	if err != nil {
		t.Fatalf("editor Stop: %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	op, err = editor.Resume(ctx, &storagev1alpha1pb.ResumeTransferRequest{Id: id})
	if err != nil {
		t.Fatalf("editor Resume: %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	trAlphaMustCreate(t, editor, env.inst.ProjectID, "tr-alpha-editor", trAlphaSpec(trAlphaOnce))
}
