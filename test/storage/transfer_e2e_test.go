//go:build e2e_storage

package storage

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"
	storagev1alpha1pb "github.com/nebius/gosdk/proto/nebius/storage/v1alpha1"
	storagesvc "github.com/nebius/gosdk/services/nebius/storage/v1"
	storagev1alpha1svc "github.com/nebius/gosdk/services/nebius/storage/v1alpha1"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/test/azuritefixture"
	"github.com/kfirgollan/NebiusMocker/test/rustfsfixture"
)

// --- helpers (prefix trx) ----------------------------------------------

// trxEnv is a mocker on a manual clock plus an editor key and S3 client.
// Nebius-bucket objects are only ever written and read through the S3 front
// with this key, and transfers are only managed through gosdk.
type trxEnv struct {
	*env
	clk    *clock.Manual
	keyID  string
	secret string
	s3     *s3.Client
	svc    storagesvc.TransferService
}

func trxStart(t *testing.T) *trxEnv {
	t.Helper()
	clk := clock.NewManual(clock.Real{}.Now())
	e := startWithClock(t, clk)
	keyID, secret, _ := e.grantedKey(t, "editor")
	return &trxEnv{env: e, clk: clk, keyID: keyID, secret: secret, s3: e.s3Client(keyID, secret), svc: storagesvc.NewTransferService(e.sdk)}
}

func (e *trxEnv) bucket(t *testing.T, tag string) string {
	t.Helper()
	name := unique(tag)
	e.createBucket(t, name, &storagepb.BucketSpec{})
	return name
}

// nebiusSrc and nebiusDst address a bucket of the mocker with the editor key.
func (e *trxEnv) nebiusSrc(bucket, prefix string) *storagepb.TransferSource {
	return trxNebiusSrc(bucket, prefix, e.keyID, e.secret)
}

func (e *trxEnv) nebiusDst(bucket, prefix string) *storagepb.TransferDestination {
	return trxNebiusDst(bucket, prefix, e.keyID, e.secret)
}

func trxNebiusSrc(bucket, prefix, keyID, secret string) *storagepb.TransferSource {
	return &storagepb.TransferSource{
		Prefix: prefix,
		Provider: &storagepb.TransferSource_Nebius{Nebius: &storagepb.TransferSource_NebiusProvider{
			Region: "eu-north1", BucketName: bucket,
			Credentials: &storagepb.TransferSource_NebiusProvider_AccessKey{AccessKey: &storagepb.TransferCredentialsAccessKey{
				AccessKeyId: keyID, SecretAccessKey: secret,
			}},
		}},
	}
}

func trxNebiusDst(bucket, prefix, keyID, secret string) *storagepb.TransferDestination {
	return &storagepb.TransferDestination{
		Prefix: prefix,
		Provider: &storagepb.TransferDestination_Nebius{Nebius: &storagepb.TransferDestination_NebiusProvider{
			Region: "eu-north1", BucketName: bucket,
			Credentials: &storagepb.TransferDestination_NebiusProvider_AccessKey{AccessKey: &storagepb.TransferCredentialsAccessKey{
				AccessKeyId: keyID, SecretAccessKey: secret,
			}},
		}},
	}
}

func trxS3Src(ext *rustfsfixture.Server, bucket, prefix string) *storagepb.TransferSource {
	return &storagepb.TransferSource{
		Prefix: prefix,
		Provider: &storagepb.TransferSource_S3Compatible{S3Compatible: &storagepb.TransferSource_S3CompatibleProvider{
			Endpoint: ext.Endpoint, Region: "us-east-1", BucketName: bucket,
			Credentials: &storagepb.TransferSource_S3CompatibleProvider_AccessKey{AccessKey: &storagepb.TransferCredentialsAccessKey{
				AccessKeyId: ext.AccessKey, SecretAccessKey: ext.SecretKey,
			}},
		}},
	}
}

func trxS3Dst(ext *rustfsfixture.Server, bucket, prefix string) *storagepb.TransferDestination {
	return &storagepb.TransferDestination{
		Prefix: prefix,
		Provider: &storagepb.TransferDestination_S3Compatible{S3Compatible: &storagepb.TransferDestination_S3CompatibleProvider{
			Endpoint: ext.Endpoint, Region: "us-east-1", BucketName: bucket,
			Credentials: &storagepb.TransferDestination_S3CompatibleProvider_AccessKey{AccessKey: &storagepb.TransferCredentialsAccessKey{
				AccessKeyId: ext.AccessKey, SecretAccessKey: ext.SecretKey,
			}},
		}},
	}
}

func trxOnce(s *storagepb.TransferSpec) {
	s.StopCondition = &storagepb.TransferSpec_AfterOneIteration{AfterOneIteration: &storagepb.TransferSpec_StopConditionAfterOneIteration{}}
}

func trxInfinite(interval time.Duration) func(*storagepb.TransferSpec) {
	return func(s *storagepb.TransferSpec) {
		s.StopCondition = &storagepb.TransferSpec_Infinite{Infinite: &storagepb.TransferSpec_StopConditionInfinite{}}
		s.InterIterationInterval = durationpb.New(interval)
	}
}

// trxSpec builds a spec (one iteration unless opts say otherwise).
func trxSpec(src *storagepb.TransferSource, dst *storagepb.TransferDestination, ov storagepb.TransferSpec_OverwriteStrategy, opts ...func(*storagepb.TransferSpec)) *storagepb.TransferSpec {
	s := &storagepb.TransferSpec{Source: src, Destination: dst, OverwriteStrategy: ov}
	trxOnce(s)
	for _, o := range opts {
		o(s)
	}
	return s
}

func (e *trxEnv) create(t *testing.T, name string, spec *storagepb.TransferSpec) string {
	t.Helper()
	return wait(t)(e.svc.Create(t.Context(), &storagepb.CreateTransferRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: e.inst.ProjectID, Name: name}, Spec: spec,
	})).ResourceID()
}

