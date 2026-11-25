package dal

import (
	"slices"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/testing/protocmp"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/compute/domain"
)

const rw = computepb.AttachedFilesystemSpec_READ_WRITE

func filesystem(id, name string) *computepb.Filesystem {
	return &computepb.Filesystem{
		Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: parent, Name: name},
		Spec: &computepb.FilesystemSpec{
			Type: computepb.FilesystemSpec_NETWORK_SSD,
			Size: &computepb.FilesystemSpec_SizeGibibytes{SizeGibibytes: 1},
		},
		Status: &computepb.FilesystemStatus{State: computepb.FilesystemStatus_READY, SizeBytes: 1 << 30, BlockSizeBytes: 4096},
	}
}

func TestFilesystemRepoRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := NewFilesystemRepo(store.NewMemory())
	a := filesystem("computefilesystem-e00a", "a")
	if err := r.Create(ctx, a); err != nil {
		t.Fatal(err)
	}
	got, err := r.GetByName(ctx, parent, "a")
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(a, got, protocmp.Transform()); diff != "" {
		t.Fatalf("read differs (-want +got):\n%s", diff)
	}
	if err := r.Create(ctx, filesystem("computefilesystem-e00b", "a")); !errs.IsKind(err, errs.KindAlreadyExists) {
		t.Fatalf("duplicate name: got %v, want AlreadyExists", err)
	}
	if err := r.Create(ctx, filesystem("computefilesystem-e00b", "b")); err != nil {
		t.Fatal(err)
	}
	page, err := r.List(ctx, parent, 1, "")
	if err != nil || len(page.Items) != 1 || page.NextPageToken == "" {
		t.Fatalf("List page 1 = %v, %v", page, err)
	}
	page, err = r.List(ctx, parent, 1, page.NextPageToken)
	if err != nil || len(page.Items) != 1 || page.Items[0].GetMetadata().GetName() != "b" || page.NextPageToken != "" {
		t.Fatalf("List page 2 = %v, %v", page, err)
	}
	if err := r.Delete(ctx, "computefilesystem-e00a"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Get(ctx, "computefilesystem-e00a"); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("Get after delete = %v", err)
	}
}

func TestFilesystemAttachments(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := NewFilesystemRepo(store.NewMemory())
	busy := filesystem("computefilesystem-busy", "busy")
	busy.Status.State = computepb.FilesystemStatus_UPDATING
	for _, fs := range []*computepb.Filesystem{filesystem("computefilesystem-ok", "ok"), busy} {
		if err := r.Create(ctx, fs); err != nil {
			t.Fatal(err)
		}
	}
	att, err := domain.NewFilesystemAttachments(r)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, fs, project string
		mode              computepb.AttachedFilesystemSpec_AttachMode
		want              errs.Kind
	}{
		{"missing", "computefilesystem-missing", parent, rw, errs.KindNotFound},
		{"other project", "computefilesystem-ok", "project-other", rw, errs.KindBadRequest},
		{"read only", "computefilesystem-ok", parent, computepb.AttachedFilesystemSpec_READ_ONLY, errs.KindBadRequest},
		{"not ready", "computefilesystem-busy", parent, rw, errs.KindBadState},
	} {
		if err := att.Attach(ctx, tc.fs, "computeinstance-a", tc.project, tc.mode); !errs.IsKind(err, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, err, tc.want)
		}
	}
	for _, inst := range []string{"computeinstance-a", "computeinstance-a", "computeinstance-b"} {
		if err := att.Attach(ctx, "computefilesystem-ok", inst, parent, rw); err != nil {
			t.Fatal(err)
		}
	}
	fs, _ := r.Get(ctx, "computefilesystem-ok")
	if got := fs.GetStatus().GetReadWriteAttachments(); !slices.Equal(got, []string{"computeinstance-a", "computeinstance-b"}) {
		t.Fatalf("read_write_attachments = %v", got)
	}
	if err := domain.CheckFilesystemDeletable(fs); !errs.IsKind(err, errs.KindBadState) {
		t.Fatalf("attached filesystem deletable: %v", err)
	}
	for _, inst := range []string{"computeinstance-a", "computeinstance-b", "computeinstance-b"} {
		if err := att.Detach(ctx, "computefilesystem-ok", inst); err != nil {
			t.Fatal(err)
		}
	}
	if err := att.Detach(ctx, "computefilesystem-missing", "computeinstance-a"); err != nil {
		t.Fatalf("detach from a missing filesystem: %v", err)
	}
	fs, _ = r.Get(ctx, "computefilesystem-ok")
	if err := domain.CheckFilesystemDeletable(fs); err != nil || len(fs.GetStatus().GetReadWriteAttachments()) != 0 {
		t.Fatalf("detached filesystem: %v, %v", fs.GetStatus(), err)
	}
}

