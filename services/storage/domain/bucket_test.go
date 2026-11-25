package domain_test

import (
	"strconv"
	"testing"
	"time"

	"github.com/nebius/gosdk/proto/fieldmask/mask"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore"
	"github.com/kfirgollan/NebiusMocker/services/storage/domain"
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

const grace = 7 * 24 * time.Hour

type fixture struct {
	svc     *domain.BucketService
	buckets *fakeBuckets
	backend *fakeBackend
	content *contentFake
	clk     *clock.Manual
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{buckets: &fakeBuckets{}, backend: newFakeBackend(), content: newContentFake(false), clk: clock.NewManual(t0)}
	svc, err := domain.NewBucketService(domain.BucketDeps{
		Buckets: f.buckets, Backend: f.backend, Content: f.content, Hierarchy: fakeHierarchy{}, Ops: inlineOps{}, IDs: &seqIDs{}, Clock: f.clk,
		Settings: domain.Settings{Region: "eu-north1", Domain: "s3.test", PurgeGrace: grace},
	})
	if err != nil {
		t.Fatal(err)
	}
	f.svc = svc
	return f
}

func (f *fixture) create(t *testing.T, name string, spec *storagepb.BucketSpec) *storagepb.Bucket {
	t.Helper()
	op, err := f.svc.Create(t.Context(), domain.CreateBucketCmd{
		Metadata: &commonpb.ResourceMetadata{ParentId: projectID, Name: name, Labels: map[string]string{"a": "1"}}, Spec: spec,
	})
	if err != nil {
		t.Fatalf("Create(%s): %v", name, err)
	}
	if op.Err != nil {
		t.Fatalf("Create(%s) operation: %v", name, op.Err)
	}
	b, err := f.svc.Get(t.Context(), op.ResourceID)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestValidateBucketName(t *testing.T) {
	t.Parallel()
	for name, ok := range map[string]bool{
		"my-bucket": true, "a.b-c.1": true, "abc": true, "0a0": true,
		"ab": false, "Upper": false, "under_score": false, "-start": false, "end-": false, "a..b": false,
		"a.-b": false, "a-.b": false, "192.168.0.1": false, "xn--abc": false, "abc-s3alias": false,
		"nebius-mocker-images": false, string(make([]byte, 64)): false,
	} {
		err := domain.ValidateBucketName(name)
		if ok != (err == nil) {
			t.Errorf("ValidateBucketName(%q) = %v, want ok=%v", name, err, ok)
		}
		if err != nil && !errs.IsKind(err, errs.KindBadRequest) {
			t.Errorf("ValidateBucketName(%q) kind = %v, want BadRequest", name, errs.KindOf(err))
		}
	}
}

func TestCreateDefaultsAndProvisioning(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	b := f.create(t, "data", nil)
	st := b.GetStatus()
	if st.GetState() != storagepb.BucketStatus_ACTIVE || st.GetDomainName() != "data.s3.test" || st.GetRegion() != "eu-north1" ||
		st.GetBucketType() != storagepb.BucketStatus_REGULAR || st.GetSuspensionState() != storagepb.BucketStatus_NOT_SUSPENDED {
		t.Fatalf("status = %v", st)
	}
	if b.GetSpec().GetVersioningPolicy() != storagepb.VersioningPolicy_DISABLED {
		t.Errorf("versioning_policy = %v, want DISABLED (NON_EMPTY_DEFAULT)", b.GetSpec().GetVersioningPolicy())
	}
	if _, ok := f.backend.versioning("data"); !ok {
		t.Error("backing bucket not created")
	}
	v := f.create(t, "versioned", &storagepb.BucketSpec{VersioningPolicy: storagepb.VersioningPolicy_ENABLED})
	if got, _ := f.backend.versioning("versioned"); got != objstore.VersioningEnabled || v.GetSpec().GetVersioningPolicy() != storagepb.VersioningPolicy_ENABLED {
		t.Errorf("backend versioning = %v, want enabled", got)
	}
}

func TestCreateValidation(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.create(t, "taken", nil)
	tests := []struct {
		name string
		md   *commonpb.ResourceMetadata
		spec *storagepb.BucketSpec
		want errs.Kind
	}{
		{"client id", &commonpb.ResourceMetadata{Id: "storagebucket-x", ParentId: projectID, Name: "aaa"}, nil, errs.KindBadRequest},
		{"no parent", &commonpb.ResourceMetadata{Name: "aaa"}, nil, errs.KindBadRequest},
		{"tenant parent", &commonpb.ResourceMetadata{ParentId: "tenant-e00x", Name: "aaa"}, nil, errs.KindBadRequest},
		{"unknown project", &commonpb.ResourceMetadata{ParentId: "project-e00other", Name: "aaa"}, nil, errs.KindNotFound},
		{"bad name", &commonpb.ResourceMetadata{ParentId: projectID, Name: "A"}, nil, errs.KindBadRequest},
		{"name taken", &commonpb.ResourceMetadata{ParentId: projectID, Name: "taken"}, nil, errs.KindAlreadyExists},
		{"filesystem", &commonpb.ResourceMetadata{ParentId: projectID, Name: "fsb"}, &storagepb.BucketSpec{
			BucketType: &storagepb.BucketSpec_FilesystemBucket{FilesystemBucket: &storagepb.BucketSpec_FilesystemBucketType{FilesystemId: "computefilesystem-1"}},
		}, errs.KindBadRequest},
		{"negative size", &commonpb.ResourceMetadata{ParentId: projectID, Name: "neg"}, &storagepb.BucketSpec{MaxSizeBytes: -1}, errs.KindBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := f.svc.Create(t.Context(), domain.CreateBucketCmd{Metadata: tt.md, Spec: tt.spec})
			if got := errs.KindOf(err); got != tt.want {
				t.Fatalf("Create kind = %v, want %v (err: %v)", got, tt.want, err)
			}
		})
	}
}

func TestCreateRollsBackOnBackendFailure(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.backend.fail = errBackendDown
	op, err := f.svc.Create(t.Context(), domain.CreateBucketCmd{Metadata: &commonpb.ResourceMetadata{ParentId: projectID, Name: "doomed"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if op.Err == nil {
		t.Fatal("operation succeeded, want backend failure")
	}
	if _, err := f.buckets.GetByBucketName(t.Context(), "doomed"); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("record after failed create: %v, want removed (name released)", err)
	}
}

func TestVersioningTransitions(t *testing.T) {
	t.Parallel()
	v := func(p storagepb.VersioningPolicy) *storagepb.BucketSpec {
		return &storagepb.BucketSpec{VersioningPolicy: p}
	}
	const (
		dis = storagepb.VersioningPolicy_DISABLED
		ena = storagepb.VersioningPolicy_ENABLED
		sus = storagepb.VersioningPolicy_SUSPENDED
	)
	tests := []struct {
		from, to storagepb.VersioningPolicy
		ok       bool
	}{
		{dis, ena, true},
		{dis, sus, true},
		{ena, sus, true},
		{sus, ena, true},
		{ena, ena, true},
		{ena, dis, false},
		{sus, dis, false},
	}
	for _, tt := range tests {
		f := newFixture(t)
		b := f.create(t, "vbucket", v(tt.from))
		_, err := f.svc.Update(t.Context(), domain.UpdateBucketCmd{
			Metadata: &commonpb.ResourceMetadata{Id: b.GetMetadata().GetId()}, Spec: v(tt.to),
		})
		if tt.ok != (err == nil) {
			t.Errorf("%v → %v: err = %v, want ok=%v", tt.from, tt.to, err, tt.ok)
			continue
		}
		if tt.ok {
			got, _ := f.svc.Get(t.Context(), b.GetMetadata().GetId())
			if got.GetSpec().GetVersioningPolicy() != tt.to || got.GetStatus().GetState() != storagepb.BucketStatus_ACTIVE {
				t.Errorf("%v → %v: bucket = %v", tt.from, tt.to, got)
			}
		}
	}
}

func TestUpdateRules(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	b := f.create(t, "rules", &storagepb.BucketSpec{MaxSizeBytes: 100})
	id := b.GetMetadata().GetId()
	for name, cmd := range map[string]domain.UpdateBucketCmd{
		"rename":       {Metadata: &commonpb.ResourceMetadata{Id: id, Name: "other"}},
		"move":         {Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: "project-e00other"}},
		"filesystem":   {Metadata: &commonpb.ResourceMetadata{Id: id}, Spec: &storagepb.BucketSpec{BucketType: &storagepb.BucketSpec_FilesystemBucket{}}},
		"no id":        {Metadata: &commonpb.ResourceMetadata{}},
		"bad versions": {Metadata: &commonpb.ResourceMetadata{Id: id, ResourceVersion: 7}},
	} {
		if _, err := f.svc.Update(t.Context(), cmd); err == nil {
			t.Errorf("%s: Update succeeded, want error", name)
		}
	}
	// Labels are replaced, max size kept when absent (no reset mask) and cleared when masked.
	if _, err := f.svc.Update(t.Context(), domain.UpdateBucketCmd{
		Metadata: &commonpb.ResourceMetadata{Id: id, Labels: map[string]string{"b": "2"}},
	}); err != nil {
		t.Fatal(err)
	}
	got, _ := f.svc.Get(t.Context(), id)
	if got.GetSpec().GetMaxSizeBytes() != 100 || got.GetMetadata().GetLabels()["b"] != "2" || got.GetMetadata().GetResourceVersion() != 2 {
		t.Fatalf("after labels update = %v", got)
	}
	if _, err := f.svc.Update(t.Context(), domain.UpdateBucketCmd{
		Metadata: &commonpb.ResourceMetadata{Id: id, Labels: map[string]string{"b": "2"}}, ResetMask: mask.ParseMust("spec.max_size_bytes"),
	}); err != nil {
		t.Fatal(err)
	}
	got, _ = f.svc.Get(t.Context(), id)
	if got.GetSpec().GetMaxSizeBytes() != 0 || got.GetMetadata().GetResourceVersion() != 3 {
		t.Fatalf("after masked update = %v", got)
	}
	// A no-op update keeps the version.
	if _, err := f.svc.Update(t.Context(), domain.UpdateBucketCmd{Metadata: &commonpb.ResourceMetadata{Id: id, Labels: map[string]string{"b": "2"}}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.svc.Get(t.Context(), id); got.GetMetadata().GetResourceVersion() != 3 {
		t.Fatalf("no-op update bumped resource_version to %d", got.GetMetadata().GetResourceVersion())
	}
}

func TestSoftDeleteUndeleteAndSweep(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	b := f.create(t, "soft", nil)
	id := b.GetMetadata().GetId()
	if _, err := f.svc.Delete(t.Context(), domain.DeleteBucketCmd{ID: id}); err != nil {
		t.Fatal(err)
	}
	got, _ := f.svc.Get(t.Context(), id)
	if got.GetStatus().GetState() != storagepb.BucketStatus_SCHEDULED_FOR_DELETION ||
		!got.GetStatus().GetDeletedAt().AsTime().Equal(t0) || !got.GetStatus().GetPurgeAt().AsTime().Equal(t0.Add(grace)) {
		t.Fatalf("after delete = %v", got.GetStatus())
	}
	for name, err := range map[string]error{
		"update scheduled": errOf(f.svc.Update(t.Context(), domain.UpdateBucketCmd{Metadata: &commonpb.ResourceMetadata{Id: id}})),
		"delete twice":     errOf(f.svc.Delete(t.Context(), domain.DeleteBucketCmd{ID: id})),
	} {
		if !errs.IsKind(err, errs.KindBadState) {
			t.Errorf("%s: err %v, want BadState", name, err)
		}
	}
	if _, err := f.svc.Undelete(t.Context(), id, nil); err != nil {
		t.Fatal(err)
	}
	got, _ = f.svc.Get(t.Context(), id)
	if got.GetStatus().GetState() != storagepb.BucketStatus_ACTIVE || got.GetStatus().GetPurgeAt() != nil {
		t.Fatalf("after undelete = %v", got.GetStatus())
	}
	if _, err := f.svc.Undelete(t.Context(), id, nil); !errs.IsKind(err, errs.KindBadState) {
		t.Errorf("undelete active: %v, want BadState", err)
	}
	if _, err := f.svc.Purge(t.Context(), id, nil); !errs.IsKind(err, errs.KindBadState) {
		t.Errorf("purge active: %v, want BadState", err)
	}

	// Delete again; the sweeper purges only after purge_at.
	if _, err := f.svc.Delete(t.Context(), domain.DeleteBucketCmd{ID: id}); err != nil {
		t.Fatal(err)
	}
	f.clk.Advance(grace - time.Second)
	if n, err := f.svc.SweepExpired(t.Context()); err != nil || n != 0 {
		t.Fatalf("sweep before purge_at = %d, %v; want 0", n, err)
	}
	f.clk.Advance(time.Second)
	if n, err := f.svc.SweepExpired(t.Context()); err != nil || n != 1 {
		t.Fatalf("sweep at purge_at = %d, %v; want 1", n, err)
	}
	if _, err := f.svc.Get(t.Context(), id); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("Get after sweep: %v, want NotFound", err)
	}
	if _, ok := f.backend.versioning("soft"); ok {
		t.Error("backing bucket survived the purge")
	}
}

func TestDeleteVariants(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	zero := time.Duration(0)
	hour := time.Hour
	past := t0.Add(-time.Minute)
	n := 0
	for name, tt := range map[string]struct {
		cmd       func(id string) domain.DeleteBucketCmd
		wantGone  bool
		wantPurge time.Time
	}{
		"zero ttl purges":   {cmd: func(id string) domain.DeleteBucketCmd { return domain.DeleteBucketCmd{ID: id, TTL: &zero} }, wantGone: true},
		"past purge_at":     {cmd: func(id string) domain.DeleteBucketCmd { return domain.DeleteBucketCmd{ID: id, PurgeAt: &past} }, wantGone: true},
		"ttl schedules":     {cmd: func(id string) domain.DeleteBucketCmd { return domain.DeleteBucketCmd{ID: id, TTL: &hour} }, wantPurge: t0.Add(time.Hour)},
		"default schedules": {cmd: func(id string) domain.DeleteBucketCmd { return domain.DeleteBucketCmd{ID: id} }, wantPurge: t0.Add(grace)},
	} {
		n++
		b := f.create(t, "del-"+strconv.Itoa(n), nil)
		id := b.GetMetadata().GetId()
		if _, err := f.svc.Delete(t.Context(), tt.cmd(id)); err != nil {
			t.Fatalf("%s: Delete: %v", name, err)
		}
		got, err := f.svc.Get(t.Context(), id)
		if tt.wantGone {
			if !errs.IsKind(err, errs.KindNotFound) {
				t.Errorf("%s: Get = %v, want NotFound", name, err)
			}
			continue
		}
		if err != nil || !got.GetStatus().GetPurgeAt().AsTime().Equal(tt.wantPurge) {
			t.Errorf("%s: purge_at = %v (%v), want %v", name, got.GetStatus().GetPurgeAt().AsTime(), err, tt.wantPurge)
		}
	}
	// Non-empty buckets cannot be deleted; negative TTLs are rejected.
	b := f.create(t, "full", nil)
	f.backend.objects["full"] = 1
	if _, err := f.svc.Delete(t.Context(), domain.DeleteBucketCmd{ID: b.GetMetadata().GetId()}); !errs.IsKind(err, errs.KindBadState) {
		t.Errorf("delete non-empty: %v, want BadState", err)
	}
	f.backend.objects["full"] = 0
	neg := -time.Second
	if _, err := f.svc.Delete(t.Context(), domain.DeleteBucketCmd{ID: b.GetMetadata().GetId(), TTL: &neg}); !errs.IsKind(err, errs.KindBadRequest) {
		t.Errorf("negative ttl: %v, want BadRequest", err)
	}
}

func TestListWithFilter(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.create(t, "regular", nil)
	page, err := f.svc.ListWithFilter(t.Context(), projectID, 0, "", &storagepb.ListBucketsWithFilterRequest_Filters{BucketType: storagepb.BucketStatus_REGULAR})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("REGULAR filter = %v, %v; want 1", page, err)
	}
	page, err = f.svc.ListWithFilter(t.Context(), projectID, 0, "", &storagepb.ListBucketsWithFilterRequest_Filters{BucketType: storagepb.BucketStatus_FILESYSTEM})
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("FILESYSTEM filter = %v, %v; want none", page, err)
	}
	if _, err := f.svc.List(t.Context(), projectID, 5000, ""); !errs.IsKind(err, errs.KindBadRequest) {
		t.Errorf("page_size 5000: %v, want BadRequest", err)
	}
}

func TestBackendBucket(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	b := f.create(t, "resolved-bucket", nil)
	if got, err := f.svc.BackendBucket(t.Context(), b.GetMetadata().GetId()); err != nil || got != "resolved-bucket" {
		t.Fatalf("BackendBucket = %q, %v", got, err)
	}
	if _, err := f.svc.BackendBucket(t.Context(), "storagebucket-e00missing"); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("BackendBucket(missing) = %v, want NotFound", err)
	}
}

func errOf[T any](_ T, err error) error { return err }