func (e *trxEnv) get(t *testing.T, id string) *storagepb.Transfer {
	t.Helper()
	tr, err := e.svc.Get(t.Context(), &storagepb.GetTransferRequest{Id: id})
	if err != nil {
		t.Fatalf("Get transfer %s: %v", id, err)
	}
	return tr
}

// await waits until the transfer is in state and its last iteration has
// sequence number seq and is no longer running. When tick > 0 the mocker clock
// advances by tick every tenth poll until iteration seq has started, so a fast
// runner cannot skip over it.
func (e *trxEnv) await(t *testing.T, id string, state storagepb.TransferStatus_State, seq int64, tick time.Duration) *storagepb.Transfer {
	t.Helper()
	var tr *storagepb.Transfer
	t.Cleanup(func() {
		if t.Failed() && tr != nil {
			t.Logf("last status seen while waiting for %v/%d: %v", state, seq, tr.GetStatus())
		}
	})
	var seen int64
	polls := 0
	poll(t, "transfer "+id+" to be "+state.String(), func() {
		polls++
		if tick > 0 && seen < seq && polls%10 == 1 {
			e.clk.Advance(tick)
		}
	}, func() bool {
		var err error
		tr, err = e.svc.Get(t.Context(), &storagepb.GetTransferRequest{Id: id})
		seen = tr.GetStatus().GetLastIteration().GetSequenceNumber()
		it := tr.GetStatus().GetLastIteration()
		return err == nil && tr.GetStatus().GetState() == state && it.GetSequenceNumber() == seq && it.GetState() != storagepb.TransferIteration_IN_PROGRESS
	})
	return tr
}

func (e *trxEnv) history(t *testing.T, id string) []*storagepb.TransferIteration {
	t.Helper()
	h, err := e.svc.GetIterationHistory(t.Context(), &storagepb.GetIterationHistoryRequest{TransferId: id})
	if err != nil {
		t.Fatalf("GetIterationHistory %s: %v", id, err)
	}
	return h.GetIterations()
}

func (e *trxEnv) stop(t *testing.T, id string) {
	t.Helper()
	wait(t)(e.svc.Stop(t.Context(), &storagepb.StopTransferRequest{Id: id}))
}

func (e *trxEnv) resume(t *testing.T, id string) {
	t.Helper()
	wait(t)(e.svc.Resume(t.Context(), &storagepb.ResumeTransferRequest{Id: id}))
}

// update changes a copy of the transfer's spec (as returned by Get: secrets are
// input-only and come back empty) and sends it through gosdk.
func (e *trxEnv) update(t *testing.T, id string, mutate func(*storagepb.TransferSpec)) {
	t.Helper()
	cur := e.get(t, id)
	spec := proto.CloneOf(cur.GetSpec())
	mutate(spec)
	wait(t)(e.svc.Update(t.Context(), &storagepb.UpdateTransferRequest{Metadata: proto.CloneOf(cur.GetMetadata()), Spec: spec}))
}

func trxSeqs(its []*storagepb.TransferIteration) []int64 {
	out := make([]int64, len(its))
	for i, it := range its {
		out[i] = it.GetSequenceNumber()
	}
	return out
}

// trxPut writes an object through an S3 endpoint with optional attributes.
func trxPut(t *testing.T, c *s3.Client, bucket, key string, data []byte, contentType string, md map[string]string) {
	t.Helper()
	in := &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), Body: bytes.NewReader(data), Metadata: md}
	if contentType != "" {
		in.ContentType = aws.String(contentType)
	}
	if _, err := c.PutObject(t.Context(), in); err != nil {
		t.Fatalf("PutObject %s/%s: %v", bucket, key, err)
	}
}

func trxText(t *testing.T, c *s3.Client, bucket, key, body string) {
	t.Helper()
	trxPut(t, c, bucket, key, []byte(body), "", nil)
}

func trxKeys(t *testing.T, c *s3.Client, bucket, prefix string) []string {
	t.Helper()
	var keys []string
	p := s3.NewListObjectsV2Paginator(c, &s3.ListObjectsV2Input{Bucket: aws.String(bucket), Prefix: aws.String(prefix)})
	for p.HasMorePages() {
		page, err := p.NextPage(t.Context())
		if err != nil {
			t.Fatalf("ListObjectsV2 %s/%s: %v", bucket, prefix, err)
		}
		for _, o := range page.Contents {
			keys = append(keys, aws.ToString(o.Key))
		}
	}
	slices.Sort(keys)
	return keys
}

func trxWantKeys(t *testing.T, c *s3.Client, bucket, prefix string, want ...string) {
	t.Helper()
	slices.Sort(want)
	if got := trxKeys(t, c, bucket, prefix); !slices.Equal(got, want) {
		t.Errorf("keys in %s/%s = %v, want %v", bucket, prefix, got, want)
	}
}

func trxBody(t *testing.T, c *s3.Client, bucket, key string) string {
	t.Helper()
	return string(getObject(t, c, bucket, key))
}

func trxWantBody(t *testing.T, c *s3.Client, bucket, key, want string) {
	t.Helper()
	if got := trxBody(t, c, bucket, key); got != want {
		t.Errorf("%s/%s = %q, want %q", bucket, key, got, want)
	}
}

// trxWantAttrs checks content type and user metadata of an object.
func trxWantAttrs(t *testing.T, c *s3.Client, bucket, key, contentType string, md map[string]string) {
	t.Helper()
	h, err := c.HeadObject(t.Context(), &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		t.Fatalf("HeadObject %s/%s: %v", bucket, key, err)
	}
	if aws.ToString(h.ContentType) != contentType {
		t.Errorf("%s/%s content-type = %q, want %q", bucket, key, aws.ToString(h.ContentType), contentType)
	}
	for k, v := range md {
		if h.Metadata[k] != v {
			t.Errorf("%s/%s metadata[%s] = %q, want %q (all: %v)", bucket, key, k, h.Metadata[k], v, h.Metadata)
		}
	}
}

