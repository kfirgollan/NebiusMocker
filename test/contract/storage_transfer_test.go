package contract

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"io"
	"iter"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"
	storagesvc "github.com/nebius/gosdk/services/nebius/storage/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore/memory"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/services/storage"
	"github.com/kfirgollan/NebiusMocker/services/storage/domain"
)

const (
	trSrcURL    = "https://src.example.com"
	trSrcID     = "s3_compatible:" + trSrcURL + "/src-bucket"
	trDstBucket = "tr-dst"
	trDstID     = "nebius:eu-north1/" + trDstBucket
	trSrcSecret = "SRC-SECRET"
	trDstSecret = "DST-SECRET"
)

// trFake is an in-memory domain.Endpoints: every storage location (side
// identity) is a map of objects, and a location with a registered secret
// answers AccessDenied to any other secret, like a real S3 server would.
type trFake struct {
	clk *clock.Manual

	mu      sync.Mutex
	stores  map[string]map[string]*trObject
	secrets map[string]string
	// gate, when set, blocks every List until it is closed (or ctx ends).
	gate chan struct{}
}

type trObject struct {
	data []byte
	meta domain.ObjectMeta
}

func newTrFake(clk *clock.Manual) *trFake {
	return &trFake{
		clk:     clk,
		stores:  map[string]map[string]*trObject{},
		secrets: map[string]string{trSrcID: trSrcSecret, trDstID: trDstSecret},
	}
}

func (f *trFake) Open(_ context.Context, side domain.Side) (domain.Endpoint, error) {
	id := side.Identity()
	f.mu.Lock()
	defer f.mu.Unlock()
	if want, ok := f.secrets[id]; ok && side.Creds.Secret != want {
		return nil, &domain.EndpointError{Code: "AccessDenied", Message: "bad credentials for " + id}
	}
	return trEndpoint{f: f, id: id}, nil
}

// put stores an object in the source bucket, modified now.
func (f *trFake) put(key, data string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.putLocked(trSrcID, key, []byte(data), domain.ObjectMeta{})
}

func (f *trFake) putLocked(id, key string, data []byte, meta domain.ObjectMeta) string {
	sum := md5.Sum(data)
	meta.Key, meta.Size, meta.ETag, meta.LastModified = key, int64(len(data)), hex.EncodeToString(sum[:]), f.clk.Now()
	if f.stores[id] == nil {
		f.stores[id] = map[string]*trObject{}
	}
	f.stores[id][key] = &trObject{data: data, meta: meta}
	return meta.ETag
}

// contents returns the Nebius destination bucket as key → data.
func (f *trFake) contents() map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]string{}
	for k, o := range f.stores[trDstID] {
		out[k] = string(o.data)
	}
	return out
}

func (f *trFake) setGate(g chan struct{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gate = g
}

type trEndpoint struct {
	f  *trFake
	id string
}

func (e trEndpoint) List(ctx context.Context, prefix string) iter.Seq2[domain.ObjectMeta, error] {
	return func(yield func(domain.ObjectMeta, error) bool) {
		e.f.mu.Lock()
		gate := e.f.gate
		e.f.mu.Unlock()
		if gate != nil {
			select {
			case <-gate:
			case <-ctx.Done():
				yield(domain.ObjectMeta{}, context.Cause(ctx))
				return
			}
		}
		e.f.mu.Lock()
		var metas []domain.ObjectMeta
		for _, k := range slices.Sorted(maps.Keys(e.f.stores[e.id])) {
			if strings.HasPrefix(k, prefix) {
				metas = append(metas, e.f.stores[e.id][k].meta)
			}
		}
		e.f.mu.Unlock()
		for i := range metas {
			if !yield(metas[i], nil) {
				return
			}
		}
	}
}

func (e trEndpoint) Head(_ context.Context, key string) (domain.ObjectMeta, error) {
	e.f.mu.Lock()
	defer e.f.mu.Unlock()
	o, ok := e.f.stores[e.id][key]
	if !ok {
		return domain.ObjectMeta{}, errs.NotFound(key)
	}
	return o.meta, nil
}

func (e trEndpoint) Get(_ context.Context, key string) (io.ReadCloser, domain.ObjectMeta, error) {
	e.f.mu.Lock()
	defer e.f.mu.Unlock()
	o, ok := e.f.stores[e.id][key]
	if !ok {
		return nil, domain.ObjectMeta{}, errs.NotFound(key)
	}
	return io.NopCloser(bytes.NewReader(o.data)), o.meta, nil
}

func (e trEndpoint) Put(_ context.Context, key string, body io.Reader, meta domain.ObjectMeta) (string, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return "", err
	}
	e.f.mu.Lock()
	defer e.f.mu.Unlock()
	return e.f.putLocked(e.id, key, data, meta), nil
}

