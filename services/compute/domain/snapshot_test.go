package domain

import (
	"slices"
	"strings"
	"testing"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

func (f *diskFixture) createSnapshot(t *testing.T, parent, name, diskID, description string) (string, error) {
	t.Helper()
	op, err := f.ss.Create(t.Context(), CreateSnapshotCmd{
		Metadata: &commonpb.ResourceMetadata{ParentId: parent, Name: name},
		Spec:     &computepb.DiskSnapshotSpec{SourceDiskId: diskID, Description: description},
	})
	if err != nil {
		return "", err
	}
	return op.ResourceID, op.Err
}

func (f *diskFixture) mustGetSnapshot(t *testing.T, id string) *computepb.DiskSnapshot {
	t.Helper()
	s, err := f.snaps.Get(t.Context(), id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	return s
}

func TestSnapshotCreate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		parent      string
		diskID      string
		description string
		seed        func(*computepb.Disk)
		wantKind    errs.Kind
	}{
		{name: "ok", parent: testProject, diskID: "disk-1"},
		{name: "description at the limit", parent: testProject, diskID: "disk-1", description: strings.Repeat("x", 1024)},
		{name: "description too long", parent: testProject, diskID: "disk-1", description: strings.Repeat("x", 1025), wantKind: errs.KindBadRequest},
		{name: "multi-byte characters count once", parent: testProject, diskID: "disk-1", description: strings.Repeat("é", 1024)},
		{name: "missing disk", parent: testProject, diskID: "disk-none", wantKind: errs.KindNotFound},
		{name: "no disk id", parent: testProject, diskID: "", wantKind: errs.KindBadRequest},
		{name: "no parent", parent: "", diskID: "disk-1", wantKind: errs.KindBadRequest},
		{name: "unknown project", parent: "project-e00mock00000000unknown", diskID: "disk-1", wantKind: errs.KindNotFound},
		{name: "disk in another project", parent: otherProject, diskID: "disk-1", wantKind: errs.KindBadRequest},
		{
			name: "disk not ready", parent: testProject, diskID: "disk-1",
			seed:     func(d *computepb.Disk) { d.Status.State = computepb.DiskStatus_CREATING },
			wantKind: errs.KindBadState,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newDiskFixture(t)
			f.seedDisk(t, "disk-1", tt.seed)
			_, err := f.createSnapshot(t, tt.parent, "snap", tt.diskID, tt.description)
			if got := errs.KindOf(err); got != tt.wantKind {
				t.Fatalf("Create kind = %v, want %v (err: %v)", got, tt.wantKind, err)
			}
			if err != nil && len(f.ops.ops) != 0 {
				t.Fatalf("a failed validation created %d operations", len(f.ops.ops))
			}
		})
	}
}

func TestSnapshotCreateCopiesDiskStatus(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		arch computepb.DiskStatus_SourceImageCPUArchitecture
		want computepb.DiskSnapshotStatus_CPUArchitecture
	}{
		{name: "amd64", arch: computepb.DiskStatus_AMD64, want: computepb.DiskSnapshotStatus_AMD64},
		{name: "arm64", arch: computepb.DiskStatus_ARM64, want: computepb.DiskSnapshotStatus_ARM64},
		{name: "unknown", arch: computepb.DiskStatus_SOURCE_IMAGE_CPU_UNSPECIFIED, want: computepb.DiskSnapshotStatus_UNDEFINED},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newDiskFixture(t)
			f.seedDisk(t, "disk-1", func(d *computepb.Disk) {
				d.Status.SizeBytes = 25 * gib
				d.Status.SourceImageCpuArchitecture = tt.arch
			})
			id, err := f.createSnapshot(t, testProject, "snap", "disk-1", "")
			if err != nil {
				t.Fatal(err)
			}
			snap := f.mustGetSnapshot(t, id)
			st := snap.GetStatus()
			if st.GetState() != computepb.DiskSnapshotStatus_READY || st.GetContentSizeBytes() != 25*gib ||
				st.GetStorageSizeBytes() != 25*gib || st.GetSourceCpuArchitecture() != tt.want {
				t.Fatalf("status = %v, want READY, 25 GiB, %v", st, tt.want)
			}
			if snap.GetSpec().GetSourceDiskId() != "disk-1" || snap.GetMetadata().GetParentId() != testProject ||
				snap.GetMetadata().GetResourceVersion() != 1 {
				t.Fatalf("snapshot = %v", snap)
			}
		})
	}
}

