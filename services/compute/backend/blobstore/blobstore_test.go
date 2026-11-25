package blobstore

import (
	"bytes"
	"io"
	"testing"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore/memory"
)

func TestBlobs(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	st := memory.New(clock.Real{})
	b, err := New(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	const id = "computeimage-e00a"
	if _, err := b.StatImage(ctx, id); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("StatImage(missing) = %v", err)
	}
	info, err := b.PutImage(ctx, id, bytes.NewReader([]byte("hello")), 5, map[string]string{"sha256": "abc"})
	if err != nil || info.Size != 5 {
		t.Fatalf("PutImage = %+v, %v", info, err)
	}
	obj, err := st.Stat(ctx, Bucket, Key(id))
	if err != nil || obj.Metadata["sha256"] != "abc" || obj.Size != 5 {
		t.Fatalf("object = %+v, %v", obj, err)
	}
	if got, err := b.StatImage(ctx, id); err != nil || got.Metadata["sha256"] != "abc" {
		t.Fatalf("StatImage = %+v, %v", got, err)
	}
	if err := b.DeleteImage(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := b.DeleteImage(ctx, id); err != nil {
		t.Fatalf("second DeleteImage = %v", err)
	}

	if _, err := b.StatSource(ctx, "user", "x"); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("StatSource(missing bucket) = %v", err)
	}
	if err := st.EnsureBucket(ctx, "user"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.OpenSource(ctx, "user", "x"); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("OpenSource(missing object) = %v", err)
	}
	if _, err := st.Put(ctx, "user", "x", bytes.NewReader([]byte("data")), 4, objstore.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	r, info, err := b.OpenSource(ctx, "user", "x")
	if err != nil || info.Size != 4 {
		t.Fatalf("OpenSource = %+v, %v", info, err)
	}
	data, _ := io.ReadAll(r)
	_ = r.Close()
	if string(data) != "data" {
		t.Fatalf("OpenSource read %q", data)
	}
}