// trxExtClient is an AWS SDK client for the external S3 provider (second RustFS).
func trxExtClient(ext *rustfsfixture.Server) *s3.Client {
	return s3.New(s3.Options{
		Region: "us-east-1", BaseEndpoint: aws.String(ext.Endpoint), UsePathStyle: true,
		Credentials: credentials.NewStaticCredentialsProvider(ext.AccessKey, ext.SecretKey, ""),
	})
}

func trxExtBucket(t *testing.T, c *s3.Client, tag string) string {
	t.Helper()
	name := unique(tag)
	if _, err := c.CreateBucket(t.Context(), &s3.CreateBucketInput{Bucket: aws.String(name)}); err != nil {
		t.Fatalf("external CreateBucket %s: %v", name, err)
	}
	return name
}

// trxBlob returns n pseudo-random bytes (deterministic) and their SHA-256.
func trxBlob(n int) (data []byte, sum string) {
	rng := rand.New(rand.NewPCG(1, 2))
	b := make([]byte, n)
	for i := 0; i+8 <= n; i += 8 {
		v := rng.Uint64()
		for j := range 8 {
			b[i+j] = byte(v >> (8 * j))
		}
	}
	s := sha256.Sum256(b)
	return b, hex.EncodeToString(s[:])
}

// trxSleep waits real time (S3 Last-Modified comes from the backend's real clock).
func trxSleep(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case <-t.Context().Done():
		t.Fatal("test ended")
	case <-clock.Real{}.After(d):
	}
}

func trxIterErr(t *testing.T, tr *storagepb.Transfer, origin storagepb.TransferError_Origin, codes ...string) {
	t.Helper()
	te := tr.GetStatus().GetError()
	if te.GetOrigin() != origin || !slices.Contains(codes, te.GetCode()) || te.GetMessage() == "" {
		t.Errorf("transfer error = %v, want origin %v, code in %v and a message", te, origin, codes)
	}
	it := tr.GetStatus().GetLastIteration()
	if it.GetState() != storagepb.TransferIteration_FAILED || it.GetError().GetOrigin() != origin || it.GetError().GetCode() != te.GetCode() {
		t.Errorf("last iteration = %v, want FAILED with the same error", it)
	}
	t.Logf("observed error: origin=%v code=%s message=%q", te.GetOrigin(), te.GetCode(), te.GetMessage())
}

// --- 1. nebius -> nebius, one iteration ---------------------------------

func TestTransferNebiusToNebiusOnce(t *testing.T) {
	e := trxStart(t)
	src, dst := e.bucket(t, "trx-src"), e.bucket(t, "trx-dst")

	attrs := map[string]string{"owner": "alice", "team": "core"}
	trxPut(t, e.s3, src, "in/a.txt", []byte("alpha"), "text/plain; charset=utf-8", attrs)
	trxPut(t, e.s3, src, "in/dir/b.json", []byte(`{"b":1}`), "application/json", nil)
	trxText(t, e.s3, src, "outside/c.txt", "not under the source prefix")
	wantCount, wantSize := int64(2), int64(len("alpha")+len(`{"b":1}`))
	var bigSum string
	big := !testing.Short()
	if big {
		var data []byte
		data, bigSum = trxBlob(120 << 20)
		trxPut(t, e.s3, src, "in/big.bin", data, "application/octet-stream", map[string]string{"kind": "big"})
		wantCount++
		wantSize += int64(len(data))
	}

	id := e.create(t, "once", trxSpec(e.nebiusSrc(src, "in/"), e.nebiusDst(dst, "out/"), storagepb.TransferSpec_NEVER))
	tr := e.await(t, id, storagepb.TransferStatus_STOPPED, 1, 0)

	it := tr.GetStatus().GetLastIteration()
	if it.GetState() != storagepb.TransferIteration_COMPLETED || it.GetObjectsTransferredCount() != wantCount || it.GetObjectsTransferredSize() != wantSize ||
		it.GetStartTime() == nil || it.GetEndTime() == nil || it.GetError() != nil {
		t.Errorf("last_iteration = %v, want COMPLETED with %d objects / %d bytes", it, wantCount, wantSize)
	}
	if hist := e.history(t, id); len(hist) != 1 || !proto.Equal(hist[0], it) {
		t.Errorf("history = %v, want exactly the last iteration %v", hist, it)
	}

	want := []string{"out/a.txt", "out/dir/b.json"}
	if big {
		want = append(want, "out/big.bin")
	}
	trxWantKeys(t, e.s3, dst, "", want...)
	trxWantBody(t, e.s3, dst, "out/a.txt", "alpha")
	trxWantBody(t, e.s3, dst, "out/dir/b.json", `{"b":1}`)
	trxWantAttrs(t, e.s3, dst, "out/a.txt", "text/plain; charset=utf-8", attrs)
	trxWantAttrs(t, e.s3, dst, "out/dir/b.json", "application/json", nil)

	if big {
		out, err := e.s3.GetObject(t.Context(), &s3.GetObjectInput{Bucket: aws.String(dst), Key: aws.String("out/big.bin")})
		if err != nil {
			t.Fatalf("GetObject big: %v", err)
		}
		h := sha256.New()
		n, err := io.Copy(h, out.Body)
		_ = out.Body.Close()
		if err != nil || n != 120<<20 || hex.EncodeToString(h.Sum(nil)) != bigSum {
			t.Errorf("destination big.bin: %d bytes, sha256 %s, err %v; want 120 MiB with sha256 %s", n, hex.EncodeToString(h.Sum(nil)), err, bigSum)
		}
		trxWantAttrs(t, e.s3, dst, "out/big.bin", "application/octet-stream", map[string]string{"kind": "big"})
		// 100 MB and larger objects are copied as multipart uploads of 50 MB parts
		// (here 50+50+20 MiB): the destination ETag is that of a multipart object.
		hd, err := e.s3.HeadObject(t.Context(), &s3.HeadObjectInput{Bucket: aws.String(dst), Key: aws.String("out/big.bin")})
		if err != nil {
			t.Fatalf("HeadObject big: %v", err)
		}
		if etag := strings.Trim(aws.ToString(hd.ETag), `"`); !strings.HasSuffix(etag, "-3") {
			t.Errorf("destination big.bin ETag = %q, want a multipart ETag ending in -3", etag)
		}
	}
}

