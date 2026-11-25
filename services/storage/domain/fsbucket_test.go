package domain_test

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/services/storage/domain"
)

const (
	fsID1        = "computefilesystem-e00test1"
	fsStore1     = "fs-store-1"
	fsCapacity   = int64(100)
	otherProject = "project-e00test0000000000002"
)

// fakeFilesystems is a map-backed domain.Filesystems.
type fakeFilesystems struct {
	mu      sync.Mutex
	fss     map[string]peer.Filesystem
	exposed map[string][]string
}

func (f *fakeFilesystems) Filesystem(_ context.Context, id string) (peer.Filesystem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fs, ok := f.fss[id]
	if !ok {
		return peer.Filesystem{}, errs.NotFound(id)
	}
	return fs, nil
}

// Expose records bucketID like compute does: the filesystem must exist, be
// in projectID and be ready.
func (f *fakeFilesystems) Expose(_ context.Context, fsID, bucketID, projectID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	fs, ok := f.fss[fsID]
	switch {
	case !ok:
		return errs.NotFound(fsID)
	case fs.ParentID != projectID:
		return errs.BadRequest(errs.Violation{Field: "spec.filesystem_bucket.filesystem_id", Message: "other project"})
	case !fs.Ready:
		return errs.BadState(fsID, "not ready")
	}
	if f.exposed == nil {
		f.exposed = map[string][]string{}
	}
	f.exposed[fsID] = append(f.exposed[fsID], bucketID)
	return nil
}

func (f *fakeFilesystems) Unexpose(_ context.Context, fsID, bucketID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.exposed[fsID] = slices.DeleteFunc(f.exposed[fsID], func(b string) bool { return b == bucketID })
	return nil
}

// exposedBy returns the buckets recorded on fsID.
func (f *fakeFilesystems) exposedBy(fsID string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.exposed[fsID])
}

func (f *fakeFilesystems) set(fs peer.Filesystem) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fss[fs.ID] = fs
}

func (f *fakeFilesystems) remove(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.fss, id)
}

// pagingBuckets honours the page size and token of List (the token is the
// index of the next item), unlike fakeBuckets.
type pagingBuckets struct{ *fakeBuckets }

func (p pagingBuckets) List(ctx context.Context, parentID string, size int, token string) (domain.BucketPage, error) {
	all, _ := p.fakeBuckets.List(ctx, parentID, 0, "")
	start := 0
	if token != "" {
		start, _ = strconv.Atoi(token)
	}
	end := min(start+size, len(all.Items))
	page := domain.BucketPage{Items: all.Items[start:end]}
	if end < len(all.Items) {
		page.NextPageToken = strconv.Itoa(end)
	}
	return page, nil
}

type fsOptions struct {
	mode    authz.Mode
	checker domain.AccessChecker
	paging  bool
}

type fsFixture struct {
	*fixture
	fss *fakeFilesystems
}

