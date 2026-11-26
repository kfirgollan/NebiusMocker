package domain_test

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"
	storagev1alpha1pb "github.com/nebius/gosdk/proto/nebius/storage/v1alpha1"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/services/storage/domain"
)

const (
	trRegion      = "eu-north1"
	trOtherProj   = "project-e00test0000000000002"
	trDstBucket   = "dst-bucket"
	trSrcEndpoint = "https://s3.example.com"
)

// trRepo is a map-backed domain.TransferRepository with per-project names.
type trRepo struct {
	mu    sync.Mutex
	order []string
	recs  map[string]*domain.TransferRecord
	its   map[string]map[int64]domain.Iteration
}

func newTrRepo() *trRepo {
	return &trRepo{recs: map[string]*domain.TransferRecord{}, its: map[string]map[int64]domain.Iteration{}}
}

func trClone(r *domain.TransferRecord) *domain.TransferRecord {
	c := *r
	if r.V1 != nil {
		c.V1 = proto.CloneOf(r.V1)
	}
	if r.Alpha != nil {
		c.Alpha = proto.CloneOf(r.Alpha)
	}
	if r.Status.Error != nil {
		e := *r.Status.Error
		c.Status.Error = &e
	}
	if r.Status.LastIteration != nil {
		it := *r.Status.LastIteration
		c.Status.LastIteration = &it
	}
	return &c
}

func (f *trRepo) Get(_ context.Context, id string) (*domain.TransferRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.recs[id]
	if !ok {
		return nil, errs.NotFound(id)
	}
	return trClone(r), nil
}

func (f *trRepo) GetByName(_ context.Context, parentID, name string) (*domain.TransferRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r := f.byName(parentID, name); r != nil {
		return trClone(r), nil
	}
	return nil, errs.NotFound(name)
}

func (f *trRepo) byName(parentID, name string) *domain.TransferRecord {
	for _, id := range f.order {
		md := f.recs[id].Metadata()
		if md.GetParentId() == parentID && md.GetName() == name {
			return f.recs[id]
		}
	}
	return nil
}

func (f *trRepo) List(_ context.Context, parentID string, only domain.TransferAPI, pageSize int, pageToken string) (domain.TransferPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	start := 0
	if pageToken != "" {
		start, _ = strconv.Atoi(pageToken)
	}
	var page domain.TransferPage
	for i := start; i < len(f.order); i++ {
		r := f.recs[f.order[i]]
		if r.Metadata().GetParentId() != parentID || only != 0 && r.API != only {
			continue
		}
		if len(page.Items) == pageSize {
			page.NextPageToken = strconv.Itoa(i)
			break
		}
		page.Items = append(page.Items, trClone(r))
	}
	return page, nil
}

func (f *trRepo) ListAll(context.Context) ([]*domain.TransferRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*domain.TransferRecord, 0, len(f.order))
	for _, id := range f.order {
		out = append(out, trClone(f.recs[id]))
	}
	return out, nil
}

func (f *trRepo) Create(_ context.Context, r *domain.TransferRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	md := r.Metadata()
	if other := f.byName(md.GetParentId(), md.GetName()); other != nil {
		return errs.AlreadyExists(other.Metadata().GetId(), "name taken")
	}
	f.order = append(f.order, md.GetId())
	f.recs[md.GetId()] = trClone(r)
	return nil
}

func (f *trRepo) Update(_ context.Context, r *domain.TransferRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	md := r.Metadata()
	if _, ok := f.recs[md.GetId()]; !ok {
		return errs.NotFound(md.GetId())
	}
	if other := f.byName(md.GetParentId(), md.GetName()); other != nil && other.Metadata().GetId() != md.GetId() {
		return errs.AlreadyExists(other.Metadata().GetId(), "name taken")
	}
	f.recs[md.GetId()] = trClone(r)
	return nil
}

func (f *trRepo) Delete(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.recs[id]; !ok {
		return errs.NotFound(id)
	}
	delete(f.recs, id)
	delete(f.its, id)
	f.order = slices.DeleteFunc(f.order, func(s string) bool { return s == id })
	return nil
}

func (f *trRepo) PutIteration(_ context.Context, id string, it domain.Iteration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.recs[id]; !ok {
		return errs.NotFound(id)
	}
	if f.its[id] == nil {
		f.its[id] = map[int64]domain.Iteration{}
	}
	f.its[id][it.Seq] = it
	return nil
}