// --- 2. replication ------------------------------------------------------

func TestTransferReplication(t *testing.T) {
	e := trxStart(t)
	src, dst := e.bucket(t, "trx-rsrc"), e.bucket(t, "trx-rdst")
	trxText(t, e.s3, src, "src/a.txt", "a-v1")
	trxText(t, e.s3, src, "src/b.txt", "b-v1")
	trxText(t, e.s3, src, "src/c.txt", "c-v1")

	id := e.create(t, "repl", trxSpec(e.nebiusSrc(src, "src/"), e.nebiusDst(dst, "rep/"), storagepb.TransferSpec_IF_NEWER,
		trxInfinite(time.Minute), func(s *storagepb.TransferSpec) { s.EnableDeletesInDestination = true }))
	tr := e.await(t, id, storagepb.TransferStatus_ACTIVE, 1, 0)
	if it := tr.GetStatus().GetLastIteration(); it.GetState() != storagepb.TransferIteration_COMPLETED || it.GetObjectsTransferredCount() != 3 || it.GetObjectsDeletedCount() != 0 {
		t.Fatalf("iteration 1 = %v, want 3 transferred", it)
	}
	trxWantKeys(t, e.s3, dst, "rep/", "rep/a.txt", "rep/b.txt", "rep/c.txt")
	if tr.GetSpec().GetTouchUnmanaged() || !tr.GetSpec().GetEnableDeletesInDestination() {
		t.Errorf("spec = %v, want enable_deletes_in_destination and no touch_unmanaged", tr.GetSpec())
	}

	// Last-Modified has second resolution: let the source changes be newer.
	trxSleep(t, 2*time.Second)
	trxText(t, e.s3, src, "src/a.txt", "a-v2")
	trxText(t, e.s3, src, "src/d.txt", "d-v1")
	if _, err := e.s3.DeleteObject(t.Context(), &s3.DeleteObjectInput{Bucket: aws.String(src), Key: aws.String("src/c.txt")}); err != nil {
		t.Fatalf("DeleteObject c: %v", err)
	}
	trxText(t, e.s3, dst, "rep/unmanaged.txt", "written by a user")

	tr = e.await(t, id, storagepb.TransferStatus_ACTIVE, 2, time.Minute)
	it := tr.GetStatus().GetLastIteration()
	if it.GetState() != storagepb.TransferIteration_COMPLETED || it.GetObjectsTransferredCount() != 2 || it.GetObjectsDeletedCount() != 1 {
		t.Errorf("iteration 2 = %v, want 2 transferred (a changed, d new) and 1 deleted (c)", it)
	}
	trxWantKeys(t, e.s3, dst, "rep/", "rep/a.txt", "rep/b.txt", "rep/d.txt", "rep/unmanaged.txt")
	trxWantBody(t, e.s3, dst, "rep/a.txt", "a-v2")
	trxWantBody(t, e.s3, dst, "rep/d.txt", "d-v1")
	trxWantBody(t, e.s3, dst, "rep/unmanaged.txt", "written by a user")

	hist := e.history(t, id)
	if got := trxSeqs(hist); !slices.Equal(got, []int64{2, 1}) {
		t.Fatalf("history sequence numbers = %v, want [2 1] (newest first)", got)
	}
	if hist[1].GetObjectsTransferredCount() != 3 || hist[0].GetObjectsDeletedCount() != 1 {
		t.Errorf("history = %v", hist)
	}

	// Stop while waiting between iterations; a source change made meanwhile is
	// picked up by the iteration that Resume starts.
	e.stop(t, id)
	if st := e.get(t, id).GetStatus().GetState(); st != storagepb.TransferStatus_STOPPED {
		t.Fatalf("state after Stop = %v, want STOPPED", st)
	}
	trxText(t, e.s3, src, "src/e.txt", "e-v1")
	e.resume(t, id)
	tr = e.await(t, id, storagepb.TransferStatus_ACTIVE, 3, 0)
	if it := tr.GetStatus().GetLastIteration(); it.GetState() != storagepb.TransferIteration_COMPLETED || it.GetObjectsTransferredCount() != 1 {
		t.Errorf("iteration 3 = %v, want 1 transferred (e)", it)
	}
	trxWantBody(t, e.s3, dst, "rep/e.txt", "e-v1")
	if got := trxSeqs(e.history(t, id)); !slices.Equal(got, []int64{3, 2, 1}) {
		t.Errorf("history after Resume = %v, want [3 2 1]", got)
	}
}

// --- 3. overwrite strategies and touch_unmanaged ---------------------------