func newFSFixture(t *testing.T, o fsOptions) *fsFixture {
	t.Helper()
	f := &fixture{buckets: &fakeBuckets{}, backend: newFakeBackend(), content: newContentFake(false), clk: clock.NewManual(t0)}
	var repo domain.BucketRepository = f.buckets
	if o.paging {
		repo = pagingBuckets{f.buckets}
	}
	fss := &fakeFilesystems{fss: map[string]peer.Filesystem{
		fsID1: {ID: fsID1, ParentID: projectID, Ready: true, CapacityBytes: fsCapacity, BackendBucket: fsStore1},
	}}
	svc, err := domain.NewBucketService(domain.BucketDeps{
		Buckets: repo, Backend: f.backend, Content: f.content, Hierarchy: fakeHierarchy{}, Ops: inlineOps{}, IDs: &seqIDs{}, Clock: f.clk,
		Settings:    domain.Settings{Region: "eu-north1", Domain: "s3.test", PurgeGrace: grace},
		Filesystems: fss, Access: o.checker, Mode: o.mode,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.svc = svc
	return &fsFixture{fixture: f, fss: fss}
}

func fsSpec(mutate ...func(*storagepb.BucketSpec)) *storagepb.BucketSpec {
	spec := &storagepb.BucketSpec{BucketType: &storagepb.BucketSpec_FilesystemBucket{
		FilesystemBucket: &storagepb.BucketSpec_FilesystemBucketType{FilesystemId: fsID1},
	}}
	for _, m := range mutate {
		m(spec)
	}
	return spec
}

func fsOf(spec *storagepb.BucketSpec) *storagepb.BucketSpec_FilesystemBucketType {
	return spec.GetFilesystemBucket()
}

// createErr creates bucket "fsbucket" and and returns its error (nil when the bucket is made).
func (f *fsFixture) createErr(t *testing.T, spec *storagepb.BucketSpec) error {
	t.Helper()
	_, err := f.svc.Create(t.Context(), domain.CreateBucketCmd{
		Metadata: &commonpb.ResourceMetadata{ParentId: projectID, Name: "fsbucket"}, Spec: spec,
	})
	return err
}

func TestCreateFilesystemBucket(t *testing.T) {
	t.Parallel()
	f := newFSFixture(t, fsOptions{})
	spec := fsSpec()
	b := f.create(t, "fsbucket", spec)
	if got := b.GetStatus().GetBucketType(); got != storagepb.BucketStatus_FILESYSTEM {
		t.Errorf("status.bucket_type = %v, want FILESYSTEM", got)
	}
	if got := b.GetStatus().GetState(); got != storagepb.BucketStatus_ACTIVE {
		t.Errorf("status.state = %v, want ACTIVE", got)
	}
	if got := b.GetSpec().GetFilesystemBucket().GetFilesystemId(); got != fsID1 {
		t.Errorf("spec.filesystem_bucket.filesystem_id = %q, want %q", got, fsID1)
	}
	if _, ok := f.backend.versioning("fsbucket"); ok {
		t.Error("EnsureBucket was called for a filesystem bucket")
	}
	if len(f.backend.buckets) != 0 {
		t.Errorf("backend buckets = %v, want none", f.backend.buckets)
	}
	if got := f.fss.exposedBy(fsID1); !slices.Equal(got, []string{b.GetMetadata().GetId()}) {
		t.Errorf("filesystem exposed by %v, want the new bucket", got)
	}
}

// TestCreateFilesystemBucketNameTakenUnexposes checks that a bucket whose
// record cannot be stored leaves no reference on the filesystem.
func TestCreateFilesystemBucketNameTakenUnexposes(t *testing.T) {
	t.Parallel()
	f := newFSFixture(t, fsOptions{})
	first := f.create(t, "fsbucket", fsSpec())
	if err := f.createErr(t, fsSpec()); !errs.IsKind(err, errs.KindAlreadyExists) {
		t.Fatalf("second fsbucket = %v, want AlreadyExists", err)
	}
	if got := f.fss.exposedBy(fsID1); !slices.Equal(got, []string{first.GetMetadata().GetId()}) {
		t.Fatalf("filesystem exposed by %v, want only the first bucket", got)
	}
}

func TestCreateFilesystemBucketFilesystemChecks(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		setup func(*fakeFilesystems)
		spec  *storagepb.BucketSpec
		want  errs.Kind
	}{
		{"missing filesystem", func(f *fakeFilesystems) { f.remove(fsID1) }, fsSpec(), errs.KindNotFound},
		{"other project", func(f *fakeFilesystems) {
			f.set(peer.Filesystem{ID: fsID1, ParentID: otherProject, Ready: true, CapacityBytes: fsCapacity, BackendBucket: fsStore1})
		}, fsSpec(), errs.KindBadRequest},
		{"not ready", func(f *fakeFilesystems) {
			f.set(peer.Filesystem{ID: fsID1, ParentID: projectID, Ready: false, CapacityBytes: fsCapacity, BackendBucket: fsStore1})
		}, fsSpec(), errs.KindBadState},
		{"empty filesystem_id", func(*fakeFilesystems) {}, fsSpec(func(s *storagepb.BucketSpec) { fsOf(s).FilesystemId = "" }), errs.KindBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFSFixture(t, fsOptions{})
			tt.setup(f.fss)
			if err := f.createErr(t, tt.spec); errs.KindOf(err) != tt.want {
				t.Fatalf("Create = %v, want kind %v", err, tt.want)
			}
			if got, _ := f.buckets.ListAll(t.Context()); len(got) != 0 {
				t.Errorf("a failed Create left %d bucket records", len(got))
			}
		})
	}
}

