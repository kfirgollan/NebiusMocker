package domain

import (
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/testing/protocmp"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

func (f *fsFixture) create(t *testing.T, name string, spec *computepb.FilesystemSpec) (*computepb.Filesystem, error) {
	t.Helper()
	op, err := f.svc.Create(t.Context(), CreateFilesystemCmd{
		Metadata: &commonpb.ResourceMetadata{ParentId: testProject, Name: name, Labels: map[string]string{"a": "b"}}, Spec: spec,
	})
	if err != nil {
		return nil, err
	}
	fs, gerr := f.repo.Get(t.Context(), op.ResourceID)
	if gerr != nil {
		t.Fatalf("created filesystem %s missing: %v", op.ResourceID, gerr)
	}
	return fs, op.Err
}

func (f *fsFixture) update(t *testing.T, cmd UpdateFilesystemCmd) error {
	t.Helper()
	_, err := f.svc.Update(t.Context(), cmd)
	return err
}

func TestFilesystemCreate(t *testing.T) {
	t.Parallel()
	f := newFsFixture(t)
	spec := filesystemSpecGiB(10)
	fs, err := f.create(t, "shared", spec)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if got := f.data.provisioned; len(got) != 1 || got[0] != fs.GetMetadata().GetId() {
		t.Fatalf("provisioned = %v, want the new filesystem", got)
	}
	want := &computepb.Filesystem{
		Metadata: fs.GetMetadata(),
		Spec:     spec, // echoed exactly as sent
		Status:   &computepb.FilesystemStatus{State: computepb.FilesystemStatus_READY, SizeBytes: 10 * gib, BlockSizeBytes: DefaultBlockSizeBytes},
	}
	if diff := cmp.Diff(want, fs, protocmp.Transform()); diff != "" {
		t.Fatalf("filesystem mismatch (-want +got):\n%s", diff)
	}
	md := fs.GetMetadata()
	if md.GetResourceVersion() != 1 || md.GetParentId() != testProject || md.GetName() != "shared" || md.GetLabels()["a"] != "b" ||
		!strings.HasPrefix(md.GetId(), KindFilesystem+"-") || !md.GetCreatedAt().AsTime().Equal(f.clk.Now()) {
		t.Fatalf("metadata = %v", md)
	}
}

func TestFilesystemCreateCreatingWhileDeferred(t *testing.T) {
	t.Parallel()
	f := newFsFixture(t)
	f.ops.deferred = true
	op, err := f.svc.Create(t.Context(), CreateFilesystemCmd{Metadata: &commonpb.ResourceMetadata{ParentId: testProject, Name: "x"}, Spec: filesystemSpecGiB(1)})
	if err != nil {
		t.Fatal(err)
	}
	fs := f.get(t, op.ResourceID)
	if fs.GetStatus().GetState() != computepb.FilesystemStatus_CREATING || !fs.GetStatus().GetReconciling() {
		t.Fatalf("status = %v, want CREATING", fs.GetStatus())
	}
	f.ops.finish()
	fs = f.get(t, op.ResourceID)
	if fs.GetStatus().GetState() != computepb.FilesystemStatus_READY || fs.GetStatus().GetReconciling() {
		t.Fatalf("status = %v, want READY", fs.GetStatus())
	}
}

func TestFilesystemCreateProvisionFailure(t *testing.T) {
	t.Parallel()
	f := newFsFixture(t)
	f.data.provisionErr = errProvision
	fs, err := f.create(t, "broken", filesystemSpecGiB(1))
	if err == nil || !strings.Contains(err.Error(), errProvision.Error()) {
		t.Fatalf("operation err = %v, want the provision error", err)
	}
	if fs.GetStatus().GetState() != computepb.FilesystemStatus_ERROR || fs.GetStatus().GetReconciling() ||
		!strings.Contains(fs.GetStatus().GetStateDescription(), errProvision.Error()) {
		t.Fatalf("status = %v, want ERROR with the cause", fs.GetStatus())
	}
}

func TestFilesystemCreateRejections(t *testing.T) {
	t.Parallel()
	withType := func(typ computepb.FilesystemSpec_FilesystemType) *computepb.FilesystemSpec {
		s := filesystemSpecGiB(10)
		s.Type = typ
		return s
	}
	withBlock := func(n int64) *computepb.FilesystemSpec {
		s := filesystemSpecGiB(10)
		s.BlockSizeBytes = n
		return s
	}
	tests := []struct {
		name  string
		md    *commonpb.ResourceMetadata
		spec  *computepb.FilesystemSpec
		kind  errs.Kind
		field string
	}{
		{"id set", &commonpb.ResourceMetadata{Id: "x", ParentId: testProject}, filesystemSpecGiB(10), errs.KindBadRequest, "metadata.id"},
		{"no parent", &commonpb.ResourceMetadata{}, filesystemSpecGiB(10), errs.KindBadRequest, "metadata.parent_id"},
		{"unknown parent", &commonpb.ResourceMetadata{ParentId: "project-nope"}, filesystemSpecGiB(10), errs.KindNotFound, ""},
		{"no type", &commonpb.ResourceMetadata{ParentId: testProject}, withType(computepb.FilesystemSpec_UNSPECIFIED), errs.KindBadRequest, "spec.type"},
		{"hdd", &commonpb.ResourceMetadata{ParentId: testProject}, withType(computepb.FilesystemSpec_NETWORK_HDD), errs.KindBadRequest, "spec.type"},
		{"no size", &commonpb.ResourceMetadata{ParentId: testProject}, &computepb.FilesystemSpec{Type: computepb.FilesystemSpec_NETWORK_SSD}, errs.KindBadRequest, "spec.size"},
		{"too small", &commonpb.ResourceMetadata{ParentId: testProject}, &computepb.FilesystemSpec{
			Type: computepb.FilesystemSpec_NETWORK_SSD, Size: &computepb.FilesystemSpec_SizeMebibytes{SizeMebibytes: 1023},
		}, errs.KindBadRequest, "spec.size"},
		{"too large", &commonpb.ResourceMetadata{ParentId: testProject}, filesystemSpecGiB(104857601), errs.KindBadRequest, "spec.size"},
		{"bad block size", &commonpb.ResourceMetadata{ParentId: testProject}, withBlock(5000), errs.KindBadRequest, "spec.block_size_bytes"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFsFixture(t)
			_, err := f.svc.Create(t.Context(), CreateFilesystemCmd{Metadata: tc.md, Spec: tc.spec})
			if !errs.IsKind(err, tc.kind) {
				t.Fatalf("err = %v, want kind %v", err, tc.kind)
			}
			if tc.field != "" && !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("err = %v, want it to name %s", err, tc.field)
			}
		})
	}
}