func (f *trRepo) History(_ context.Context, id string, pageSize int, pageToken string) (domain.IterationPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var before int64
	if pageToken != "" {
		before, _ = strconv.ParseInt(pageToken, 10, 64)
	}
	var its []domain.Iteration
	for _, it := range f.its[id] {
		if before == 0 || it.Seq < before {
			its = append(its, it)
		}
	}
	slices.SortFunc(its, func(a, b domain.Iteration) int { return cmp.Compare(b.Seq, a.Seq) })
	var page domain.IterationPage
	if len(its) > pageSize {
		its = its[:pageSize]
		page.NextPageToken = strconv.FormatInt(its[len(its)-1].Seq, 10)
	}
	page.Items = its
	return page, nil
}

// trCall is one engine invocation.
type trCall struct {
	plan      domain.Plan
	seq       int64
	watermark time.Time
}

// trEngine is a fake iteration engine; run decides each iteration's result.
type trEngine struct {
	mu    sync.Mutex
	calls []trCall
	run   func(ctx context.Context, seq int64, progress func(domain.Iteration)) domain.Iteration
	clk   clock.Clock
}

func (e *trEngine) Run(ctx context.Context, plan domain.Plan, seq int64, wm time.Time, progress func(domain.Iteration)) (domain.Iteration, time.Time) {
	e.mu.Lock()
	e.calls = append(e.calls, trCall{plan: plan, seq: seq, watermark: wm})
	run := e.run
	e.mu.Unlock()
	start := e.clk.Now()
	it := domain.Iteration{Seq: seq, State: domain.IterationCompleted, Transferred: 1}
	if run != nil {
		it = run(ctx, seq, progress)
	}
	it.Seq, it.Start = seq, start
	if it.End.IsZero() {
		it.End = e.clk.Now()
	}
	return it, start
}

func (e *trEngine) setRun(run func(ctx context.Context, seq int64, progress func(domain.Iteration)) domain.Iteration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.run = run
}

func (e *trEngine) callsSnapshot() []trCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.calls)
}

// trEmptyRun completes iterations that transfer nothing.
func trEmptyRun(context.Context, int64, func(domain.Iteration)) domain.Iteration {
	return domain.Iteration{State: domain.IterationCompleted}
}

// trBlockRun blocks until the iteration is cancelled.
func trBlockRun(ctx context.Context, _ int64, _ func(domain.Iteration)) domain.Iteration {
	<-ctx.Done()
	return domain.Iteration{State: domain.IterationInterrupted}
}

type trFixture struct {
	svc     *domain.TransferService
	repo    *trRepo
	buckets *fakeBuckets
	clk     *clock.Manual
	eng     *trEngine
}