func TestCreateFilesystemBucketWithoutComputeModule(t *testing.T) {
	t.Parallel()
	f := newFixture(t) // no Filesystems wired
	_, err := f.svc.Create(t.Context(), domain.CreateBucketCmd{
		Metadata: &commonpb.ResourceMetadata{ParentId: projectID, Name: "fsbucket"}, Spec: fsSpec(),
	})
	if !errs.IsKind(err, errs.KindBadRequest) {
		t.Fatalf("Create without Filesystems = %v, want BadRequest", err)
	}
}

func TestCreateFilesystemBucketRejectedFields(t *testing.T) {
	t.Parallel()
	tests := []struct {
		field  string
		mutate func(*storagepb.BucketSpec)
	}{
		{"directory", func(s *storagepb.BucketSpec) { fsOf(s).Directory = "/data" }},
		{"uid", func(s *storagepb.BucketSpec) { fsOf(s).Uid = 1 }},
		{"gid", func(s *storagepb.BucketSpec) { fsOf(s).Gid = 1 }},
		{"file_mode", func(s *storagepb.BucketSpec) { fsOf(s).FileMode = "600" }},
		{"directory_mode", func(s *storagepb.BucketSpec) { fsOf(s).DirectoryMode = "700" }},
		{"versioning_policy", func(s *storagepb.BucketSpec) { s.VersioningPolicy = storagepb.VersioningPolicy_ENABLED }},
		{"lifecycle_configuration", func(s *storagepb.BucketSpec) {
			s.LifecycleConfiguration = &storagepb.LifecycleConfiguration{Rules: []*storagepb.LifecycleRule{{Id: "r", Status: storagepb.LifecycleRule_ENABLED}}}
		}},
		{"bucket_policy", func(s *storagepb.BucketSpec) {
			s.BucketPolicy = &storagepb.BucketPolicy{Rules: []*storagepb.BucketPolicy_Rule{{
				Paths: []string{"*"}, Roles: []string{"storage.object-viewer"},
				Subject: &storagepb.BucketPolicy_Rule_Anonymous{Anonymous: &storagepb.BucketPolicy_Rule_AnonymousAccess{}},
			}}}
		}},
		{"default_storage_class", func(s *storagepb.BucketSpec) { s.DefaultStorageClass = storagepb.StorageClass_STANDARD }},
	}
	for _, tt := range tests {
		t.Run(tt.field, func(t *testing.T) {
			t.Parallel()
			f := newFSFixture(t, fsOptions{})
			err := f.createErr(t, fsSpec(tt.mutate))
			if !errs.IsKind(err, errs.KindBadRequest) {
				t.Fatalf("Create with %s = %v, want BadRequest", tt.field, err)
			}
			if !strings.Contains(err.Error(), tt.field) {
				t.Errorf("error %q does not name %s", err, tt.field)
			}
		})
	}
}

func TestCreateFilesystemBucketAcceptedDefaults(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*storagepb.BucketSpec)
	}{
		{"directory empty", func(s *storagepb.BucketSpec) { fsOf(s).Directory = "" }},
		{"directory root", func(s *storagepb.BucketSpec) { fsOf(s).Directory = "/" }},
		{"file_mode 644", func(s *storagepb.BucketSpec) { fsOf(s).FileMode = "644" }},
		{"file_mode 0644", func(s *storagepb.BucketSpec) { fsOf(s).FileMode = "0644" }},
		{"file_mode empty", func(s *storagepb.BucketSpec) { fsOf(s).FileMode = "" }},
		{"directory_mode 755", func(s *storagepb.BucketSpec) { fsOf(s).DirectoryMode = "755" }},
		{"directory_mode 0755", func(s *storagepb.BucketSpec) { fsOf(s).DirectoryMode = "0755" }},
		{"directory_mode empty", func(s *storagepb.BucketSpec) { fsOf(s).DirectoryMode = "" }},
		{"storage class FILESYSTEM", func(s *storagepb.BucketSpec) { s.DefaultStorageClass = storagepb.StorageClass_FILESYSTEM }},
		{"versioning DISABLED", func(s *storagepb.BucketSpec) { s.VersioningPolicy = storagepb.VersioningPolicy_DISABLED }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFSFixture(t, fsOptions{})
			if err := f.createErr(t, fsSpec(tt.mutate)); err != nil {
				t.Fatalf("Create = %v, want success", err)
			}
		})
	}
}