func TestSnapshotCreateDuplicateName(t *testing.T) {
	t.Parallel()
	f := newDiskFixture(t)
	f.seedDisk(t, "disk-1", nil)
	if _, err := f.createSnapshot(t, testProject, "dup", "disk-1", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := f.createSnapshot(t, testProject, "dup", "disk-1", ""); !errs.IsKind(err, errs.KindAlreadyExists) {
		t.Fatalf("second create err = %v, want already exists", err)
	}
	if got := f.mustGetDisk(t, "disk-1").GetStatus().GetLockState().GetSnapshots(); len(got) != 0 {
		t.Fatalf("a failed create left the disk locked by %v", got)
	}
}

func TestSnapshotCreateLocksDiskWhileCreating(t *testing.T) {
	t.Parallel()
	f := newDiskFixture(t)
	f.seedDisk(t, "disk-1", nil)
	f.ops.deferred = true

	id, err := f.createSnapshot(t, testProject, "snap", "disk-1", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := f.mustGetDisk(t, "disk-1").GetStatus().GetLockState().GetSnapshots(); !slices.Equal(got, []string{id}) {
		t.Fatalf("disk lock_state.snapshots = %v, want [%s]", got, id)
	}
	if st := f.mustGetSnapshot(t, id).GetStatus().GetState(); st != computepb.DiskSnapshotStatus_CREATING {
		t.Fatalf("snapshot state = %v, want CREATING", st)
	}
	if _, err := f.ds.Delete(t.Context(), "disk-1", nil); !errs.IsKind(err, errs.KindBadState) {
		t.Fatalf("disk delete while snapshotting: err = %v, want bad state", err)
	}

	f.ops.finish()
	if got := f.mustGetDisk(t, "disk-1").GetStatus().GetLockState().GetSnapshots(); len(got) != 0 {
		t.Fatalf("disk lock_state.snapshots after create = %v, want empty", got)
	}
	if st := f.mustGetSnapshot(t, id).GetStatus().GetState(); st != computepb.DiskSnapshotStatus_READY {
		t.Fatalf("snapshot state = %v, want READY", st)
	}
	if _, err := f.ds.Delete(t.Context(), "disk-1", nil); err != nil {
		t.Fatalf("disk delete after the snapshot is READY: %v", err)
	}
}

func TestSnapshotUpdate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		seed     func(*computepb.DiskSnapshot)
		md       *commonpb.ResourceMetadata
		spec     *computepb.DiskSnapshotSpec
		wantKind errs.Kind
		wantVer  int64
	}{
		{
			name: "description", md: &commonpb.ResourceMetadata{Id: "snap-1"},
			spec: &computepb.DiskSnapshotSpec{Description: "new"}, wantVer: 2,
		},
		{
			name: "labels", md: &commonpb.ResourceMetadata{Id: "snap-1", Labels: map[string]string{"a": "b"}}, wantVer: 2,
		},
		{name: "name", md: &commonpb.ResourceMetadata{Id: "snap-1", Name: "renamed"}, wantVer: 2},
		{name: "no change", md: &commonpb.ResourceMetadata{Id: "snap-1"}, wantVer: 1},
		{
			name: "same source disk", md: &commonpb.ResourceMetadata{Id: "snap-1"},
			spec: &computepb.DiskSnapshotSpec{SourceDiskId: "disk-1"}, wantVer: 1,
		},
		{
			name: "source disk change", md: &commonpb.ResourceMetadata{Id: "snap-1"},
			spec: &computepb.DiskSnapshotSpec{SourceDiskId: "disk-2"}, wantKind: errs.KindBadRequest,
		},
		{
			name: "description too long", md: &commonpb.ResourceMetadata{Id: "snap-1"},
			spec: &computepb.DiskSnapshotSpec{Description: strings.Repeat("x", 1025)}, wantKind: errs.KindBadRequest,
		},
		{
			name: "version mismatch", md: &commonpb.ResourceMetadata{Id: "snap-1", ResourceVersion: 9, Name: "x"},
			wantKind: errs.KindConflict,
		},
		{
			name: "parent change", md: &commonpb.ResourceMetadata{Id: "snap-1", ParentId: otherProject, Name: "x"},
			wantKind: errs.KindBadRequest,
		},
		{
			name: "not ready", md: &commonpb.ResourceMetadata{Id: "snap-1", Name: "x"},
			seed:     func(s *computepb.DiskSnapshot) { s.Status.State = computepb.DiskSnapshotStatus_CREATING },
			wantKind: errs.KindBadState,
		},
		{name: "missing", md: &commonpb.ResourceMetadata{Id: "snap-none"}, wantKind: errs.KindNotFound},
		{name: "no id", md: &commonpb.ResourceMetadata{}, wantKind: errs.KindBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newDiskFixture(t)
			f.seedDisk(t, "disk-1", nil)
			f.seedSnapshot(t, "snap-1", testProject, "disk-1", tt.seed)
			_, err := f.ss.Update(t.Context(), UpdateSnapshotCmd{Metadata: tt.md, Spec: tt.spec})
			if got := errs.KindOf(err); got != tt.wantKind {
				t.Fatalf("Update kind = %v, want %v (err: %v)", got, tt.wantKind, err)
			}
			snap := f.mustGetSnapshot(t, "snap-1")
			wantVer := tt.wantVer
			if tt.wantKind != errs.KindUnknown {
				wantVer = 1
			}
			if got := snap.GetMetadata().GetResourceVersion(); got != wantVer {
				t.Fatalf("resource_version = %d, want %d", got, wantVer)
			}
			if snap.GetSpec().GetSourceDiskId() != "disk-1" {
				t.Fatalf("source_disk_id = %q, want it unchanged", snap.GetSpec().GetSourceDiskId())
			}
		})
	}
}

