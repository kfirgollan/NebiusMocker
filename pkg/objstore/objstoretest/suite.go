// Package objstoretest is the conformance suite every objstore.Store
// implementation passes, so the in-memory fake cannot drift from RustFS.
package objstoretest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/kfirgollan/NebiusMocker/pkg/objstore"
)

// Run exercises store. bucket must be a fresh, valid S3 bucket name that the
// suite may create and delete.
func Run(t *testing.T, store objstore.Store, bucket string) {
	t.Helper()
	ctx := t.Context()
	if err := store.EnsureBucket(ctx, bucket); err != nil {
		t.Fatalf("EnsureBucket: %v", err)
	}
	if err := store.EnsureBucket(ctx, bucket); err != nil {
		t.Fatalf("EnsureBucket (again, must be idempotent): %v", err)
	}
	t.Cleanup(func() {
		if err := store.DeleteBucket(context.WithoutCancel(t.Context()), bucket); err != nil {
			t.Errorf("cleanup DeleteBucket: %v", err)
		}
	})
	empty, err := store.BucketEmpty(ctx, bucket)
	if err != nil || !empty {
		t.Fatalf("BucketEmpty(new) = %v, %v; want true, nil", empty, err)
	}

	runObjects(t, store, bucket)
	runDeleteBucket(t, store, bucket)
}

// runObjects covers put/get/stat/copy/list/versioning/delete.
func runObjects(t *testing.T, store objstore.Store, bucket string) {
	t.Helper()
	ctx := t.Context()
	payload := []byte("hello, nebius mocker")
	info, err := store.Put(ctx, bucket, "dir/a.txt", bytes.NewReader(payload), int64(len(payload)),
		objstore.PutOptions{ContentType: "text/plain", Metadata: map[string]string{"owner": "suite"}})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if info.Size != int64(len(payload)) || info.ETag == "" {
		t.Errorf("Put info = %+v, want size %d and an ETag", info, len(payload))
	}
	// Unknown size is streamed (spooled by implementations that need a length).
	if _, err := store.Put(ctx, bucket, "dir/b.bin", io.MultiReader(strings.NewReader("ab"), strings.NewReader("cd")), -1, objstore.PutOptions{}); err != nil {
		t.Fatalf("Put (unknown size): %v", err)
	}

	checkGet(t, store, bucket, "dir/a.txt", payload)
	st, err := store.Stat(ctx, bucket, "dir/a.txt")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if st.ContentType != "text/plain" || st.Metadata["owner"] != "suite" || st.ETag != info.ETag {
		t.Errorf("Stat = %+v, want content type, metadata and ETag from Put (%s)", st, info.ETag)
	}

	if _, err := store.Copy(ctx, bucket, "dir/a.txt", bucket, "copy/a.txt"); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	checkGet(t, store, bucket, "copy/a.txt", payload)

	keys := listKeys(t, store, bucket, "dir/")
	if strings.Join(keys, ",") != "dir/a.txt,dir/b.bin" {
		t.Errorf("List(dir/) = %v, want [dir/a.txt dir/b.bin]", keys)
	}

	runVersionedDelete(t, store, bucket)
}

func runVersionedDelete(t *testing.T, store objstore.Store, bucket string) {
	t.Helper()
	ctx := t.Context()
	if err := store.SetVersioning(ctx, bucket, objstore.VersioningEnabled); err != nil {
		t.Fatalf("SetVersioning(enabled): %v", err)
	}
	// Overwrite under versioning, then delete: old versions must not block DeleteBucket.
	if _, err := store.Put(ctx, bucket, "dir/a.txt", strings.NewReader("v2"), 2, objstore.PutOptions{}); err != nil {
		t.Fatalf("Put v2: %v", err)
	}
	checkGet(t, store, bucket, "dir/a.txt", []byte("v2"))

	for _, k := range []string{"dir/a.txt", "dir/b.bin", "copy/a.txt", "never-existed"} {
		if err := store.Delete(ctx, bucket, k); err != nil {
			t.Fatalf("Delete(%s): %v", k, err)
		}
	}
	if _, err := store.Stat(ctx, bucket, "dir/a.txt"); !errors.Is(err, objstore.ErrNotFound) {
		t.Errorf("Stat after delete: err = %v, want ErrNotFound", err)
	}
	if _, _, err := store.Get(ctx, bucket, "dir/a.txt"); !errors.Is(err, objstore.ErrNotFound) {
		t.Errorf("Get after delete: err = %v, want ErrNotFound", err)
	}
	if empty, err := store.BucketEmpty(ctx, bucket); err != nil || !empty {
		t.Errorf("BucketEmpty after deletes = %v, %v; want true, nil", empty, err)
	}
	if err := store.SetVersioning(ctx, bucket, objstore.VersioningSuspended); err != nil {
		t.Fatalf("SetVersioning(suspended): %v", err)
	}
}

func runDeleteBucket(t *testing.T, store objstore.Store, bucket string) {
	t.Helper()
	ctx := t.Context()
	if err := store.DeleteBucket(ctx, bucket); err != nil {
		t.Fatalf("DeleteBucket: %v", err)
	}
	if err := store.DeleteBucket(ctx, bucket); err != nil {
		t.Fatalf("DeleteBucket (missing) = %v, want nil", err)
	}
	if _, err := store.BucketEmpty(ctx, bucket); !errors.Is(err, objstore.ErrBucketNotFound) {
		t.Errorf("BucketEmpty(deleted) err = %v, want ErrBucketNotFound", err)
	}
	if _, err := store.Put(ctx, bucket, "x", strings.NewReader("x"), 1, objstore.PutOptions{}); !errors.Is(err, objstore.ErrBucketNotFound) {
		t.Errorf("Put into deleted bucket err = %v, want ErrBucketNotFound", err)
	}
}

func checkGet(t *testing.T, store objstore.Store, bucket, key string, want []byte) {
	t.Helper()
	rc, info, err := store.Get(t.Context(), bucket, key)
	if err != nil {
		t.Fatalf("Get(%s): %v", key, err)
	}
	defer func() { _ = rc.Close() }() // read-only
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read %s: %v", key, err)
	}
	if !bytes.Equal(got, want) || info.Size != int64(len(want)) {
		t.Errorf("Get(%s) = %q (size %d), want %q", key, got, info.Size, want)
	}
}

func listKeys(t *testing.T, store objstore.Store, bucket, prefix string) []string {
	t.Helper()
	var keys []string
	for info, err := range store.List(t.Context(), bucket, prefix) {
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		keys = append(keys, info.Key)
	}
	return keys
}