func TestCreateFilesystemBucketAuthz(t *testing.T) {
	t.Parallel()
	perm := domain.PermFilesystemViaObjectStorage
	deny := fakeChecker{allowed: map[authz.Permission]bool{}}
	allow := fakeChecker{allowed: map[authz.Permission]bool{perm: true}}
	tests := []struct {
		name    string
		mode    authz.Mode
		checker domain.AccessChecker
		want    errs.Kind
	}{
		{"enforce denied", authz.ModeEnforce, deny, errs.KindPermissionDenied},
		{"enforce allowed", authz.ModeEnforce, allow, 0},
		{"audit denied", authz.ModeAudit, deny, 0},
		{"off denied", authz.ModeOff, deny, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFSFixture(t, fsOptions{mode: tt.mode, checker: tt.checker})
			if err := f.createErr(t, fsSpec()); errs.KindOf(err) != tt.want {
				t.Fatalf("Create = %v, want kind %v", err, tt.want)
			}
		})
	}
}

func TestUpdateFilesystemBucket(t *testing.T) {
	t.Parallel()
	f := newFSFixture(t, fsOptions{})
	b := f.create(t, "fsbucket", fsSpec())
	id := b.GetMetadata().GetId()

	// Terraform repeats the whole spec on update.
	if _, err := f.svc.Update(t.Context(), domain.UpdateBucketCmd{
		Metadata: &commonpb.ResourceMetadata{Id: id, Labels: map[string]string{"b": "2"}}, Spec: fsSpec(),
	}); err != nil {
		t.Fatalf("Update repeating filesystem_bucket = %v, want success", err)
	}
	got, err := f.svc.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.GetMetadata().GetLabels()["b"] != "2" || got.GetSpec().GetFilesystemBucket().GetFilesystemId() != fsID1 {
		t.Fatalf("after update = %v", got)
	}
	if got.GetStatus().GetBucketType() != storagepb.BucketStatus_FILESYSTEM {
		t.Errorf("bucket_type after update = %v, want FILESYSTEM", got.GetStatus().GetBucketType())
	}

	other := fsSpec(func(s *storagepb.BucketSpec) { fsOf(s).FilesystemId = "computefilesystem-e00other" })
	if _, err := f.svc.Update(t.Context(), domain.UpdateBucketCmd{Metadata: &commonpb.ResourceMetadata{Id: id}, Spec: other}); !errs.IsKind(err, errs.KindBadRequest) {
		t.Errorf("Update changing filesystem_id = %v, want BadRequest", err)
	}
}