func TestSnapshotUpdateAppliesFields(t *testing.T) {
	t.Parallel()
	f := newDiskFixture(t)
	f.seedDisk(t, "disk-1", nil)
	f.seedSnapshot(t, "snap-1", testProject, "disk-1", nil)
	_, err := f.ss.Update(t.Context(), UpdateSnapshotCmd{
		Metadata: &commonpb.ResourceMetadata{Id: "snap-1", Labels: map[string]string{"env": "dev"}},
		Spec:     &computepb.DiskSnapshotSpec{Description: "nightly"},
	})
	if err != nil {
		t.Fatal(err)
	}
	snap := f.mustGetSnapshot(t, "snap-1")
	if snap.GetSpec().GetDescription() != "nightly" || snap.GetMetadata().GetLabels()["env"] != "dev" ||
		snap.GetStatus().GetContentSizeBytes() != 10*gib {
		t.Fatalf("snapshot after update = %v", snap)
	}
}

func TestSnapshotDelete(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		seed     func(*computepb.DiskSnapshot)
		wantKind errs.Kind
	}{
		{name: "plain snapshot"},
		{
			name: "locked by a disk being created",
			seed: func(s *computepb.DiskSnapshot) {
				s.Status.LockState = &computepb.DiskSnapshotStatus_LockState{Disks: []string{"disk-new"}}
			},
			wantKind: errs.KindBadState,
		},
		{
			name: "empty lock state",
			seed: func(s *computepb.DiskSnapshot) { s.Status.LockState = &computepb.DiskSnapshotStatus_LockState{} },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newDiskFixture(t)
			f.seedSnapshot(t, "snap-1", testProject, "disk-1", tt.seed)
			op, err := f.ss.Delete(t.Context(), "snap-1", nil)
			if got := errs.KindOf(err); got != tt.wantKind {
				t.Fatalf("Delete kind = %v, want %v (err: %v)", got, tt.wantKind, err)
			}
			_, gerr := f.snaps.Get(t.Context(), "snap-1")
			if tt.wantKind == errs.KindUnknown {
				if op.Err != nil || !errs.IsKind(gerr, errs.KindNotFound) {
					t.Fatalf("after delete: op.Err = %v, Get = %v, want removed", op.Err, gerr)
				}
				return
			}
			if gerr != nil {
				t.Fatalf("a refused delete removed the snapshot: %v", gerr)
			}
			if st := f.mustGetSnapshot(t, "snap-1").GetStatus().GetState(); st != computepb.DiskSnapshotStatus_READY {
				t.Fatalf("state after a refused delete = %v, want READY", st)
			}
		})
	}
}