func TestFilesystemCreateDuplicateName(t *testing.T) {
	t.Parallel()
	f := newFsFixture(t)
	if _, err := f.create(t, "dup", filesystemSpecGiB(1)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Create(t.Context(), CreateFilesystemCmd{
		Metadata: &commonpb.ResourceMetadata{ParentId: testProject, Name: "dup"}, Spec: filesystemSpecGiB(1),
	}); !errs.IsKind(err, errs.KindAlreadyExists) {
		t.Fatalf("err = %v, want AlreadyExists", err)
	}
}

func TestFilesystemListAndGet(t *testing.T) {
	t.Parallel()
	f := newFsFixture(t)
	for _, n := range []string{"a", "b", "c"} {
		f.seed(t, "fs-"+n, nil)
	}
	p1, err := f.svc.List(t.Context(), testProject, 2, "", "")
	if err != nil || len(p1.Items) != 2 || p1.NextPageToken == "" {
		t.Fatalf("page 1 = %v, %v", p1, err)
	}
	p2, err := f.svc.List(t.Context(), testProject, 2, p1.NextPageToken, "")
	if err != nil || len(p2.Items) != 1 || p2.NextPageToken != "" {
		t.Fatalf("page 2 = %v, %v", p2, err)
	}
	if _, err := f.svc.List(t.Context(), testProject, 0, "", "name=a"); !errs.IsKind(err, errs.KindBadRequest) {
		t.Fatalf("filtered list err = %v, want BadRequest", err)
	}
	if _, err := f.svc.List(t.Context(), "", 0, "", ""); !errs.IsKind(err, errs.KindBadRequest) {
		t.Fatalf("list without parent err = %v", err)
	}
	if got, err := f.svc.GetByName(t.Context(), testProject, "fs-b"); err != nil || got.GetMetadata().GetId() != "fs-b" {
		t.Fatalf("GetByName = %v, %v", got, err)
	}
	if _, err := f.svc.Get(t.Context(), "nope"); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("Get(missing) = %v", err)
	}
	if _, err := f.svc.Get(t.Context(), ""); !errs.IsKind(err, errs.KindBadRequest) {
		t.Fatalf("Get(empty) = %v", err)
	}
}

