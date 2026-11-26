//go:build e2e_storage

package s3endpoint_test

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/services/storage/backend/s3endpoint"
	"github.com/kfirgollan/NebiusMocker/services/storage/domain"
	"github.com/kfirgollan/NebiusMocker/test/rustfsfixture"
)

func TestMain(m *testing.M) { os.Exit(rustfsfixture.Main(m)) }

// epPattern generates n bytes of a repeating, position-dependent pattern.
type epPattern struct{ n, off int64 }

func (p *epPattern) Read(b []byte) (int, error) {
	if p.off >= p.n {
		return 0, io.EOF
	}
	k := int(min(int64(len(b)), p.n-p.off))
	for i := range k {
		b[i] = byte((p.off + int64(i)) * 31 >> 3)
	}
	p.off += int64(k)
	return k, nil
}

// epOpen opens an "external S3" side on a fresh RustFS bucket.
func epOpen(t *testing.T) (domain.Endpoint, domain.Side) {
	t.Helper()
	srv := rustfsfixture.Start(t)
	bucket := "ep-" + strings.ToLower(strings.NewReplacer("/", "-", "_", "-").Replace(t.Name()))
	bucket = bucket[:min(len(bucket), 60)]
	if err := srv.Client(t).EnsureBucket(t.Context(), bucket); err != nil {
		t.Fatal(err)
	}
	side := domain.Side{
		Kind: domain.ProviderS3Compatible, Endpoint: srv.Endpoint, Region: "us-east-1", Bucket: bucket,
		Creds: domain.Credentials{AccessKeyID: srv.AccessKey, Secret: srv.SecretKey},
	}
	ep, err := s3endpoint.New(s3endpoint.Config{}).Open(t.Context(), side)
	if err != nil {
		t.Fatal(err)
	}
	return ep, side
}

func TestPutGetHeadReplaceDelete(t *testing.T) {
	ep, _ := epOpen(t)
	ctx := t.Context()
	meta := domain.ObjectMeta{
		Size: 5, ContentType: "text/plain", CacheControl: "max-age=10", ContentDisposition: "attachment",
		ContentEncoding: "identity", ContentLanguage: "en", Metadata: map[string]string{"owner": "me"},
	}
	etag, err := ep.Put(ctx, "dir/a b+c.txt", strings.NewReader("hello"), meta)
	if err != nil || etag == "" {
		t.Fatalf("put = %q, %v", etag, err)
	}
	h, err := ep.Head(ctx, "dir/a b+c.txt")
	if err != nil {
		t.Fatal(err)
	}
	if h.Size != 5 || h.ETag != etag || h.ContentType != "text/plain" || h.CacheControl != "max-age=10" ||
		h.ContentDisposition != "attachment" || h.ContentLanguage != "en" || h.Metadata["owner"] != "me" || h.LastModified.IsZero() {
		t.Errorf("head = %+v", h)
	}
	rc, gm, err := ep.Get(ctx, "dir/a b+c.txt")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(body) != "hello" || gm.Size != 5 || gm.ETag != etag || gm.ContentType != "text/plain" || gm.Metadata["owner"] != "me" {
		t.Errorf("get = %q %+v", body, gm)
	}

	meta.ContentType = "application/json"
	meta.Metadata = map[string]string{"owner": "you", "extra": "1"}
	if _, err := ep.ReplaceMetadata(ctx, "dir/a b+c.txt", meta); err != nil {
		t.Fatal(err)
	}
	h, err = ep.Head(ctx, "dir/a b+c.txt")
	if err != nil {
		t.Fatal(err)
	}
	if h.ContentType != "application/json" || h.Metadata["owner"] != "you" || h.Metadata["extra"] != "1" || h.Size != 5 {
		t.Errorf("head after replace = %+v", h)
	}

	if err := ep.Delete(ctx, "dir/a b+c.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := ep.Head(ctx, "dir/a b+c.txt"); errs.KindOf(err) != errs.KindNotFound {
		t.Errorf("head after delete = %v, want NotFound", err)
	}
}