func TestSnapshotDeleteMissingAndMarksDeleting(t *testing.T) {
	t.Parallel()
	f := newDiskFixture(t)
	if _, err := f.ss.Delete(t.Context(), "nope", nil); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("delete of a missing snapshot: err = %v, want not found", err)
	}
	if _, err := f.ss.Delete(t.Context(), "", nil); !errs.IsKind(err, errs.KindBadRequest) {
		t.Fatalf("delete without an id: err = %v, want bad request", err)
	}
	f.seedSnapshot(t, "snap-1", testProject, "disk-1", nil)
	f.ops.deferred = true
	if _, err := f.ss.Delete(t.Context(), "snap-1", nil); err != nil {
		t.Fatal(err)
	}
	if st := f.mustGetSnapshot(t, "snap-1").GetStatus().GetState(); st != computepb.DiskSnapshotStatus_DELETING {
		t.Fatalf("state during delete = %v, want DELETING", st)
	}
	f.ops.finish()
	if _, err := f.ss.Get(t.Context(), "snap-1"); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("Get after delete: err = %v, want not found", err)
	}
}

func TestSnapshotListByDisk(t *testing.T) {
	t.Parallel()
	f := newDiskFixture(t)
	f.seedDisk(t, "disk-1", nil)
	f.seedDisk(t, "disk-2", nil)
	f.seedSnapshot(t, "s1", testProject, "disk-1", nil)
	f.seedSnapshot(t, "s2", testProject, "disk-2", nil)
	f.seedSnapshot(t, "s3", testProject, "disk-1", nil)
	f.seedSnapshot(t, "s4", testProject, "disk-1", nil)

	if _, err := f.ss.ListByDisk(t.Context(), "disk-none", 0, ""); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("missing disk: err = %v, want not found", err)
	}
	if _, err := f.ss.ListByDisk(t.Context(), "", 0, ""); !errs.IsKind(err, errs.KindBadRequest) {
		t.Fatalf("empty disk id: err = %v, want bad request", err)
	}
	if _, err := f.ss.ListByDisk(t.Context(), "disk-1", -1, ""); !errs.IsKind(err, errs.KindBadRequest) {
		t.Fatalf("negative page size: err = %v, want bad request", err)
	}

	var got []string
	token := ""
	for page := range 5 {
		p, err := f.ss.ListByDisk(t.Context(), "disk-1", 2, token)
		if err != nil {
			t.Fatal(err)
		}
		if page == 0 && (len(p.Items) != 2 || p.NextPageToken == "") {
			t.Fatalf("first page = %v", p)
		}
		for _, s := range p.Items {
			got = append(got, s.GetMetadata().GetId())
		}
		if token = p.NextPageToken; token == "" {
			break
		}
	}
	if want := []string{"s1", "s3", "s4"}; !slices.Equal(got, want) {
		t.Fatalf("ListByDisk(disk-1) = %v, want %v in creation order", got, want)
	}
	p, err := f.ss.ListByDisk(t.Context(), "disk-2", 0, "")
	if err != nil || len(p.Items) != 1 || p.Items[0].GetMetadata().GetId() != "s2" {
		t.Fatalf("ListByDisk(disk-2) = %v, %v", p, err)
	}
}

func TestSnapshotListGetByName(t *testing.T) {
	t.Parallel()
	f := newDiskFixture(t)
	f.seedSnapshot(t, "s1", testProject, "disk-1", nil)
	f.seedSnapshot(t, "s2", testProject, "disk-1", nil)
	f.seedSnapshot(t, "s3", otherProject, "disk-1", nil)

	p, err := f.ss.List(t.Context(), testProject, 1, "")
	if err != nil || len(p.Items) != 1 || p.NextPageToken == "" {
		t.Fatalf("first page = %v, %v", p, err)
	}
	p2, err := f.ss.List(t.Context(), testProject, 1, p.NextPageToken)
	if err != nil || len(p2.Items) != 1 || p2.NextPageToken != "" || p2.Items[0].GetMetadata().GetId() != "s2" {
		t.Fatalf("second page = %v, %v", p2, err)
	}
	if _, err := f.ss.List(t.Context(), "", 0, ""); !errs.IsKind(err, errs.KindBadRequest) {
		t.Fatalf("List without parent: err = %v", err)
	}
	s, err := f.ss.GetByName(t.Context(), otherProject, "s3")
	if err != nil || s.GetMetadata().GetId() != "s3" {
		t.Fatalf("GetByName = %v, %v", s, err)
	}
	if _, err := f.ss.GetByName(t.Context(), testProject, "s3"); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("GetByName in the wrong project: err = %v, want not found", err)
	}
	if _, err := f.ss.Get(t.Context(), ""); !errs.IsKind(err, errs.KindBadRequest) {
		t.Fatalf("Get without id: err = %v", err)
	}
}