func TestTransferOverwriteStrategies(t *testing.T) {
	e := trxStart(t)
	src, dst := e.bucket(t, "trx-osrc"), e.bucket(t, "trx-odst")

	// The destination holds user-written (unmanaged) objects that are OLDER than the source's.
	for _, p := range []string{"never/", "ifnewer/", "touch/"} {
		trxText(t, e.s3, dst, p+"x.txt", "old-dest")
	}
	trxSleep(t, 2*time.Second)
	trxText(t, e.s3, src, "s/x.txt", "new-src")
	trxText(t, e.s3, src, "s/y.txt", "y-src")

	run := func(name, prefix string, ov storagepb.TransferSpec_OverwriteStrategy, opts ...func(*storagepb.TransferSpec)) {
		t.Helper()
		id := e.create(t, name, trxSpec(e.nebiusSrc(src, "s/"), e.nebiusDst(dst, prefix), ov, opts...))
		if it := e.await(t, id, storagepb.TransferStatus_STOPPED, 1, 0).GetStatus().GetLastIteration(); it.GetState() != storagepb.TransferIteration_COMPLETED {
			t.Errorf("%s: iteration = %v", name, it)
		}
	}
	run("never", "never/", storagepb.TransferSpec_NEVER)
	run("ifnewer", "ifnewer/", storagepb.TransferSpec_IF_NEWER)
	run("touch", "touch/", storagepb.TransferSpec_IF_NEWER, func(s *storagepb.TransferSpec) { s.TouchUnmanaged = true })

	// Missing objects are always copied; existing ones follow the strategy.
	for _, p := range []string{"never/", "ifnewer/", "touch/"} {
		trxWantBody(t, e.s3, dst, p+"y.txt", "y-src")
	}
	trxWantBody(t, e.s3, dst, "never/x.txt", "old-dest")   // NEVER: existing objects are kept
	trxWantBody(t, e.s3, dst, "ifnewer/x.txt", "old-dest") // IF_NEWER, but unmanaged and touch_unmanaged=false
	trxWantBody(t, e.s3, dst, "touch/x.txt", "new-src")    // IF_NEWER with touch_unmanaged=true

	// A destination object the transfer itself wrote is managed: IF_NEWER updates it.
	id := e.create(t, "managed", trxSpec(e.nebiusSrc(src, "s/"), e.nebiusDst(dst, "managed/"), storagepb.TransferSpec_IF_NEWER, trxInfinite(time.Minute)))
	e.await(t, id, storagepb.TransferStatus_ACTIVE, 1, 0)
	trxWantBody(t, e.s3, dst, "managed/x.txt", "new-src")
	trxSleep(t, 2*time.Second)
	trxText(t, e.s3, src, "s/x.txt", "newer-src")
	e.await(t, id, storagepb.TransferStatus_ACTIVE, 2, time.Minute)
	trxWantBody(t, e.s3, dst, "managed/x.txt", "newer-src")
}

// --- 4. failures and recovery ---------------------------------------------

func TestTransferSourceCredentialsFailure(t *testing.T) {
	e := trxStart(t)
	src, dst := e.bucket(t, "trx-fsrc"), e.bucket(t, "trx-fdst")
	trxText(t, e.s3, src, "in/a.txt", "alpha")

	// A real access key id with a wrong secret.
	id := e.create(t, "bad-source", trxSpec(trxNebiusSrc(src, "in/", e.keyID, "not-the-secret"), e.nebiusDst(dst, "out/"), storagepb.TransferSpec_NEVER))
	tr := e.await(t, id, storagepb.TransferStatus_FAILED, 1, 0)
	trxIterErr(t, tr, storagepb.TransferError_SOURCE, "AccessDenied", "SignatureDoesNotMatch", "InvalidAccessKeyId")
	trxWantKeys(t, e.s3, dst, "")

	// The documented fix: update the source credentials, then resume.
	e.update(t, id, func(s *storagepb.TransferSpec) {
		s.GetSource().GetNebius().GetAccessKey().SecretAccessKey = e.secret
	})
	e.resume(t, id)
	tr = e.await(t, id, storagepb.TransferStatus_STOPPED, 2, 0)
	if it := tr.GetStatus().GetLastIteration(); it.GetState() != storagepb.TransferIteration_COMPLETED || it.GetObjectsTransferredCount() != 1 || tr.GetStatus().GetError() != nil {
		t.Errorf("after the fix: status = %v, want a COMPLETED second iteration and no error", tr.GetStatus())
	}
	trxWantBody(t, e.s3, dst, "out/a.txt", "alpha")
	if got := e.history(t, id); len(got) != 2 || got[1].GetState() != storagepb.TransferIteration_FAILED || got[1].GetError().GetOrigin() != storagepb.TransferError_SOURCE {
		t.Errorf("history = %v, want [COMPLETED, FAILED(SOURCE)]", got)
	}
}

func TestTransferDestinationCredentialsFailure(t *testing.T) {
	e := trxStart(t)
	src, dst := e.bucket(t, "trx-gsrc"), e.bucket(t, "trx-gdst")
	trxText(t, e.s3, src, "in/a.txt", "alpha")
	viewerID, viewerSecret, _ := e.grantedKey(t, "viewer") // may read, not write

	id := e.create(t, "read-only-dest", trxSpec(e.nebiusSrc(src, "in/"), trxNebiusDst(dst, "out/", viewerID, viewerSecret), storagepb.TransferSpec_NEVER))
	tr := e.await(t, id, storagepb.TransferStatus_FAILED, 1, 0)
	trxIterErr(t, tr, storagepb.TransferError_DESTINATION, "AccessDenied")
	trxWantKeys(t, e.s3, dst, "")

	e.update(t, id, func(s *storagepb.TransferSpec) {
		k := s.GetDestination().GetNebius().GetAccessKey()
		k.AccessKeyId, k.SecretAccessKey = e.keyID, e.secret
	})
	e.resume(t, id)
	tr = e.await(t, id, storagepb.TransferStatus_STOPPED, 2, 0)
	if it := tr.GetStatus().GetLastIteration(); it.GetState() != storagepb.TransferIteration_COMPLETED || it.GetObjectsTransferredCount() != 1 {
		t.Errorf("after the fix: last_iteration = %v", it)
	}
	trxWantBody(t, e.s3, dst, "out/a.txt", "alpha")
}