func TestDeleteFilesystemBucket(t *testing.T) {
	t.Parallel()
	zero, hour := time.Duration(0), time.Hour
	future := t0.Add(time.Hour)
	tests := []struct {
		name     string
		cmd      domain.DeleteBucketCmd
		wantGone bool
	}{
		{"default is immediate", domain.DeleteBucketCmd{}, true},
		{"ttl 0 is immediate", domain.DeleteBucketCmd{TTL: &zero}, true},
		{"ttl 1h is refused", domain.DeleteBucketCmd{TTL: &hour}, false},
		{"future purge_at is refused", domain.DeleteBucketCmd{PurgeAt: &future}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFSFixture(t, fsOptions{})
			b := f.create(t, "fsbucket", fsSpec())
			id := b.GetMetadata().GetId()
			f.content.put("file.txt", "v1", t0, 10, "etag", "STANDARD") // the filesystem holds files
			f.backend.objects["fsbucket"] = 1                           // and the bucket is "non-empty"
			f.backend.objects[fsStore1] = 1
			cmd := tt.cmd
			cmd.ID = id
			_, err := f.svc.Delete(t.Context(), cmd)
			got, getErr := f.svc.Get(t.Context(), id)
			if !tt.wantGone {
				if !errs.IsKind(err, errs.KindBadRequest) {
					t.Fatalf("Delete = %v, want BadRequest", err)
				}
				if getErr != nil || got.GetStatus().GetState() != storagepb.BucketStatus_ACTIVE {
					t.Fatalf("bucket after refused delete = %v, %v; want ACTIVE", got, getErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Delete = %v", err)
			}
			if !errs.IsKind(getErr, errs.KindNotFound) {
				t.Fatalf("Get after delete = %v, want NotFound (immediate deletion)", getErr)
			}
			if len(f.backend.deleteHits) != 0 {
				t.Errorf("backend DeleteBucket called for %v; the files belong to the filesystem", f.backend.deleteHits)
			}
			if got := f.fss.exposedBy(fsID1); len(got) != 0 {
				t.Errorf("filesystem still exposed by %v after the bucket was deleted", got)
			}
			if len(f.content.calls) != 0 {
				t.Errorf("content calls = %v, want none", f.content.calls)
			}
		})
	}
}

func TestCheckCapacityFilesystemBucket(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		maxSize  int64
		stored   int64
		incoming int64
		want     errs.Kind
	}{
		{"under capacity", 0, 60, 39, 0},
		{"exactly at capacity", 0, 60, 40, 0},
		{"over capacity", 0, 60, 41, errs.KindQuotaExceeded},
		{"empty filesystem over capacity", 0, 0, 101, errs.KindQuotaExceeded},
		{"max_size_bytes below capacity still applies", 50, 40, 11, errs.KindQuotaExceeded},
		{"max_size_bytes honoured at the limit", 50, 40, 10, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFSFixture(t, fsOptions{})
			b := f.create(t, "fsbucket", fsSpec(func(s *storagepb.BucketSpec) { s.MaxSizeBytes = tt.maxSize }))
			if tt.stored > 0 {
				f.content.put("file.txt", "v1", t0, tt.stored, "etag", "STANDARD")
			}
			if err := f.svc.CheckCapacity(t.Context(), b, tt.incoming); errs.KindOf(err) != tt.want {
				t.Fatalf("CheckCapacity(%d) = %v, want kind %v", tt.incoming, err, tt.want)
			}
		})
	}
}

func TestFilesystemBucketBackend(t *testing.T) {
	t.Parallel()
	f := newFSFixture(t, fsOptions{})
	b := f.create(t, "fsbucket", fsSpec())
	id := b.GetMetadata().GetId()
	if got, err := f.svc.Backend(t.Context(), b); err != nil || got != fsStore1 {
		t.Fatalf("Backend = %q, %v; want %q", got, err, fsStore1)
	}
	if got, err := f.svc.BackendBucket(t.Context(), id); err != nil || got != fsStore1 {
		t.Fatalf("BackendBucket = %q, %v; want %q", got, err, fsStore1)
	}
	regular := f.create(t, "regular", nil)
	if got, err := f.svc.Backend(t.Context(), regular); err != nil || got != "regular" {
		t.Fatalf("Backend(regular) = %q, %v; want the bucket name", got, err)
	}

	// Counters come from the filesystem's files.
	f.content.put("file.txt", "v1", t0, 7, "etag", "STANDARD")
	got, err := f.svc.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	var size int64
	for _, c := range got.GetStatus().GetCounters() {
		size += c.GetCounters().GetSimpleObjectsSize()
	}
	if size != 7 {
		t.Errorf("filesystem bucket counters = %d bytes, want 7 (the filesystem's files)", size)
	}

	// A gone or not READY filesystem makes the bucket unusable, not unreadable.
	f.fss.remove(fsID1)
	if _, err := f.svc.Backend(t.Context(), b); !errs.IsKind(err, errs.KindBadState) {
		t.Errorf("Backend with the filesystem gone = %v, want BadState", err)
	}
	if _, err := f.svc.BackendBucket(t.Context(), id); !errs.IsKind(err, errs.KindBadState) {
		t.Errorf("BackendBucket with the filesystem gone = %v, want BadState", err)
	}
	if err := f.svc.CheckCapacity(t.Context(), b, 1); !errs.IsKind(err, errs.KindBadState) {
		t.Errorf("CheckCapacity with the filesystem gone = %v, want BadState", err)
	}
	if _, err := f.svc.Get(t.Context(), id); err != nil {
		t.Errorf("Get with the filesystem gone = %v, want the bucket", err)
	}
	f.fss.set(peer.Filesystem{ID: fsID1, ParentID: projectID, Ready: false, CapacityBytes: fsCapacity, BackendBucket: fsStore1})
	if _, err := f.svc.Backend(t.Context(), b); !errs.IsKind(err, errs.KindBadState) {
		t.Errorf("Backend with the filesystem not ready = %v, want BadState", err)
	}
}