func (e trEndpoint) ReplaceMetadata(_ context.Context, key string, meta domain.ObjectMeta) (string, error) {
	e.f.mu.Lock()
	defer e.f.mu.Unlock()
	o, ok := e.f.stores[e.id][key]
	if !ok {
		return "", errs.NotFound(key)
	}
	return e.f.putLocked(e.id, key, o.data, meta), nil
}

func (e trEndpoint) Delete(_ context.Context, key string) error {
	e.f.mu.Lock()
	defer e.f.mu.Unlock()
	delete(e.f.stores[e.id], key)
	return nil
}

var trStart = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// trEnv is a mocker with the transfer endpoints replaced by a fake.
type trEnv struct {
	inst *testkit.Instance
	fake *trFake
	clk  *clock.Manual
}

func startTransfers(t *testing.T, opts ...testkit.Option) *trEnv {
	t.Helper()
	clk := clock.NewManual(trStart)
	fake := newTrFake(clk)
	inst := testkit.StartT(t, []mocker.Module{iam.New(), storage.New(storage.WithS3Listen("off"), storage.WithTransferEndpoints(fake))},
		append([]testkit.Option{testkit.WithObjStore(memory.New(clk)), testkit.WithClock(clk)}, opts...)...)
	env := &trEnv{inst: inst, fake: fake, clk: clk}
	createBucket(t, storagesvc.NewBucketService(inst.SDK(t)), inst.ProjectID, trDstBucket, nil)
	return env
}

// trSpec is a v1 spec copying src-bucket/in/ to the Nebius bucket under out/.
func trSpec(stop func(*storagepb.TransferSpec)) *storagepb.TransferSpec {
	spec := &storagepb.TransferSpec{
		Source: &storagepb.TransferSource{
			Prefix: "in/",
			Provider: &storagepb.TransferSource_S3Compatible{S3Compatible: &storagepb.TransferSource_S3CompatibleProvider{
				Endpoint: trSrcURL, Region: "us-east-1", BucketName: "src-bucket",
				Credentials: &storagepb.TransferSource_S3CompatibleProvider_AccessKey{AccessKey: &storagepb.TransferCredentialsAccessKey{
					AccessKeyId: "SRCKEY", SecretAccessKey: trSrcSecret,
				}},
			}},
		},
		Destination: &storagepb.TransferDestination{
			Prefix: "out/",
			Provider: &storagepb.TransferDestination_Nebius{Nebius: &storagepb.TransferDestination_NebiusProvider{
				Region: "eu-north1", BucketName: trDstBucket,
				Credentials: &storagepb.TransferDestination_NebiusProvider_AccessKey{AccessKey: &storagepb.TransferCredentialsAccessKey{
					AccessKeyId: "DSTKEY", SecretAccessKey: trDstSecret,
				}},
			}},
		},
		OverwriteStrategy: storagepb.TransferSpec_NEVER,
	}
	stop(spec)
	return spec
}

func trOnce(s *storagepb.TransferSpec) {
	s.StopCondition = &storagepb.TransferSpec_AfterOneIteration{AfterOneIteration: &storagepb.TransferSpec_StopConditionAfterOneIteration{}}
}

func trInfinite(s *storagepb.TransferSpec) {
	s.StopCondition = &storagepb.TransferSpec_Infinite{Infinite: &storagepb.TransferSpec_StopConditionInfinite{}}
	s.InterIterationInterval = durationpb.New(time.Minute)
}

func trCreate(ctx context.Context, svc storagesvc.TransferService, parent, name string, spec *storagepb.TransferSpec) (string, error) {
	op, err := svc.Create(ctx, &storagepb.CreateTransferRequest{Metadata: &commonpb.ResourceMetadata{ParentId: parent, Name: name}, Spec: spec})
	if err != nil {
		return "", err
	}
	if _, err := op.Wait(ctx); err != nil {
		return "", err
	}
	return op.ResourceID(), nil
}

func trMustCreate(t *testing.T, svc storagesvc.TransferService, parent, name string, spec *storagepb.TransferSpec) string {
	t.Helper()
	id, err := trCreate(t.Context(), svc, parent, name, spec)
	if err != nil {
		t.Fatalf("Create transfer %q: %v", name, err)
	}
	return id
}

func trGet(t *testing.T, svc storagesvc.TransferService, id string) *storagepb.Transfer {
	t.Helper()
	tr, err := svc.Get(t.Context(), &storagepb.GetTransferRequest{Id: id})
	if err != nil {
		t.Fatalf("Get transfer %s: %v", id, err)
	}
	return tr
}