func newTrFixture(t *testing.T, maxParallel int) *trFixture {
	t.Helper()
	clk := clock.NewManual(t0)
	f := &trFixture{repo: newTrRepo(), buckets: &fakeBuckets{}, clk: clk, eng: &trEngine{clk: clk}}
	for _, b := range []struct {
		name, parent string
		state        storagepb.BucketStatus_State
	}{
		{trDstBucket, projectID, storagepb.BucketStatus_ACTIVE},
		{"src-bucket", projectID, storagepb.BucketStatus_ACTIVE},
		{"foreign-bucket", trOtherProj, storagepb.BucketStatus_ACTIVE},
		{"new-bucket", projectID, storagepb.BucketStatus_CREATING},
	} {
		f.buckets.items = append(f.buckets.items, &storagepb.Bucket{
			Metadata: &commonpb.ResourceMetadata{Id: "storagebucket-" + b.name, ParentId: b.parent, Name: b.name},
			Status:   &storagepb.BucketStatus{State: b.state},
		})
	}
	svc, err := domain.NewTransferService(domain.TransferDeps{
		Repo: f.repo, Buckets: f.buckets, Hierarchy: fakeHierarchy{}, Ops: inlineOps{}, IDs: &seqIDs{}, Clock: clk,
		Region: trRegion, Engine: f.eng, MaxParallel: maxParallel,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.svc = svc
	return f
}

func trAccessKey(id, secret string) *storagepb.TransferCredentialsAccessKey {
	return &storagepb.TransferCredentialsAccessKey{AccessKeyId: id, SecretAccessKey: secret}
}

// trV1Spec: an S3-compatible source into a Nebius bucket, one iteration.
func trV1Spec() *storagepb.TransferSpec {
	return &storagepb.TransferSpec{
		Source: &storagepb.TransferSource{Prefix: "in/", Provider: &storagepb.TransferSource_S3Compatible{
			S3Compatible: &storagepb.TransferSource_S3CompatibleProvider{
				Endpoint: trSrcEndpoint, Region: "us-east-1", BucketName: "src",
				Credentials: &storagepb.TransferSource_S3CompatibleProvider_AccessKey{AccessKey: trAccessKey("SRCKEY", "SRCSECRET")},
			},
		}},
		Destination: &storagepb.TransferDestination{Prefix: "out/", Provider: &storagepb.TransferDestination_Nebius{
			Nebius: &storagepb.TransferDestination_NebiusProvider{
				Region: trRegion, BucketName: trDstBucket,
				Credentials: &storagepb.TransferDestination_NebiusProvider_AccessKey{AccessKey: trAccessKey("DSTKEY", "DSTSECRET")},
			},
		}},
		StopCondition:     &storagepb.TransferSpec_AfterOneIteration{AfterOneIteration: &storagepb.TransferSpec_StopConditionAfterOneIteration{}},
		OverwriteStrategy: storagepb.TransferSpec_NEVER,
	}
}

func trAlphaCreds(id, secret string) *storagev1alpha1pb.TransferSpec_BucketCredentials {
	return &storagev1alpha1pb.TransferSpec_BucketCredentials{Credentials: &storagev1alpha1pb.TransferSpec_BucketCredentials_AccessKey{
		AccessKey: &storagev1alpha1pb.TransferSpec_BucketCredentials_CredentialsAccessKey{AccessKeyId: id, SecretAccessKey: secret},
	}}
}

// trAlphaSpec: a v1alpha1 transfer from an external S3 bucket into a Nebius bucket.
func trAlphaSpec() *storagev1alpha1pb.TransferSpec {
	return &storagev1alpha1pb.TransferSpec{
		Source: &storagev1alpha1pb.TransferSpec_SourceBucket{
			Endpoint: trSrcEndpoint, BucketName: "src", Region: "us-east-1", Prefix: "in/",
			Credentials: trAlphaCreds("SRCKEY", "SRCSECRET"),
		},
		Destination: &storagev1alpha1pb.TransferSpec_DestinationBucket{
			BucketName: trDstBucket, Credentials: trAlphaCreds("DSTKEY", "DSTSECRET"),
		},
		StopCondition:     &storagev1alpha1pb.TransferSpec_AfterOneIteration{AfterOneIteration: &storagev1alpha1pb.TransferSpec_StopConditionAfterOneIteration{}},
		OverwriteStrategy: storagev1alpha1pb.TransferSpec_ALWAYS,
	}
}

func trInfinite(spec *storagepb.TransferSpec, interval time.Duration) *storagepb.TransferSpec {
	spec.StopCondition = &storagepb.TransferSpec_Infinite{Infinite: &storagepb.TransferSpec_StopConditionInfinite{}}
	spec.InterIterationInterval = durationpb.New(interval)
	return spec
}

func (f *trFixture) createV1(t *testing.T, name string, spec *storagepb.TransferSpec) string {
	t.Helper()
	op, err := f.svc.CreateV1(t.Context(), domain.CreateTransferV1Cmd{
		Metadata: &commonpb.ResourceMetadata{ParentId: projectID, Name: name, Labels: map[string]string{"k": "v"}}, Spec: spec,
	})
	if err != nil {
		t.Fatalf("CreateV1(%s): %v", name, err)
	}
	if op.Err != nil {
		t.Fatalf("CreateV1(%s) operation: %v", name, op.Err)
	}
	return op.ResourceID
}

func (f *trFixture) createAlpha(t *testing.T, name string, spec *storagev1alpha1pb.TransferSpec) string {
	t.Helper()
	op, err := f.svc.CreateV1Alpha1(t.Context(), domain.CreateTransferV1Alpha1Cmd{
		Metadata: &commonpb.ResourceMetadata{ParentId: projectID, Name: name}, Spec: spec,
	})
	if err != nil {
		t.Fatalf("CreateV1Alpha1(%s): %v", name, err)
	}
	if op.Err != nil {
		t.Fatalf("CreateV1Alpha1(%s) operation: %v", name, op.Err)
	}
	return op.ResourceID
}

func (f *trFixture) get(t *testing.T, id string) *domain.TransferRecord {
	t.Helper()
	rec, err := f.svc.Get(t.Context(), id, domain.TransferAPIV1)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	return rec
}

// trWantViolation fails unless err is BadRequest with a violation on field.
func trWantViolation(t *testing.T, err error, field string) {
	t.Helper()
	e, ok := errors.AsType[*errs.Error](err)
	if !ok || e.Kind != errs.KindBadRequest {
		t.Fatalf("err = %v, want BadRequest on %s", err, field)
	}
	for _, v := range e.Violations {
		if v.Field == field {
			return
		}
	}
	t.Fatalf("violations %v do not name %s", e.Violations, field)
}