func TestListWithFilterFilesystemBuckets(t *testing.T) {
	t.Parallel()
	f := newFSFixture(t, fsOptions{paging: true})
	const fsID2 = "computefilesystem-e00test2"
	f.fss.set(peer.Filesystem{ID: fsID2, ParentID: projectID, Ready: true, CapacityBytes: fsCapacity, BackendBucket: "fs-store-2"})
	f.create(t, "reg-1", nil)
	f.create(t, "fs-a", fsSpec())
	f.create(t, "reg-2", nil)
	f.create(t, "fs-b", fsSpec(func(s *storagepb.BucketSpec) { fsOf(s).FilesystemId = fsID2 }))
	f.create(t, "fs-c", fsSpec())

	names := func(p domain.BucketPage) []string {
		var out []string
		for _, b := range p.Items {
			out = append(out, b.GetMetadata().GetName())
		}
		return out
	}
	tests := []struct {
		name   string
		filter *storagepb.ListBucketsWithFilterRequest_Filters
		want   []string
	}{
		{"FILESYSTEM", &storagepb.ListBucketsWithFilterRequest_Filters{BucketType: storagepb.BucketStatus_FILESYSTEM}, []string{"fs-a", "fs-b", "fs-c"}},
		{"REGULAR", &storagepb.ListBucketsWithFilterRequest_Filters{BucketType: storagepb.BucketStatus_REGULAR}, []string{"reg-1", "reg-2"}},
		{"filesystem_id", &storagepb.ListBucketsWithFilterRequest_Filters{FilesystemId: fsID1}, []string{"fs-a", "fs-c"}},
		{"filesystem_id other", &storagepb.ListBucketsWithFilterRequest_Filters{FilesystemId: fsID2}, []string{"fs-b"}},
		{"filesystem_id unknown", &storagepb.ListBucketsWithFilterRequest_Filters{FilesystemId: "computefilesystem-none"}, nil},
		{"no filter", &storagepb.ListBucketsWithFilterRequest_Filters{}, []string{"reg-1", "fs-a", "reg-2", "fs-b", "fs-c"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			page, err := f.svc.ListWithFilter(t.Context(), projectID, 0, "", tt.filter)
			if err != nil || !slices.Equal(names(page), tt.want) || page.NextPageToken != "" {
				t.Fatalf("ListWithFilter = %v (next %q), %v; want %v", names(page), page.NextPageToken, err, tt.want)
			}
		})
	}

	t.Run("paging", func(t *testing.T) {
		t.Parallel()
		filter := &storagepb.ListBucketsWithFilterRequest_Filters{BucketType: storagepb.BucketStatus_FILESYSTEM}
		page, err := f.svc.ListWithFilter(t.Context(), projectID, 1, "", filter)
		if err != nil || !slices.Equal(names(page), []string{"fs-a"}) || page.NextPageToken == "" {
			t.Fatalf("page 1 = %v (next %q), %v; want [fs-a] and a token", names(page), page.NextPageToken, err)
		}
		var rest []string
		for token := page.NextPageToken; token != ""; {
			p, err := f.svc.ListWithFilter(t.Context(), projectID, 1, token, filter)
			if err != nil {
				t.Fatal(err)
			}
			if len(p.Items) > 1 {
				t.Fatalf("page of size 1 returned %v", names(p))
			}
			rest = append(rest, names(p)...)
			token = p.NextPageToken
		}
		if !slices.Equal(rest, []string{"fs-b", "fs-c"}) {
			t.Fatalf("following the token yielded %v, want [fs-b fs-c]", rest)
		}
	})

	t.Run("bad page size", func(t *testing.T) {
		t.Parallel()
		_, err := f.svc.ListWithFilter(t.Context(), projectID, 5000, "", &storagepb.ListBucketsWithFilterRequest_Filters{FilesystemId: fsID1})
		if !errs.IsKind(err, errs.KindBadRequest) {
			t.Fatalf("page_size 5000 = %v, want BadRequest", err)
		}
	})
}