func TestFilesystemUpdateLabelsAndNoop(t *testing.T) {
	t.Parallel()
	f := newFsFixture(t)
	f.seed(t, "fs-1", nil)
	f.clk.Advance(time.Hour)

	md := &commonpb.ResourceMetadata{Id: "fs-1", Name: "fs-1", Labels: map[string]string{"env": "dev"}}
	if err := f.update(t, UpdateFilesystemCmd{Metadata: md}); err != nil {
		t.Fatalf("update: %v", err)
	}
	fs := f.get(t, "fs-1")
	if fs.GetMetadata().GetResourceVersion() != 2 || fs.GetMetadata().GetLabels()["env"] != "dev" ||
		!fs.GetMetadata().GetUpdatedAt().AsTime().Equal(f.clk.Now()) || fs.GetMetadata().GetName() != "fs-1" {
		t.Fatalf("metadata after label change = %v", fs.GetMetadata())
	}
	f.clk.Advance(time.Hour)
	if err := f.update(t, UpdateFilesystemCmd{Metadata: md}); err != nil {
		t.Fatalf("no-op update: %v", err)
	}
	again := f.get(t, "fs-1")
	if again.GetMetadata().GetResourceVersion() != 2 || !again.GetMetadata().GetUpdatedAt().AsTime().Equal(fs.GetMetadata().GetUpdatedAt().AsTime()) {
		t.Fatalf("no-op update changed metadata: %v", again.GetMetadata())
	}
	if fs.GetStatus().GetState() != computepb.FilesystemStatus_READY {
		t.Fatalf("a label change must not leave READY, got %v", fs.GetStatus().GetState())
	}
}

func TestFilesystemUpdateEmptyNameKeepsStored(t *testing.T) {
	t.Parallel()
	f := newFsFixture(t)
	f.seed(t, "fs-1", nil)
	if err := f.update(t, UpdateFilesystemCmd{Metadata: &commonpb.ResourceMetadata{Id: "fs-1", Labels: map[string]string{"k": "v"}}}); err != nil {
		t.Fatal(err)
	}
	if got := f.get(t, "fs-1").GetMetadata().GetName(); got != "fs-1" {
		t.Fatalf("name = %q, want it kept", got)
	}
}

