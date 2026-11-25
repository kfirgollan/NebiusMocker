package objstoretest

import (
	"errors"
	"strings"
	"testing"

	"github.com/kfirgollan/NebiusMocker/pkg/objstore"
)

// runInventory covers the Inventory behaviour every backend shares.
func runInventory(t *testing.T, store objstore.Store, inv objstore.Inventory, bucket string) {
	t.Helper()
	ctx := t.Context()

	for u, err := range inv.ListUploads(ctx, bucket) {
		t.Fatalf("ListUploads(fresh bucket) = %+v, %v; want nothing", u, err)
	}
	for _, err := range inv.ListUploads(ctx, "no-such-bucket-"+bucket) {
		if !errors.Is(err, objstore.ErrBucketNotFound) {
			t.Errorf("ListUploads(missing bucket) err = %v, want ErrBucketNotFound", err)
		}
	}

	for _, o := range []struct{ key, body string }{{"inv/one", "1"}, {"inv/two", "22"}} {
		if _, err := store.Put(ctx, bucket, o.key, strings.NewReader(o.body), int64(len(o.body)), objstore.PutOptions{}); err != nil {
			t.Fatalf("Put(%s): %v", o.key, err)
		}
	}
	versions := listVersions(t, inv, bucket, "inv/")
	if len(versions) != 2 {
		t.Fatalf("ListVersions(inv/) = %+v, want 2 versions", versions)
	}
	for i, want := range []struct {
		key  string
		size int64
	}{{"inv/one", 1}, {"inv/two", 2}} {
		v := versions[i]
		if v.Key != want.key || v.Size != want.size || !v.IsLatest || v.DeleteMarker || v.ETag == "" || v.StorageClass != "STANDARD" {
			t.Errorf("version %d = %+v, want latest %s of size %d with ETag and STANDARD class", i, v, want.key, want.size)
		}
		if v.Multipart() {
			t.Errorf("version %+v of a plain put reports Multipart", v)
		}
	}

	runInventoryDelete(t, store, inv, bucket, versions[0].VersionID)
}

func runInventoryDelete(t *testing.T, store objstore.Store, inv objstore.Inventory, bucket, versionID string) {
	t.Helper()
	ctx := t.Context()
	tags, err := inv.ObjectTags(ctx, bucket, "inv/one", versionID)
	if err != nil || tags == nil || len(tags) != 0 {
		t.Errorf("ObjectTags = %v, %v; want empty non-nil map", tags, err)
	}

	if err := inv.DeleteVersion(ctx, bucket, "inv/one", versionID); err != nil {
		t.Fatalf("DeleteVersion: %v", err)
	}
	if err := inv.DeleteVersion(ctx, bucket, "inv/one", versionID); err != nil {
		t.Errorf("DeleteVersion (again) = %v, want nil", err)
	}
	if got := listVersions(t, inv, bucket, "inv/"); len(got) != 1 || got[0].Key != "inv/two" {
		t.Errorf("ListVersions after DeleteVersion = %+v, want only inv/two", got)
	}
	if err := inv.AbortUpload(ctx, bucket, "inv/two", "no-such-upload"); err != nil {
		t.Errorf("AbortUpload(missing) = %v, want nil", err)
	}
	if err := store.Delete(ctx, bucket, "inv/two"); err != nil {
		t.Fatalf("Delete(inv/two): %v", err)
	}
}

func listVersions(t *testing.T, inv objstore.Inventory, bucket, prefix string) []objstore.Version {
	t.Helper()
	var out []objstore.Version
	for v, err := range inv.ListVersions(t.Context(), bucket, prefix) {
		if err != nil {
			t.Fatalf("ListVersions: %v", err)
		}
		out = append(out, v)
	}
	return out
}