// trAwait waits until the transfer is in state and its last iteration has
// sequence number seq, advancing the manual clock by tick before each check.
func (e *trEnv) trAwait(t *testing.T, svc storagesvc.TransferService, id string, state storagepb.TransferStatus_State, seq int64, tick time.Duration) *storagepb.Transfer {
	t.Helper()
	var tr *storagepb.Transfer
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("last status seen while waiting for %v/%d: %v", state, seq, tr.GetStatus())
		}
	})
	// Time moves slowly (every 10th poll) and only until the awaited iteration
	// started, so a fast runner cannot skip over it.
	var seen int64
	polls := 0
	poll(t, "transfer "+state.String(), func() {
		polls++
		if tick > 0 && seen < seq && polls%10 == 1 {
			e.clk.Advance(tick)
		}
	}, func() bool {
		var err error
		tr, err = svc.Get(t.Context(), &storagepb.GetTransferRequest{Id: id})
		seen = tr.GetStatus().GetLastIteration().GetSequenceNumber()
		return err == nil && tr.GetStatus().GetState() == state && tr.GetStatus().GetLastIteration().GetSequenceNumber() == seq &&
			tr.GetStatus().GetLastIteration().GetState() != storagepb.TransferIteration_IN_PROGRESS
	})
	return tr
}

func trNoTick() {}