func TestFilesystemUpdateGrow(t *testing.T) {
	t.Parallel()
	f := newFsFixture(t)
	f.seed(t, "fs-1", nil)
	f.ops.deferred = true

	if err := f.update(t, UpdateFilesystemCmd{Metadata: &commonpb.ResourceMetadata{Id: "fs-1"}, Spec: filesystemSpecGiB(20)}); err != nil {
		t.Fatalf("grow: %v", err)
	}
	fs := f.get(t, "fs-1")
	if fs.GetStatus().GetState() != computepb.FilesystemStatus_UPDATING || !fs.GetStatus().GetReconciling() || fs.GetStatus().GetSizeBytes() != 10*gib {
		t.Fatalf("status during grow = %v, want UPDATING at the old size", fs.GetStatus())
	}
	f.ops.finish()
	fs = f.get(t, "fs-1")
	if fs.GetStatus().GetState() != computepb.FilesystemStatus_READY || fs.GetStatus().GetReconciling() || fs.GetStatus().GetSizeBytes() != 20*gib {
		t.Fatalf("status after grow = %v, want READY with 20 GiB", fs.GetStatus())
	}
	if fs.GetMetadata().GetResourceVersion() != 2 {
		t.Fatalf("resource_version = %d, want 2", fs.GetMetadata().GetResourceVersion())
	}
}

func TestFilesystemUpdateRejections(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		seed  func(*computepb.Filesystem)
		md    *commonpb.ResourceMetadata
		spec  *computepb.FilesystemSpec
		kind  errs.Kind
		field string
	}{
		{name: "shrink", spec: filesystemSpecGiB(5), kind: errs.KindBadRequest, field: "spec.size"},
		{name: "grow beyond the maximum", spec: filesystemSpecGiB(104857601), kind: errs.KindBadRequest, field: "spec.size"},
		{name: "rename", md: &commonpb.ResourceMetadata{Id: "fs-1", Name: "other"}, kind: errs.KindBadRequest, field: "metadata.name"},
		{
			name: "type change", spec: &computepb.FilesystemSpec{Type: computepb.FilesystemSpec_WEKA},
			kind: errs.KindBadRequest, field: "spec.type",
		},
		{
			name: "block size change", spec: &computepb.FilesystemSpec{BlockSizeBytes: 8192},
			kind: errs.KindBadRequest, field: "spec.block_size_bytes",
		},
		{
			name: "not READY", seed: func(fs *computepb.Filesystem) { fs.Status.State = computepb.FilesystemStatus_ERROR },
			spec: filesystemSpecGiB(20), kind: errs.KindBadState,
		},
		{name: "stale resource_version", md: &commonpb.ResourceMetadata{Id: "fs-1", ResourceVersion: 9}, kind: errs.KindConflict},
		{name: "parent change", md: &commonpb.ResourceMetadata{Id: "fs-1", ParentId: "project-other"}, kind: errs.KindBadRequest, field: "metadata.parent_id"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFsFixture(t)
			f.seed(t, "fs-1", tc.seed)
			md := tc.md
			if md == nil {
				md = &commonpb.ResourceMetadata{Id: "fs-1"}
			}
			err := f.update(t, UpdateFilesystemCmd{Metadata: md, Spec: tc.spec})
			if !errs.IsKind(err, tc.kind) {
				t.Fatalf("err = %v, want kind %v", err, tc.kind)
			}
			if tc.field != "" && !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("err = %v, want it to name %s", err, tc.field)
			}
			if got := f.get(t, "fs-1").GetMetadata().GetResourceVersion(); got != 1 {
				t.Fatalf("a rejected update changed the resource: resource_version = %d", got)
			}
		})
	}
}

func TestFilesystemUpdateForbidDeletionAndSameImmutables(t *testing.T) {
	t.Parallel()
	f := newFsFixture(t)
	f.seed(t, "fs-1", nil)
	spec := filesystemSpecGiB(10)
	spec.ForbidDeletion = true
	spec.BlockSizeBytes = DefaultBlockSizeBytes
	if err := f.update(t, UpdateFilesystemCmd{Metadata: &commonpb.ResourceMetadata{Id: "fs-1"}, Spec: spec}); err == nil ||
		!errs.IsKind(err, errs.KindBadRequest) {
		t.Fatalf("setting block_size_bytes that differs from the unset stored spec: err = %v, want immutable", err)
	}
	spec.BlockSizeBytes = 0
	if err := f.update(t, UpdateFilesystemCmd{Metadata: &commonpb.ResourceMetadata{Id: "fs-1"}, Spec: spec}); err != nil {
		t.Fatal(err)
	}
	fs := f.get(t, "fs-1")
	if !fs.GetSpec().GetForbidDeletion() || fs.GetStatus().GetState() != computepb.FilesystemStatus_READY || fs.GetMetadata().GetResourceVersion() != 2 {
		t.Fatalf("filesystem = %v", fs)
	}
}

