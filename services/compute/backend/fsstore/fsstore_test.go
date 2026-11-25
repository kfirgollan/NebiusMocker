package fsstore

import (
	"bytes"
	"strings"
	"testing"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/nid"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore/memory"
)

func TestBucketName(t *testing.T) {
	t.Parallel()
	gen, err := nid.NewGenerator(nid.Random, "e00", 0)
	if err != nil {
		t.Fatal(err)
	}
	id, err := gen.New("computefilesystem")
	if err != nil {
		t.Fatal(err)
	}
	b := New(memory.New(clock.Real{})).Bucket(id)
	if !strings.HasPrefix(b, "nebius-mocker-fs-computefilesystem-") || len(b) > 63 || b != strings.ToLower(b) {
		t.Fatalf("Bucket(%s) = %q, want a lower-case S3 name of at most 63 chars under the reserved prefix", id, b)
	}
}

func TestProvisionPurge(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	mem := memory.New(clock.Real{})
	s := New(mem)
	const id = "computefilesystem-e00abc"

	if err := s.Purge(ctx, id); err != nil {
		t.Fatalf("Purge(missing) = %v, want nil", err)
	}
	for range 2 { // idempotent
		if err := s.Provision(ctx, id); err != nil {
			t.Fatalf("Provision = %v", err)
		}
	}
	if _, err := mem.Put(ctx, s.Bucket(id), "dir/file", bytes.NewReader([]byte("hi")), 2, objstore.PutOptions{}); err != nil {
		t.Fatalf("put into the filesystem bucket: %v", err)
	}
	if err := s.Purge(ctx, id); err != nil {
		t.Fatalf("Purge = %v", err)
	}
	if _, err := mem.Stat(ctx, s.Bucket(id), "dir/file"); err == nil {
		t.Fatal("object survived Purge")
	}
	if err := s.Purge(ctx, id); err != nil {
		t.Fatalf("second Purge = %v", err)
	}
}