// --- 5. an external S3 provider ----------------------------------------------

func TestTransferExternalS3(t *testing.T) {
	e := trxStart(t)
	ext := rustfsfixture.StartExternal(t) // the user's other S3 provider, with its own admin credentials
	extS3 := trxExtClient(ext)

	// External S3 -> Nebius (v1 s3_compatible source).
	extSrc := trxExtBucket(t, extS3, "ext-src")
	trxPut(t, extS3, extSrc, "data/x.txt", []byte("from the other cloud"), "text/plain", map[string]string{"origin": "ext"})
	trxPut(t, extS3, extSrc, "data/sub/y.bin", []byte{1, 2, 3, 4}, "application/octet-stream", nil)
	trxText(t, extS3, extSrc, "elsewhere/z.txt", "outside the prefix")
	dst := e.bucket(t, "trx-imp")
	id := e.create(t, "import", trxSpec(trxS3Src(ext, extSrc, "data/"), e.nebiusDst(dst, "imported/"), storagepb.TransferSpec_NEVER))
	tr := e.await(t, id, storagepb.TransferStatus_STOPPED, 1, 0)
	if it := tr.GetStatus().GetLastIteration(); it.GetState() != storagepb.TransferIteration_COMPLETED || it.GetObjectsTransferredCount() != 2 || it.GetObjectsTransferredSize() != int64(len("from the other cloud")+4) {
		t.Errorf("import iteration = %v", it)
	}
	trxWantKeys(t, e.s3, dst, "", "imported/sub/y.bin", "imported/x.txt")
	trxWantBody(t, e.s3, dst, "imported/x.txt", "from the other cloud")
	trxWantAttrs(t, e.s3, dst, "imported/x.txt", "text/plain", map[string]string{"origin": "ext"})
	if got := trxBody(t, e.s3, dst, "imported/sub/y.bin"); got != "\x01\x02\x03\x04" {
		t.Errorf("imported/sub/y.bin = %q", got)
	}
	if k := tr.GetSpec().GetSource().GetS3Compatible().GetAccessKey(); k.GetAccessKeyId() != ext.AccessKey || k.GetSecretAccessKey() != "" {
		t.Errorf("source credentials in Get = %v, want the key id and no secret", k)
	}

	// Nebius -> external S3 (v1 s3_compatible destination).
	up := e.bucket(t, "trx-up")
	trxPut(t, e.s3, up, "up/z.txt", []byte("to the other cloud"), "text/markdown", map[string]string{"made-in": "nebius"})
	extDst := trxExtBucket(t, extS3, "ext-dst")
	id = e.create(t, "export", trxSpec(e.nebiusSrc(up, "up/"), trxS3Dst(ext, extDst, "backup/"), storagepb.TransferSpec_NEVER))
	tr = e.await(t, id, storagepb.TransferStatus_STOPPED, 1, 0)
	if it := tr.GetStatus().GetLastIteration(); it.GetState() != storagepb.TransferIteration_COMPLETED || it.GetObjectsTransferredCount() != 1 {
		t.Errorf("export iteration = %v", it)
	}
	trxWantKeys(t, extS3, extDst, "", "backup/z.txt")
	trxWantBody(t, extS3, extDst, "backup/z.txt", "to the other cloud")
	trxWantAttrs(t, extS3, extDst, "backup/z.txt", "text/markdown", map[string]string{"made-in": "nebius"})
}

// --- 6. Azure Blob Storage source ---------------------------------------------