func TestFilesystemDelete(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		seed    func(*computepb.Filesystem)
		buckets []string
		kind    errs.Kind
		want    string
	}{
		{name: "forbid_deletion", seed: func(fs *computepb.Filesystem) { fs.Spec.ForbidDeletion = true }, kind: errs.KindBadState, want: "forbid_deletion"},
		{
			name: "attached", seed: func(fs *computepb.Filesystem) { fs.Status.ReadWriteAttachments = []string{"computeinstance-1"} },
			kind: errs.KindBadState, want: "computeinstance-1",
		},
		{name: "bucket users", buckets: []string{"b1", "b2"}, kind: errs.KindBadState, want: "[b1, b2]"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFsFixture(t)
			f.seed(t, "fs-1", tc.seed)
			f.repo.mu.Lock()
			f.repo.exposed = map[string][]string{"fs-1": tc.buckets}
			f.repo.mu.Unlock()
			_, err := f.svc.Delete(t.Context(), "fs-1", nil)
			if !errs.IsKind(err, tc.kind) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %v mentioning %q", err, tc.kind, tc.want)
			}
			if got := f.get(t, "fs-1").GetStatus().GetState(); got != computepb.FilesystemStatus_READY {
				t.Fatalf("a refused delete left state %v", got)
			}
			if len(f.data.purged) != 0 {
				t.Fatalf("purged = %v after a refused delete", f.data.purged)
			}
		})
	}
}

func TestFilesystemDeletePurges(t *testing.T) {
	t.Parallel()
	f := newFsFixture(t)
	f.seed(t, "fs-1", nil)
	f.ops.deferred = true
	if _, err := f.svc.Delete(t.Context(), "fs-1", nil); err != nil {
		t.Fatal(err)
	}
	if st := f.get(t, "fs-1").GetStatus(); st.GetState() != computepb.FilesystemStatus_DELETING || !st.GetReconciling() {
		t.Fatalf("status = %v, want DELETING", st)
	}
	f.ops.finish()
	if got := f.data.purged; len(got) != 1 || got[0] != "fs-1" {
		t.Fatalf("purged = %v", got)
	}
	if _, err := f.repo.Get(t.Context(), "fs-1"); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("Get after delete = %v, want NotFound", err)
	}
	if _, err := f.svc.Delete(t.Context(), "fs-1", nil); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("second delete = %v, want NotFound", err)
	}
}

func TestFilesystemListOperationsByParent(t *testing.T) {
	t.Parallel()
	f := newFsFixture(t)
	for _, n := range []string{"a", "b", "c"} {
		if _, err := f.create(t, n, filesystemSpecGiB(1)); err != nil {
			t.Fatal(err)
		}
	}
	p1, err := f.svc.ListOperationsByParent(t.Context(), testProject, 2, "")
	if err != nil || len(p1.Items) != 2 || p1.NextPageToken == "" {
		t.Fatalf("page 1 = %v, %v", p1, err)
	}
	p2, err := f.svc.ListOperationsByParent(t.Context(), testProject, 2, p1.NextPageToken)
	if err != nil || len(p2.Items) != 1 || p2.NextPageToken != "" {
		t.Fatalf("page 2 = %v, %v", p2, err)
	}
	if p1.Items[0].Description != "Create filesystem" {
		t.Fatalf("description = %q", p1.Items[0].Description)
	}
}