func TestFilesystemExposures(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := NewFilesystemRepo(store.NewMemory())
	busy := filesystem("computefilesystem-busy", "busy")
	busy.Status.State = computepb.FilesystemStatus_DELETING
	for _, fs := range []*computepb.Filesystem{filesystem("computefilesystem-ok", "ok"), busy} {
		if err := r.Create(ctx, fs); err != nil {
			t.Fatal(err)
		}
	}
	exp, err := domain.NewFilesystemAttachments(r)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, fs, project string
		want              errs.Kind
	}{
		{"missing", "computefilesystem-missing", parent, errs.KindNotFound},
		{"other project", "computefilesystem-ok", "project-other", errs.KindBadRequest},
		{"deleting", "computefilesystem-busy", parent, errs.KindBadState},
	} {
		if err := exp.Expose(ctx, tc.fs, "storagebucket-a", tc.project); !errs.IsKind(err, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, err, tc.want)
		}
	}
	for _, b := range []string{"storagebucket-a", "storagebucket-a", "storagebucket-b"} {
		if err := exp.Expose(ctx, "computefilesystem-ok", b, parent); err != nil {
			t.Fatal(err)
		}
	}
	var seen []string
	if _, err := r.MutateExposed(ctx, "computefilesystem-ok", func(_ *computepb.Filesystem, b []string) error {
		seen = b
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(seen, []string{"storagebucket-a", "storagebucket-b"}) {
		t.Fatalf("exposed by %v", seen)
	}
	for _, b := range []string{"storagebucket-a", "storagebucket-b", "storagebucket-b"} {
		if err := exp.Unexpose(ctx, "computefilesystem-ok", b); err != nil {
			t.Fatalf("Unexpose(%s): %v", b, err)
		}
	}
	if _, err := r.MutateExposed(ctx, "computefilesystem-ok", func(_ *computepb.Filesystem, b []string) error {
		seen = b
		return nil
	}); err != nil || len(seen) != 0 {
		t.Fatalf("after unexpose: %v, %v", seen, err)
	}
}

// TestFilesystemExposeVersusDelete races a filesystem bucket's Expose against
// the Delete check (as FilesystemService.Delete runs it): exactly one wins.
func TestFilesystemExposeVersusDelete(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	for range 50 {
		r := NewFilesystemRepo(store.NewMemory())
		if err := r.Create(ctx, filesystem("computefilesystem-x", "x")); err != nil {
			t.Fatal(err)
		}
		exp, err := domain.NewFilesystemAttachments(r)
		if err != nil {
			t.Fatal(err)
		}
		var exposeErr, deleteErr error
		var wg sync.WaitGroup
		wg.Go(func() { exposeErr = exp.Expose(ctx, "computefilesystem-x", "storagebucket-a", parent) })
		wg.Go(func() {
			_, deleteErr = r.MutateExposed(ctx, "computefilesystem-x", func(fs *computepb.Filesystem, buckets []string) error {
				if len(buckets) > 0 {
					return errs.BadState("computefilesystem-x", "exposed")
				}
				if err := domain.CheckFilesystemDeletable(fs); err != nil {
					return err
				}
				fs.Status.State = computepb.FilesystemStatus_DELETING
				return nil
			})
		})
		wg.Wait()
		if (exposeErr == nil) == (deleteErr == nil) {
			t.Fatalf("expose err %v, delete err %v: want exactly one to succeed", exposeErr, deleteErr)
		}
	}
}

// TestFilesystemAttachVersusDelete races Attach against the Delete check
// (as FilesystemService.Delete runs it): exactly one of them wins.
func TestFilesystemAttachVersusDelete(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	for range 50 {
		r := NewFilesystemRepo(store.NewMemory())
		if err := r.Create(ctx, filesystem("computefilesystem-x", "x")); err != nil {
			t.Fatal(err)
		}
		att, err := domain.NewFilesystemAttachments(r)
		if err != nil {
			t.Fatal(err)
		}
		var attachErr, deleteErr error
		var wg sync.WaitGroup
		wg.Go(func() { attachErr = att.Attach(ctx, "computefilesystem-x", "computeinstance-a", parent, rw) })
		wg.Go(func() {
			_, deleteErr = r.Mutate(ctx, "computefilesystem-x", func(fs *computepb.Filesystem) error {
				if err := domain.CheckFilesystemDeletable(fs); err != nil {
					return err
				}
				fs.Status.State = computepb.FilesystemStatus_DELETING
				return nil
			})
		})
		wg.Wait()
		if (attachErr == nil) == (deleteErr == nil) {
			t.Fatalf("attach err %v, delete err %v: want exactly one to succeed", attachErr, deleteErr)
		}
	}
}