func TestTransferAzureSource(t *testing.T) {
	e := trxStart(t)
	az := azuritefixture.Start(t)
	t.Cleanup(azuritefixture.Cleanup) // this binary's TestMain only removes the RustFS container

	opts := azuritefixture.BlobOptions{ContentType: "text/plain", Metadata: map[string]string{"owner": "bob"}}
	az.CreateContainer(t, "private-docs")
	az.Upload(t, "private-docs", "docs/a.txt", []byte("azure alpha"), opts)
	az.Upload(t, "private-docs", "docs/deep/b.bin", []byte{9, 8, 7}, azuritefixture.BlobOptions{ContentType: "application/octet-stream"})
	az.Upload(t, "private-docs", "other/c.txt", []byte("outside"), opts)
	az.CreatePublicContainer(t, "public-docs")
	az.Upload(t, "public-docs", "pub/p.txt", []byte("public azure"), opts)
	dst := e.bucket(t, "trx-az")

	azSrc := func(container, prefix string, creds func(*storagepb.TransferSource_AzureBlobStorageProvider)) *storagepb.TransferSource {
		p := &storagepb.TransferSource_AzureBlobStorageProvider{Endpoint: az.Endpoint, ContainerName: container}
		creds(p)
		return &storagepb.TransferSource{Prefix: prefix, Provider: &storagepb.TransferSource_AzureBlobStorage{AzureBlobStorage: p}}
	}
	keyCreds := func(key string) func(*storagepb.TransferSource_AzureBlobStorageProvider) {
		return func(p *storagepb.TransferSource_AzureBlobStorageProvider) {
			p.Credentials = &storagepb.TransferSource_AzureBlobStorageProvider_AzureStorageAccount{AzureStorageAccount: &storagepb.TransferCredentialsAzureStorageAccount{
				AccountName: az.Account, AccessKey: key,
			}}
		}
	}

	// Storage account name + key.
	id := e.create(t, "azure-key", trxSpec(azSrc("private-docs", "docs/", keyCreds(az.Key)), e.nebiusDst(dst, "az/"), storagepb.TransferSpec_NEVER))
	tr := e.await(t, id, storagepb.TransferStatus_STOPPED, 1, 0)
	if it := tr.GetStatus().GetLastIteration(); it.GetState() != storagepb.TransferIteration_COMPLETED || it.GetObjectsTransferredCount() != 2 || it.GetObjectsTransferredSize() != int64(len("azure alpha")+3) {
		t.Errorf("shared-key iteration = %v", it)
	}
	trxWantKeys(t, e.s3, dst, "az/", "az/a.txt", "az/deep/b.bin")
	trxWantBody(t, e.s3, dst, "az/a.txt", "azure alpha")
	trxWantAttrs(t, e.s3, dst, "az/a.txt", "text/plain", map[string]string{"owner": "bob"})
	if got := trxBody(t, e.s3, dst, "az/deep/b.bin"); got != "\x09\x08\x07" {
		t.Errorf("az/deep/b.bin = %q", got)
	}
	if k := tr.GetSpec().GetSource().GetAzureBlobStorage().GetAzureStorageAccount(); k.GetAccountName() != az.Account || k.GetAccessKey() != "" {
		t.Errorf("source credentials in Get = %v, want the account name and no key", k)
	}

	// A public container read anonymously.
	id = e.create(t, "azure-anon", trxSpec(azSrc("public-docs", "pub/", func(p *storagepb.TransferSource_AzureBlobStorageProvider) {
		p.Credentials = &storagepb.TransferSource_AzureBlobStorageProvider_Anonymous{Anonymous: &storagepb.TransferCredentialsAnonymous{}}
	}), e.nebiusDst(dst, "anon/"), storagepb.TransferSpec_NEVER))
	tr = e.await(t, id, storagepb.TransferStatus_STOPPED, 1, 0)
	if it := tr.GetStatus().GetLastIteration(); it.GetState() != storagepb.TransferIteration_COMPLETED || it.GetObjectsTransferredCount() != 1 {
		t.Errorf("anonymous iteration = %v", it)
	}
	trxWantBody(t, e.s3, dst, "anon/p.txt", "public azure")

	// A wrong account key fails at the source.
	id = e.create(t, "azure-bad", trxSpec(azSrc("private-docs", "docs/", keyCreds("d3Jvbmcta2V5")), e.nebiusDst(dst, "bad/"), storagepb.TransferSpec_NEVER))
	tr = e.await(t, id, storagepb.TransferStatus_FAILED, 1, 0)
	if te := tr.GetStatus().GetError(); te.GetOrigin() != storagepb.TransferError_SOURCE || te.GetCode() == "" {
		t.Errorf("wrong Azure key: error = %v, want origin SOURCE with a code", te)
	} else {
		t.Logf("observed error: code=%s message=%q", te.GetCode(), te.GetMessage())
	}
}

// --- 7. v1alpha1 --------------------------------------------------------------

func TestTransferV1Alpha1(t *testing.T) {
	e := trxStart(t)
	ext := rustfsfixture.StartExternal(t)
	extS3 := trxExtClient(ext)
	extSrc := trxExtBucket(t, extS3, "alpha-src")
	trxPut(t, extS3, extSrc, "in/one.txt", []byte("one"), "text/plain", map[string]string{"v": "alpha"})
	trxText(t, extS3, extSrc, "in/two/2.txt", "two")
	trxText(t, extS3, extSrc, "skip/3.txt", "three")
	dst := e.bucket(t, "trx-alpha")

	alpha := storagev1alpha1svc.NewTransferService(e.sdk)
	alphaCreds := func(id, secret string) *storagev1alpha1pb.TransferSpec_BucketCredentials {
		return &storagev1alpha1pb.TransferSpec_BucketCredentials{Credentials: &storagev1alpha1pb.TransferSpec_BucketCredentials_AccessKey{
			AccessKey: &storagev1alpha1pb.TransferSpec_BucketCredentials_CredentialsAccessKey{AccessKeyId: id, SecretAccessKey: secret},
		}}
	}
	op, err := alpha.Create(t.Context(), &storagev1alpha1pb.CreateTransferRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: e.inst.ProjectID, Name: "alpha-once"},
		Spec: &storagev1alpha1pb.TransferSpec{
			Source: &storagev1alpha1pb.TransferSpec_SourceBucket{
				Endpoint: ext.Endpoint, BucketName: extSrc, Region: "us-east-1", Prefix: "in/", Credentials: alphaCreds(ext.AccessKey, ext.SecretKey),
			},
			Destination: &storagev1alpha1pb.TransferSpec_DestinationBucket{BucketName: dst, Prefix: "from-alpha/", Credentials: alphaCreds(e.keyID, e.secret)},
			StopCondition: &storagev1alpha1pb.TransferSpec_AfterOneIteration{
				AfterOneIteration: &storagev1alpha1pb.TransferSpec_StopConditionAfterOneIteration{},
			},
			OverwriteStrategy: storagev1alpha1pb.TransferSpec_ALWAYS,
		},
	})
	if err != nil {
		t.Fatalf("v1alpha1 Create: %v", err)
	}
	done, err := op.Wait(t.Context())
	if err != nil {
		t.Fatalf("v1alpha1 Create operation: %v", err)
	}
	id := done.ResourceID()

	var got *storagev1alpha1pb.Transfer
	poll(t, "the v1alpha1 transfer to stop", nil, func() bool {
		got, err = alpha.Get(t.Context(), &storagev1alpha1pb.GetTransferRequest{Id: id})
		return err == nil && got.GetStatus().GetState() == storagev1alpha1pb.TransferStatus_STOPPED &&
			got.GetStatus().GetLastIteration().GetState() == storagev1alpha1pb.TransferIteration_COMPLETED
	})
	trxWantKeys(t, e.s3, dst, "", "from-alpha/one.txt", "from-alpha/two/2.txt")
	trxWantBody(t, e.s3, dst, "from-alpha/one.txt", "one")
	trxWantBody(t, e.s3, dst, "from-alpha/two/2.txt", "two")
	trxWantAttrs(t, e.s3, dst, "from-alpha/one.txt", "text/plain", map[string]string{"v": "alpha"})
	if it := got.GetStatus().GetLastIteration(); it.GetObjectsDiscoveredCount() != 2 || it.GetObjectsMigratedCount() != 2 || it.GetObjectsMigratedSize() != 6 {
		t.Errorf("v1alpha1 last_iteration = %v, want 2 discovered / 2 migrated / 6 bytes", it)
	}
	hist, err := alpha.GetIterationHistory(t.Context(), &storagev1alpha1pb.GetIterationHistoryRequest{TransferId: id})
	if err != nil || len(hist.GetIterations()) != 1 || hist.GetIterations()[0].GetObjectsMigratedCount() != 2 || hist.GetIterations()[0].GetObjectsMigratedSize() != 6 {
		t.Errorf("v1alpha1 GetIterationHistory = %v, %v; want one iteration with 2 objects migrated (6 bytes)", hist, err)
	}

	// The v1 API shows the same transfer.
	v1 := e.get(t, id)
	if v1.GetMetadata().GetName() != "alpha-once" || v1.GetStatus().GetState() != storagepb.TransferStatus_STOPPED ||
		v1.GetStatus().GetLastIteration().GetObjectsTransferredCount() != 2 ||
		v1.GetSpec().GetSource().GetS3Compatible().GetBucketName() != extSrc || v1.GetSpec().GetDestination().GetNebius().GetBucketName() != dst {
		t.Errorf("v1 Get of the v1alpha1 transfer = %v", v1)
	}
	if h := e.history(t, id); len(h) != 1 || h[0].GetObjectsTransferredCount() != 2 {
		t.Errorf("v1 GetIterationHistory = %v", h)
	}
}