func trOp(t *testing.T, what string, run func() error) {
	t.Helper()
	if err := run(); err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

func trStop(ctx context.Context, svc storagesvc.TransferService, id string) error {
	op, err := svc.Stop(ctx, &storagepb.StopTransferRequest{Id: id})
	if err != nil {
		return err
	}
	_, err = op.Wait(ctx)
	return err
}

func trResume(ctx context.Context, svc storagesvc.TransferService, id string) error {
	op, err := svc.Resume(ctx, &storagepb.ResumeTransferRequest{Id: id})
	if err != nil {
		return err
	}
	_, err = op.Wait(ctx)
	return err
}

func trWantCode(t *testing.T, what string, err error, want codes.Code) {
	t.Helper()
	if status.Code(err) != want {
		t.Errorf("%s: code %v (%v), want %v", what, status.Code(err), err, want)
	}
}

func TestTransferV1Lifecycle(t *testing.T) {
	t.Parallel()
	env := startTransfers(t)
	svc := storagesvc.NewTransferService(env.inst.SDK(t))
	ctx := t.Context()
	env.fake.put("in/a.txt", "alpha")
	env.fake.put("in/dir/b.txt", "bravo!")
	env.fake.put("other/c.txt", "not under the prefix")
	gate := make(chan struct{})
	env.fake.setGate(gate)

	id := trMustCreate(t, svc, env.inst.ProjectID, "tr-once", trSpec(trOnce))
	if !strings.HasPrefix(id, "storagetransfer-") {
		t.Errorf("id = %q, want storagetransfer-...", id)
	}
	poll(t, "the first iteration to start", trNoTick, func() bool {
		return trGet(t, svc, id).GetStatus().GetLastIteration().GetState() == storagepb.TransferIteration_IN_PROGRESS
	})
	tr := trGet(t, svc, id)
	st := tr.GetStatus()
	if st.GetState() != storagepb.TransferStatus_ACTIVE || st.GetSuspensionState() != storagepb.TransferStatus_NOT_SUSPENDED ||
		st.GetLastIteration().GetSequenceNumber() != 1 || st.GetLastIteration().GetEndTime() != nil {
		t.Fatalf("running status = %v", st)
	}
	md := tr.GetMetadata()
	if md.GetParentId() != env.inst.ProjectID || md.GetName() != "tr-once" || md.GetResourceVersion() != 1 || md.GetCreatedAt() == nil {
		t.Errorf("metadata = %v", md)
	}
	if got := tr.GetSpec().GetInterIterationInterval().AsDuration(); got != 15*time.Minute {
		t.Errorf("default inter_iteration_interval = %v, want 15m", got)
	}

	close(gate)
	tr = env.trAwait(t, svc, id, storagepb.TransferStatus_STOPPED, 1, 0)
	it := tr.GetStatus().GetLastIteration()
	if it.GetState() != storagepb.TransferIteration_COMPLETED || it.GetObjectsTransferredCount() != 2 ||
		it.GetObjectsTransferredSize() != int64(len("alpha")+len("bravo!")) || it.GetEndTime() == nil || it.GetStartTime() == nil {
		t.Errorf("iteration = %v, want 2 objects transferred", it)
	}
	want := map[string]string{"out/a.txt": "alpha", "out/dir/b.txt": "bravo!"}
	if got := env.fake.contents(); !maps.Equal(got, want) {
		t.Errorf("destination = %v, want %v", got, want)
	}

	// Secrets are input-only: no get returns them.
	byName, err := svc.GetByName(ctx, &commonpb.GetByNameRequest{ParentId: env.inst.ProjectID, Name: "tr-once"})
	if err != nil || byName.GetMetadata().GetId() != id {
		t.Fatalf("GetByName = %v, %v", byName, err)
	}
	for _, got := range []*storagepb.Transfer{tr, byName} {
		src := got.GetSpec().GetSource().GetS3Compatible().GetAccessKey()
		dst := got.GetSpec().GetDestination().GetNebius().GetAccessKey()
		if src.GetAccessKeyId() != "SRCKEY" || src.GetSecretAccessKey() != "" || dst.GetAccessKeyId() != "DSTKEY" || dst.GetSecretAccessKey() != "" {
			t.Errorf("credentials = %v / %v, want key ids without secrets", src, dst)
		}
	}
	if _, err := svc.GetByName(ctx, &commonpb.GetByNameRequest{ParentId: env.inst.ProjectID, Name: "missing"}); status.Code(err) != codes.NotFound {
		t.Errorf("GetByName missing: code %v, want NotFound", status.Code(err))
	}
	if _, err := svc.Get(ctx, &storagepb.GetTransferRequest{Id: "storagetransfer-e00doesnotexist"}); status.Code(err) != codes.NotFound {
		t.Errorf("Get missing: code %v, want NotFound", status.Code(err))
	}
	if _, err := trCreate(ctx, svc, env.inst.ProjectID, "tr-once", trSpec(trOnce)); status.Code(err) != codes.AlreadyExists {
		t.Errorf("duplicate name: code %v, want AlreadyExists", status.Code(err))
	}
}

func TestTransferV1StopResumeHistory(t *testing.T) {
	t.Parallel()
	env := startTransfers(t)
	svc := storagesvc.NewTransferService(env.inst.SDK(t))
	ctx := t.Context()
	env.fake.put("in/a.txt", "one")

	id := trMustCreate(t, svc, env.inst.ProjectID, "tr-inf", trSpec(trInfinite))
	tr := env.trAwait(t, svc, id, storagepb.TransferStatus_ACTIVE, 1, 0)
	if tr.GetStatus().GetLastIteration().GetObjectsTransferredCount() != 1 {
		t.Fatalf("iteration 1 = %v", tr.GetStatus().GetLastIteration())
	}

	// Within the interval nothing runs; once it passed, iteration 2 copies the new object.
	env.clk.Advance(30 * time.Second)
	env.fake.put("in/b.txt", "two")
	env.trAwait(t, svc, id, storagepb.TransferStatus_ACTIVE, 2, 30*time.Second)
	if got := env.fake.contents(); len(got) != 2 || got["out/b.txt"] != "two" {
		t.Errorf("destination after iteration 2 = %v", got)
	}

	trOp(t, "Stop", func() error { return trStop(ctx, svc, id) })
	tr = trGet(t, svc, id)
	if tr.GetStatus().GetState() != storagepb.TransferStatus_STOPPED {
		t.Fatalf("after Stop: state %v, want STOPPED", tr.GetStatus().GetState())
	}
	trWantCode(t, "Stop again", trStop(ctx, svc, id), codes.FailedPrecondition)
	// A stopped transfer runs no iteration, however much time passes.
	env.clk.Advance(time.Hour)
	if got := trGet(t, svc, id).GetStatus().GetLastIteration().GetSequenceNumber(); got != 2 {
		t.Errorf("iteration after the stop = %d, want 2", got)
	}

	env.fake.put("in/c.txt", "three")
	trOp(t, "Resume", func() error { return trResume(ctx, svc, id) })
	tr = env.trAwait(t, svc, id, storagepb.TransferStatus_ACTIVE, 3, 0)
	if tr.GetStatus().GetLastIteration().GetObjectsTransferredCount() != 1 || env.fake.contents()["out/c.txt"] != "three" {
		t.Errorf("iteration 3 = %v", tr.GetStatus().GetLastIteration())
	}
	trWantCode(t, "Resume an ACTIVE transfer", trResume(ctx, svc, id), codes.FailedPrecondition)

	hist, err := svc.GetIterationHistory(ctx, &storagepb.GetIterationHistoryRequest{TransferId: id})
	if err != nil {
		t.Fatal(err)
	}
	var seqs []int64
	for _, it := range hist.GetIterations() {
		seqs = append(seqs, it.GetSequenceNumber())
	}
	if !slices.Equal(seqs, []int64{3, 2, 1}) || hist.GetNextPageToken() != "" {
		t.Fatalf("history = %v (next %q), want 3,2,1 newest first", seqs, hist.GetNextPageToken())
	}
	page1, err := svc.GetIterationHistory(ctx, &storagepb.GetIterationHistoryRequest{TransferId: id, PageSize: 2})
	if err != nil || len(page1.GetIterations()) != 2 || page1.GetNextPageToken() == "" {
		t.Fatalf("history page 1 = %v, %v", page1, err)
	}
	page2, err := svc.GetIterationHistory(ctx, &storagepb.GetIterationHistoryRequest{TransferId: id, PageSize: 2, PageToken: page1.GetNextPageToken()})
	if err != nil || len(page2.GetIterations()) != 1 || page2.GetIterations()[0].GetSequenceNumber() != 1 || page2.GetNextPageToken() != "" {
		t.Fatalf("history page 2 = %v, %v", page2, err)
	}
	if _, err := svc.GetIterationHistory(ctx, &storagepb.GetIterationHistoryRequest{TransferId: "storagetransfer-e00doesnotexist"}); status.Code(err) != codes.NotFound {
		t.Errorf("history of a missing transfer: code %v, want NotFound", status.Code(err))
	}
}

func TestTransferV1StopRunningIteration(t *testing.T) {
	t.Parallel()
	env := startTransfers(t)
	svc := storagesvc.NewTransferService(env.inst.SDK(t))
	ctx := t.Context()
	env.fake.put("in/a.txt", "one")
	env.fake.setGate(make(chan struct{})) // the iteration never gets past listing

	id := trMustCreate(t, svc, env.inst.ProjectID, "tr-running", trSpec(trInfinite))
	poll(t, "the iteration to start", trNoTick, func() bool {
		return trGet(t, svc, id).GetStatus().GetLastIteration().GetState() == storagepb.TransferIteration_IN_PROGRESS
	})
	trOp(t, "Stop", func() error { return trStop(ctx, svc, id) })
	tr := trGet(t, svc, id)
	if tr.GetStatus().GetState() != storagepb.TransferStatus_STOPPED || tr.GetStatus().GetLastIteration().GetState() != storagepb.TransferIteration_INTERRUPTED {
		t.Errorf("status after stopping a running iteration = %v, want STOPPED with an INTERRUPTED iteration", tr.GetStatus())
	}
}

func TestTransferV1FailedIteration(t *testing.T) {
	t.Parallel()
	env := startTransfers(t)
	svc := storagesvc.NewTransferService(env.inst.SDK(t))
	env.fake.put("in/a.txt", "one")
	spec := trSpec(trOnce)
	spec.Source.GetS3Compatible().GetAccessKey().SecretAccessKey = "WRONG"

	id := trMustCreate(t, svc, env.inst.ProjectID, "tr-denied", spec)
	tr := env.trAwait(t, svc, id, storagepb.TransferStatus_FAILED, 1, 0)
	e := tr.GetStatus().GetError()
	if e.GetOrigin() != storagepb.TransferError_SOURCE || e.GetCode() != "AccessDenied" {
		t.Errorf("error = %v, want a SOURCE AccessDenied", e)
	}
	if it := tr.GetStatus().GetLastIteration(); it.GetState() != storagepb.TransferIteration_FAILED || it.GetError().GetCode() != "AccessDenied" {
		t.Errorf("iteration = %v", it)
	}
	// A FAILED transfer can be resumed once the credentials are fixed.
	trOp(t, "Update", func() error {
		return trUpdate(t.Context(), svc, id, func(tr *storagepb.Transfer) {
			tr.Spec.GetSource().GetS3Compatible().GetAccessKey().SecretAccessKey = trSrcSecret
		})
	})
	trOp(t, "Resume", func() error { return trResume(t.Context(), svc, id) })
	tr = env.trAwait(t, svc, id, storagepb.TransferStatus_STOPPED, 2, 0)
	if tr.GetStatus().GetError() != nil || tr.GetStatus().GetLastIteration().GetObjectsTransferredCount() != 1 {
		t.Errorf("after fixing the secret: %v", tr.GetStatus())
	}
}

func TestTransferV1Update(t *testing.T) {
	t.Parallel()
	env := startTransfers(t)
	svc := storagesvc.NewTransferService(env.inst.SDK(t))
	ctx := t.Context()
	env.fake.put("in/a.txt", "one")
	id := trMustCreate(t, svc, env.inst.ProjectID, "tr-update", trSpec(trInfinite))
	env.trAwait(t, svc, id, storagepb.TransferStatus_ACTIVE, 1, 0)

	// The SDK sends a full resource: the client edits what Get returned, whose
	// secrets are empty, so every update below also proves that an empty
	// secret keeps the stored one.
	trOp(t, "Update", func() error {
		return trUpdate(ctx, svc, id, func(tr *storagepb.Transfer) {
			tr.Metadata.Name, tr.Metadata.Labels = "tr-renamed", map[string]string{"env": "test"}
			tr.Spec.Limiters = &storagepb.TransferSpec_Limiters{RequestsPerSecond: 10, BandwidthBytesPerSecond: 1 << 20}
			tr.Spec.InterIterationInterval = durationpb.New(5 * time.Minute)
		})
	})
	tr := trGet(t, svc, id)
	spec := tr.GetSpec()
	if tr.GetMetadata().GetName() != "tr-renamed" || tr.GetMetadata().GetLabels()["env"] != "test" || tr.GetMetadata().GetResourceVersion() != 2 ||
		spec.GetLimiters().GetRequestsPerSecond() != 10 || spec.GetLimiters().GetBandwidthBytesPerSecond() != 1<<20 ||
		spec.GetInterIterationInterval().AsDuration() != 5*time.Minute || spec.GetInfinite() == nil || spec.GetSource().GetPrefix() != "in/" {
		t.Fatalf("after update = %v", tr)
	}
	byName, err := svc.GetByName(ctx, &commonpb.GetByNameRequest{ParentId: env.inst.ProjectID, Name: "tr-renamed"})
	if err != nil || byName.GetMetadata().GetId() != id {
		t.Errorf("GetByName(new name) = %v, %v", byName, err)
	}
	trOp(t, "clear labels", func() error {
		return trUpdate(ctx, svc, id, func(tr *storagepb.Transfer) { tr.Metadata.Labels = nil })
	})
	if got := trGet(t, svc, id).GetMetadata().GetLabels(); len(got) != 0 {
		t.Errorf("labels after clearing = %v, want none", got)
	}

	// The stored secrets are still used by the next iteration (the fake
	// answers AccessDenied to any other secret).
	env.fake.put("in/b.txt", "two")
	tr = env.trAwait(t, svc, id, storagepb.TransferStatus_ACTIVE, 2, 5*time.Minute)
	if tr.GetStatus().GetLastIteration().GetState() != storagepb.TransferIteration_COMPLETED || env.fake.contents()["out/b.txt"] != "two" {
		t.Errorf("iteration after the updates = %v: the stored secret was lost", tr.GetStatus())
	}

	// Immutable fields and bad values.
	for name, tc := range map[string]struct {
		mutate func(*storagepb.Transfer)
		field  string
	}{
		"source prefix":     {func(tr *storagepb.Transfer) { tr.Spec.Source.Prefix = "other/" }, "prefix"},
		"destination bkt":   {func(tr *storagepb.Transfer) { tr.Spec.Destination.GetNebius().BucketName = "elsewhere" }, "bucket_name"},
		"overwrite":         {func(tr *storagepb.Transfer) { tr.Spec.OverwriteStrategy = storagepb.TransferSpec_IF_NEWER }, "overwrite_strategy"},
		"deletes":           {func(tr *storagepb.Transfer) { tr.Spec.EnableDeletesInDestination = true }, "enable_deletes_in_destination"},
		"touch unmanaged":   {func(tr *storagepb.Transfer) { tr.Spec.TouchUnmanaged = true }, "touch_unmanaged"},
		"negative interval": {func(tr *storagepb.Transfer) { tr.Spec.InterIterationInterval = durationpb.New(-time.Second) }, "interval"},
	} {
		err := trUpdate(ctx, svc, id, tc.mutate)
		trWantCode(t, name, err, codes.InvalidArgument)
		if err != nil && !strings.Contains(err.Error(), tc.field) {
			t.Errorf("%s: error %q does not name %q", name, err, tc.field)
		}
	}
	trWantCode(t, "stale resource_version", trUpdate(ctx, svc, id, func(tr *storagepb.Transfer) { tr.Metadata.ResourceVersion = 1 }), codes.Aborted)
	_, err = svc.Update(ctx, &storagepb.UpdateTransferRequest{Metadata: &commonpb.ResourceMetadata{}, Spec: trSpec(trOnce)})
	trWantCode(t, "missing id", err, codes.InvalidArgument)
	if got := trGet(t, svc, id); got.GetMetadata().GetResourceVersion() != 3 {
		t.Errorf("resource_version = %d after failed updates, want 3", got.GetMetadata().GetResourceVersion())
	}
}

// trUpdate reads the transfer, applies mutate and sends the result as an
// update, like the Terraform provider does.
func trUpdate(ctx context.Context, svc storagesvc.TransferService, id string, mutate func(*storagepb.Transfer)) error {
	tr, err := svc.Get(ctx, &storagepb.GetTransferRequest{Id: id})
	if err != nil {
		return err
	}
	mutate(tr)
	op, err := svc.Update(ctx, &storagepb.UpdateTransferRequest{Metadata: tr.GetMetadata(), Spec: tr.GetSpec()})
	if err != nil {
		return err
	}
	_, err = op.Wait(ctx)
	return err
}

func TestTransferV1Delete(t *testing.T) {
	t.Parallel()
	env := startTransfers(t)
	svc := storagesvc.NewTransferService(env.inst.SDK(t))
	ctx := t.Context()
	env.fake.put("in/a.txt", "one")
	id := trMustCreate(t, svc, env.inst.ProjectID, "tr-delete", trSpec(trInfinite))
	env.trAwait(t, svc, id, storagepb.TransferStatus_ACTIVE, 1, 0)

	if _, err := svc.Delete(ctx, &storagepb.DeleteTransferRequest{Id: id, ResourceVersion: 99}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("Delete with a wrong resource_version: code %v (%v), want FailedPrecondition", status.Code(err), err)
	}
	// v1 deletes ACTIVE transfers, interrupting the running iteration if any.
	op, err := svc.Delete(ctx, &storagepb.DeleteTransferRequest{Id: id, ResourceVersion: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Get(ctx, &storagepb.GetTransferRequest{Id: id}); status.Code(err) != codes.NotFound {
		t.Errorf("Get after delete: code %v, want NotFound", status.Code(err))
	}
	if _, err := svc.GetIterationHistory(ctx, &storagepb.GetIterationHistoryRequest{TransferId: id}); status.Code(err) != codes.NotFound {
		t.Errorf("history after delete: code %v, want NotFound", status.Code(err))
	}
	if _, err := svc.Delete(ctx, &storagepb.DeleteTransferRequest{Id: id}); status.Code(err) != codes.NotFound {
		t.Errorf("Delete again: code %v, want NotFound", status.Code(err))
	}
	trMustCreate(t, svc, env.inst.ProjectID, "tr-delete", trSpec(trOnce)) // the name is free again
}

func TestTransferV1ListPaging(t *testing.T) {
	t.Parallel()
	env := startTransfers(t)
	svc := storagesvc.NewTransferService(env.inst.SDK(t))
	ctx := t.Context()
	env.fake.setGate(make(chan struct{})) // keep the iterations from running
	var ids []string
	for _, name := range []string{"tr-list-a", "tr-list-b", "tr-list-c"} {
		ids = append(ids, trMustCreate(t, svc, env.inst.ProjectID, name, trSpec(trInfinite)))
	}

	var got []string
	token := ""
	pages := 0
	for {
		resp, err := svc.List(ctx, &storagepb.ListTransfersRequest{ParentId: env.inst.ProjectID, PageSize: 2, PageToken: token})
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, tr := range resp.GetItems() {
			got = append(got, tr.GetMetadata().GetId())
			if tr.GetStatus().GetState() != storagepb.TransferStatus_ACTIVE {
				t.Errorf("listed transfer status = %v", tr.GetStatus())
			}
		}
		if token = resp.GetNextPageToken(); token == "" {
			break
		}
	}
	if !slices.Equal(got, ids) || pages != 2 {
		t.Errorf("listed %v in %d pages, want %v in 2", got, pages, ids)
	}
	if _, err := svc.List(ctx, &storagepb.ListTransfersRequest{ParentId: env.inst.ProjectID, PageSize: -1}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("negative page_size: code %v, want InvalidArgument", status.Code(err))
	}
	empty, err := svc.List(ctx, &storagepb.ListTransfersRequest{ParentId: env.inst.TenantID})
	if err != nil || len(empty.GetItems()) != 0 {
		t.Errorf("List of another parent = %v, %v", empty, err)
	}
}

func TestTransferV1Validation(t *testing.T) {
	t.Parallel()
	env := startTransfers(t)
	svc := storagesvc.NewTransferService(env.inst.SDK(t))
	src := func(s *storagepb.TransferSpec) *storagepb.TransferSource_S3CompatibleProvider {
		return s.GetSource().GetS3Compatible()
	}
	dst := func(s *storagepb.TransferSpec) *storagepb.TransferDestination_NebiusProvider {
		return s.GetDestination().GetNebius()
	}
	for name, tc := range map[string]struct {
		mutate func(*storagepb.TransferSpec)
		code   codes.Code
		field  string
	}{
		"external to external": {func(s *storagepb.TransferSpec) {
			s.Destination.Provider = &storagepb.TransferDestination_S3Compatible{S3Compatible: &storagepb.TransferDestination_S3CompatibleProvider{
				Endpoint: "https://dst.example.com", Region: "us-east-1", BucketName: "ext",
				Credentials: &storagepb.TransferDestination_S3CompatibleProvider_Anonymous{Anonymous: &storagepb.TransferCredentialsAnonymous{}},
			}}
		}, codes.InvalidArgument, "bucket"},
		"ftp endpoint":      {func(s *storagepb.TransferSpec) { src(s).Endpoint = "ftp://src.example.com" }, codes.InvalidArgument, "endpoint"},
		"no scheme":         {func(s *storagepb.TransferSpec) { src(s).Endpoint = "src.example.com" }, codes.InvalidArgument, "endpoint"},
		"trailing slash":    {func(s *storagepb.TransferSpec) { src(s).Endpoint = trSrcURL + "/" }, codes.InvalidArgument, "endpoint"},
		"region not served": {func(s *storagepb.TransferSpec) { dst(s).Region = "us-central1" }, codes.InvalidArgument, "region"},
		"missing bucket":    {func(s *storagepb.TransferSpec) { dst(s).BucketName = "no-such-bucket" }, codes.InvalidArgument, "no-such-bucket"},
		"no stop condition": {func(s *storagepb.TransferSpec) { s.StopCondition = nil }, codes.InvalidArgument, "stop_condition"},
		"zero threshold": {func(s *storagepb.TransferSpec) {
			s.StopCondition = &storagepb.TransferSpec_AfterNEmptyIterations{AfterNEmptyIterations: &storagepb.TransferSpec_StopConditionAfterNEmptyIterations{}}
		}, codes.InvalidArgument, "threshold"},
		"no overwrite":      {func(s *storagepb.TransferSpec) { s.OverwriteStrategy = 0 }, codes.InvalidArgument, "overwrite_strategy"},
		"no source secret":  {func(s *storagepb.TransferSpec) { src(s).GetAccessKey().SecretAccessKey = "" }, codes.InvalidArgument, "secret_access_key"},
		"no destination":    {func(s *storagepb.TransferSpec) { s.Destination = nil }, codes.InvalidArgument, "destination"},
		"negative interval": {func(s *storagepb.TransferSpec) { s.InterIterationInterval = durationpb.New(-time.Second) }, codes.InvalidArgument, "interval"},
	} {
		spec := trSpec(trOnce)
		tc.mutate(spec)
		_, err := trCreate(t.Context(), svc, env.inst.ProjectID, "tr-invalid", spec)
		trWantCode(t, name, err, tc.code)
		if err != nil && !strings.Contains(err.Error(), tc.field) {
			t.Errorf("%s: error %q does not mention %q", name, err, tc.field)
		}
	}
	if _, err := svc.Create(t.Context(), &storagepb.CreateTransferRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: "project-e00mock00000000missing", Name: "orphan"}, Spec: trSpec(trOnce),
	}); status.Code(err) != codes.NotFound {
		t.Errorf("missing project: code %v, want NotFound", status.Code(err))
	}
	// None of the failed creations left a transfer behind.
	if list, err := svc.List(t.Context(), &storagepb.ListTransfersRequest{ParentId: env.inst.ProjectID}); err != nil || len(list.GetItems()) != 0 {
		t.Errorf("transfers after failed creates = %v, %v", list, err)
	}
}

func TestTransferAuthz(t *testing.T) {
	t.Parallel()
	env := startTransfers(t, testkit.WithAuthz(authz.ModeEnforce))
	admin := storagesvc.NewTransferService(env.inst.SDK(t))
	viewer := storagesvc.NewTransferService(env.inst.SDKAs(t, config.DefaultViewerToken))
	editor := storagesvc.NewTransferService(env.inst.SDKAs(t, config.DefaultEditorToken))
	ctx := t.Context()
	env.fake.put("in/a.txt", "one")
	id := trMustCreate(t, admin, env.inst.ProjectID, "tr-authz", trSpec(trInfinite))
	env.trAwait(t, admin, id, storagepb.TransferStatus_ACTIVE, 1, 0)

	if _, err := viewer.Get(ctx, &storagepb.GetTransferRequest{Id: id}); err != nil {
		t.Errorf("viewer Get: %v", err)
	}
	if _, err := viewer.GetByName(ctx, &commonpb.GetByNameRequest{ParentId: env.inst.ProjectID, Name: "tr-authz"}); err != nil {
		t.Errorf("viewer GetByName: %v", err)
	}
	if _, err := viewer.List(ctx, &storagepb.ListTransfersRequest{ParentId: env.inst.ProjectID}); err != nil {
		t.Errorf("viewer List: %v", err)
	}
	if _, err := viewer.GetIterationHistory(ctx, &storagepb.GetIterationHistoryRequest{TransferId: id}); err != nil {
		t.Errorf("viewer GetIterationHistory: %v", err)
	}
	_, err := viewer.Create(ctx, &storagepb.CreateTransferRequest{Metadata: &commonpb.ResourceMetadata{ParentId: env.inst.ProjectID, Name: "denied"}, Spec: trSpec(trOnce)})
	trWantCode(t, "viewer Create", err, codes.PermissionDenied)
	_, err = viewer.Update(ctx, &storagepb.UpdateTransferRequest{Metadata: &commonpb.ResourceMetadata{Id: id}, Spec: &storagepb.TransferSpec{}})
	trWantCode(t, "viewer Update", err, codes.PermissionDenied)
	trWantCode(t, "viewer Stop", trStop(ctx, viewer, id), codes.PermissionDenied)
	trWantCode(t, "viewer Resume", trResume(ctx, viewer, id), codes.PermissionDenied)
	_, err = viewer.Delete(ctx, &storagepb.DeleteTransferRequest{Id: id})
	trWantCode(t, "viewer Delete", err, codes.PermissionDenied)

	trOp(t, "editor Stop", func() error { return trStop(ctx, editor, id) })
	trOp(t, "editor Resume", func() error { return trResume(ctx, editor, id) })
	trMustCreate(t, editor, env.inst.ProjectID, "tr-editor", trSpec(trOnce))
	if _, err := editor.GetIterationHistory(ctx, &storagepb.GetIterationHistoryRequest{TransferId: id}); err != nil {
		t.Errorf("editor GetIterationHistory: %v", err)
	}
	op, err := editor.Delete(ctx, &storagepb.DeleteTransferRequest{Id: id})
	if err != nil {
		t.Fatalf("editor Delete: %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
}