func TestMultipartPut(t *testing.T) {
	ep, _ := epOpen(t)
	ctx := t.Context()
	const size = domain.TransferMultipartThreshold + 7<<20 // three 50 MiB parts' worth: 2 parts
	etag, err := ep.Put(ctx, "big.bin", &epPattern{n: size}, domain.ObjectMeta{Size: size, ContentType: "application/x-big", Metadata: map[string]string{"k": "v"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(etag, "-") {
		t.Errorf("etag %q is not a multipart etag", etag)
	}
	h, err := ep.Head(ctx, "big.bin")
	if err != nil {
		t.Fatal(err)
	}
	if h.Size != size || h.ContentType != "application/x-big" || h.Metadata["k"] != "v" {
		t.Errorf("head = %+v", h)
	}
	rc, _, err := ep.Get(ctx, "big.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	got, want := sha256.New(), sha256.New()
	if _, err := io.Copy(got, rc); err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(want, &epPattern{n: size}); err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(got.Sum(nil)) != hex.EncodeToString(want.Sum(nil)) {
		t.Error("downloaded content differs from the uploaded content")
	}
}

func TestListPagesAndPrefix(t *testing.T) {
	ep, _ := epOpen(t)
	ctx := t.Context()
	const n = domain.TransferListPage + 100
	var wg sync.WaitGroup
	sem := make(chan struct{}, 16)
	errc := make(chan error, n)
	for i := range n {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			_, err := ep.Put(ctx, fmt.Sprintf("p/%05d", i), strings.NewReader("x"), domain.ObjectMeta{Size: 1})
			errc <- err
		})
	}
	wg.Wait()
	close(errc)
	for err := range errc {
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ep.Put(ctx, "q/other", strings.NewReader("y"), domain.ObjectMeta{Size: 1}); err != nil {
		t.Fatal(err)
	}

	count, prev := 0, ""
	for m, err := range ep.List(ctx, "p/") {
		if err != nil {
			t.Fatal(err)
		}
		if m.Key <= prev || m.Size != 1 || m.ETag == "" || strings.Contains(m.ETag, `"`) || m.LastModified.IsZero() {
			t.Fatalf("bad entry %+v after %q", m, prev)
		}
		prev = m.Key
		count++
	}
	if count != n {
		t.Errorf("listed %d objects, want %d", count, n)
	}
	// Stopping early must not leak or error.
	for _, err := range ep.List(ctx, "") {
		if err != nil {
			t.Fatal(err)
		}
		break
	}
}

func TestErrors(t *testing.T) {
	ep, side := epOpen(t)
	ctx := t.Context()
	if _, err := ep.Head(ctx, "missing"); errs.KindOf(err) != errs.KindNotFound {
		t.Errorf("head err = %v, want NotFound", err)
	}
	if _, _, err := ep.Get(ctx, "missing"); errs.KindOf(err) != errs.KindNotFound {
		t.Errorf("get err = %v, want NotFound", err)
	}

	noBucket := side
	noBucket.Bucket = "no-such-bucket"
	nb, err := s3endpoint.New(s3endpoint.Config{}).Open(ctx, noBucket)
	if err != nil {
		t.Fatal(err)
	}
	for _, err := range nb.List(ctx, "") {
		if ee, ok := errors.AsType[*domain.EndpointError](err); !ok || ee.Code != "NoSuchBucket" {
			t.Errorf("list err = %v, want NoSuchBucket", err)
		}
	}

	wrong := side
	wrong.Creds = domain.Credentials{AccessKeyID: side.Creds.AccessKeyID, Secret: "wrong-secret"}
	w, err := s3endpoint.New(s3endpoint.Config{}).Open(ctx, wrong)
	if err != nil {
		t.Fatal(err)
	}
	_, err = w.Head(ctx, "x")
	if ee, ok := errors.AsType[*domain.EndpointError](err); !ok || ee.Code == "" || ee.Code == "RequestError" {
		t.Errorf("wrong credentials err = %v, want an S3 error code", err)
	}
	_, err = w.Put(ctx, "x", strings.NewReader("z"), domain.ObjectMeta{Size: 1})
	if ee, ok := errors.AsType[*domain.EndpointError](err); !ok || ee.Code == "" || ee.Code == "RequestError" {
		t.Errorf("wrong credentials put err = %v, want an S3 error code", err)
	}

	anon := side
	anon.Creds = domain.Credentials{Anonymous: true}
	a, err := s3endpoint.New(s3endpoint.Config{}).Open(ctx, anon)
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.Put(ctx, "x", strings.NewReader("z"), domain.ObjectMeta{Size: 1})
	if ee, ok := errors.AsType[*domain.EndpointError](err); !ok || ee.Code != "AccessDenied" {
		t.Errorf("anonymous put err = %v, want AccessDenied", err)
	}
}