// --- 8. limiters ---------------------------------------------------------------

func TestTransferLimiterSlowsIteration(t *testing.T) {
	e := trxStart(t)
	src, dst := e.bucket(t, "trx-lsrc"), e.bucket(t, "trx-ldst")
	for i := range 5 {
		trxText(t, e.s3, src, "in/"+string(rune('a'+i))+".txt", "object "+string(rune('a'+i)))
	}
	id := e.create(t, "slow", trxSpec(e.nebiusSrc(src, "in/"), e.nebiusDst(dst, "out/"), storagepb.TransferSpec_NEVER,
		func(s *storagepb.TransferSpec) { s.Limiters = &storagepb.TransferSpec_Limiters{RequestsPerSecond: 1} }))

	// The limiter waits on the mocker clock, which does not move by itself.
	trxSleep(t, 2*time.Second)
	tr := e.get(t, id)
	if it := tr.GetStatus().GetLastIteration(); it.GetState() != storagepb.TransferIteration_IN_PROGRESS || it.GetEndTime() != nil {
		t.Fatalf("after 2 s of real time without advancing the mocker clock: last_iteration = %v, want IN_PROGRESS", it)
	}
	if n := len(trxKeys(t, e.s3, dst, "out/")); n >= 5 {
		t.Errorf("%d of 5 objects already copied at 1 request/s without time passing", n)
	}
	if tr.GetSpec().GetLimiters().GetRequestsPerSecond() != 1 {
		t.Errorf("spec.limiters = %v, want requests_per_second 1", tr.GetSpec().GetLimiters())
	}

	advanced := time.Duration(0)
	poll(t, "the limited iteration to complete as the clock advances", func() {
		e.clk.Advance(time.Second)
		advanced += time.Second
	}, func() bool {
		return e.get(t, id).GetStatus().GetState() == storagepb.TransferStatus_STOPPED
	})
	if advanced < 3*time.Second {
		t.Errorf("iteration completed after only %v of mocker time; 5 objects at 1 request/s need several seconds", advanced)
	}
	tr = e.get(t, id)
	if it := tr.GetStatus().GetLastIteration(); it.GetState() != storagepb.TransferIteration_COMPLETED || it.GetObjectsTransferredCount() != 5 {
		t.Errorf("last_iteration = %v, want COMPLETED with 5 objects", it)
	}
	if n := len(trxKeys(t, e.s3, dst, "out/")); n != 5 {
		t.Errorf("destination has %d objects, want 5", n)
	}
}

// TestTransferStopInterruptsIteration stops a transfer whose iteration is held
// back by a limiter: the running iteration ends INTERRUPTED.
func TestTransferStopInterruptsIteration(t *testing.T) {
	e := trxStart(t)
	src, dst := e.bucket(t, "trx-isrc"), e.bucket(t, "trx-idst")
	for i := range 4 {
		trxText(t, e.s3, src, "in/"+string(rune('a'+i))+".txt", "object")
	}
	id := e.create(t, "interrupt", trxSpec(e.nebiusSrc(src, "in/"), e.nebiusDst(dst, "out/"), storagepb.TransferSpec_NEVER, trxInfinite(time.Minute),
		func(s *storagepb.TransferSpec) { s.Limiters = &storagepb.TransferSpec_Limiters{RequestsPerSecond: 1} }))
	poll(t, "the iteration to start", nil, func() bool {
		return e.get(t, id).GetStatus().GetLastIteration().GetState() == storagepb.TransferIteration_IN_PROGRESS
	})
	trxSleep(t, time.Second)
	e.stop(t, id)
	tr := e.await(t, id, storagepb.TransferStatus_STOPPED, 1, 0)
	if it := tr.GetStatus().GetLastIteration(); it.GetState() != storagepb.TransferIteration_INTERRUPTED || it.GetEndTime() == nil {
		t.Errorf("last_iteration after Stop = %v, want INTERRUPTED with an end time", it)
	}
	if h := e.history(t, id); len(h) != 1 || h[0].GetState() != storagepb.TransferIteration_INTERRUPTED {
		t.Errorf("history = %v, want one INTERRUPTED iteration", h)
	}
}
