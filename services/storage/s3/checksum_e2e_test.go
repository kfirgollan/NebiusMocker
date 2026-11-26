//go:build e2e_storage

package s3

import (
	"bytes"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore/rustfs"
	"github.com/kfirgollan/NebiusMocker/services/storage/domain"
	"github.com/kfirgollan/NebiusMocker/test/rustfsfixture"
)

func TestMain(m *testing.M) { os.Exit(rustfsfixture.Main(m)) }

// ckEnv is the S3 front relaying to a real RustFS, over plain HTTP and over
// TLS (the AWS SDK sends checksums as headers over HTTP and as aws-chunked
// trailers over HTTPS, as against the real Nebius endpoint).
type ckEnv struct {
	bucket      string
	plain, tls  *awss3.Client
	rustfs      *awss3.Client // direct, with the RustFS credentials
	rustfsStore *rustfs.Client
}

func ckStart(t *testing.T) *ckEnv {
	t.Helper()
	srv := rustfsfixture.Start(t)
	store := srv.Client(t)
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	name := "ck-" + hex.EncodeToString(b)
	if err := store.EnsureBucket(t.Context(), name); err != nil {
		t.Fatal(err)
	}
	gate, err := domain.NewS3Gate(domain.GateDeps{
		Buckets: bucketsFake{items: []*storagepb.Bucket{bucket("storagebucket-ck", name, storagepb.BucketStatus_ACTIVE)}},
		Keys:    keysFake{}, Checker: checkerFake{}, Hierarchy: hierarchyFake{}, Mode: authz.ModeEnforce,
		Members: func() domain.GroupMembers { return membersFake{} },
	})
	if err != nil {
		t.Fatal(err)
	}
	front := NewServer(Config{Domain: "s3.test", Region: "eu-north1"}, gate, &opsFake{},
		store.Forwarder(clock.Real{}, slog.New(slog.DiscardHandler)), clock.Real{}, slog.New(slog.DiscardHandler))
	plain, secure := httptest.NewServer(front), httptest.NewTLSServer(front)
	t.Cleanup(plain.Close)
	t.Cleanup(secure.Close)
	// LoadDefaultConfig's default (and the AWS CLI's): checksums whenever the operation supports them.
	opts := func(url string, hc *http.Client) awss3.Options {
		return awss3.Options{
			Region: "eu-north1", BaseEndpoint: aws.String(url), UsePathStyle: true, HTTPClient: hc,
			Credentials:                credentials.NewStaticCredentialsProvider(testKeyID, testSecret, ""),
			RequestChecksumCalculation: aws.RequestChecksumCalculationWhenSupported,
		}
	}
	tlsClient := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: secure.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs}}}
	return &ckEnv{
		bucket: name,
		plain:  awss3.New(opts(plain.URL, http.DefaultClient)),
		tls:    awss3.New(opts(secure.URL, tlsClient)),
		rustfs: awss3.New(awss3.Options{
			Region: rustfs.DefaultRegion, BaseEndpoint: aws.String(srv.Endpoint), UsePathStyle: true,
			Credentials: credentials.NewStaticCredentialsProvider(srv.AccessKey, srv.SecretKey, ""),
		}),
		rustfsStore: store,
	}
}

// ckSums returns the checksum headers of a HeadObject/GetObject output.
func ckSums(crc32, crc32c, crc64, sha1, sha256 *string) map[types.ChecksumAlgorithm]string {
	out := map[types.ChecksumAlgorithm]string{}
	for alg, v := range map[types.ChecksumAlgorithm]*string{
		types.ChecksumAlgorithmCrc32: crc32, types.ChecksumAlgorithmCrc32c: crc32c, types.ChecksumAlgorithmCrc64nvme: crc64,
		types.ChecksumAlgorithmSha1: sha1, types.ChecksumAlgorithmSha256: sha256,
	} {
		if v != nil {
			out[alg] = *v
		}
	}
	return out
}

// ckCheckStored asserts that HeadObject and GetObject through the front and
// HeadObject on RustFS itself report exactly want for key, and that the
// stored algorithm is the one the inventory reads.
func (e *ckEnv) ckCheckStored(t *testing.T, key string, want map[types.ChecksumAlgorithm]string) {
	t.Helper()
	ctx := t.Context()
	for name, c := range map[string]*awss3.Client{"front": e.plain, "rustfs": e.rustfs} {
		h, err := c.HeadObject(ctx, &awss3.HeadObjectInput{Bucket: aws.String(e.bucket), Key: aws.String(key), ChecksumMode: types.ChecksumModeEnabled})
		if err != nil {
			t.Fatalf("%s HeadObject: %v", name, err)
		}
		if got := ckSums(h.ChecksumCRC32, h.ChecksumCRC32C, h.ChecksumCRC64NVME, h.ChecksumSHA1, h.ChecksumSHA256); !ckEqual(got, want) {
			t.Errorf("%s HeadObject(%s) checksums = %v, want %v", name, key, got, want)
		}
	}
	g, err := e.plain.GetObject(ctx, &awss3.GetObjectInput{Bucket: aws.String(e.bucket), Key: aws.String(key), ChecksumMode: types.ChecksumModeEnabled})
	if err != nil {
		t.Fatalf("front GetObject: %v", err)
	}
	if _, err := io.Copy(io.Discard, g.Body); err != nil { // the SDK validates the body against the checksum
		t.Fatalf("front GetObject body: %v", err)
	}
	_ = g.Body.Close()
	if got := ckSums(g.ChecksumCRC32, g.ChecksumCRC32C, g.ChecksumCRC64NVME, g.ChecksumSHA1, g.ChecksumSHA256); !ckEqual(got, want) {
		t.Errorf("front GetObject(%s) checksums = %v, want %v", key, got, want)
	}
	var algs []string
	for alg := range want {
		algs = append(algs, string(alg))
	}
	got, err := e.rustfsStore.ChecksumAlgorithms(ctx, e.bucket, key, "")
	if err != nil || !equalStrings(got, algs) {
		t.Errorf("ChecksumAlgorithms(%s) = %v, %v; want %v", key, got, err, algs)
	}
}

func ckEqual(a, b map[types.ChecksumAlgorithm]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestChecksumsSurviveTheFront(t *testing.T) {
	e := ckStart(t)
	ctx := t.Context()
	data := []byte("hello checksum world")
	tests := []struct {
		name   string
		client *awss3.Client
		alg    types.ChecksumAlgorithm // "" is the SDK default
		want   map[types.ChecksumAlgorithm]string
	}{
		{"default over http (header)", e.plain, "", map[types.ChecksumAlgorithm]string{types.ChecksumAlgorithmCrc32: "AifpTA=="}},
		{"default over https (trailer)", e.tls, "", map[types.ChecksumAlgorithm]string{types.ChecksumAlgorithmCrc32: "AifpTA=="}},
		{"sha256 over http", e.plain, types.ChecksumAlgorithmSha256, map[types.ChecksumAlgorithm]string{types.ChecksumAlgorithmSha256: "yZW+S8F4/5IzdAnd5MtPUok2IPYeXCIs7+iTkXtfOgA="}},
		{"sha256 over https", e.tls, types.ChecksumAlgorithmSha256, map[types.ChecksumAlgorithm]string{types.ChecksumAlgorithmSha256: "yZW+S8F4/5IzdAnd5MtPUok2IPYeXCIs7+iTkXtfOgA="}},
		{"crc32 over https", e.tls, types.ChecksumAlgorithmCrc32, map[types.ChecksumAlgorithm]string{types.ChecksumAlgorithmCrc32: "AifpTA=="}},
	}
	for i, tt := range tests {
		key := "obj-" + string(rune('a'+i))
		put, err := tt.client.PutObject(ctx, &awss3.PutObjectInput{
			Bucket: aws.String(e.bucket), Key: aws.String(key), Body: bytes.NewReader(data), ChecksumAlgorithm: tt.alg,
		})
		if err != nil {
			t.Fatalf("%s: PutObject: %v", tt.name, err)
		}
		if got := ckSums(put.ChecksumCRC32, put.ChecksumCRC32C, put.ChecksumCRC64NVME, put.ChecksumSHA1, put.ChecksumSHA256); !ckEqual(got, tt.want) {
			t.Errorf("%s: PutObject checksums = %v, want %v", tt.name, got, tt.want)
		}
		t.Run(tt.name, func(t *testing.T) { e.ckCheckStored(t, key, tt.want) })
	}
}

// A checksum that does not match the body is rejected by RustFS (BadDigest),
// whether it came as a header or as a trailer.
func TestChecksumMismatchRejected(t *testing.T) {
	e := ckStart(t)
	_, err := e.plain.PutObject(t.Context(), &awss3.PutObjectInput{
		Bucket: aws.String(e.bucket), Key: aws.String("bad"), Body: bytes.NewReader([]byte("hello checksum world")),
		ChecksumCRC32: aws.String("AAAAAA=="),
	})
	if apiErr, ok := errors.AsType[smithy.APIError](err); !ok || apiErr.ErrorCode() != "BadDigest" {
		t.Fatalf("PutObject with a wrong x-amz-checksum-crc32: %v, want BadDigest", err)
	}
	if _, err := e.rustfs.HeadObject(t.Context(), &awss3.HeadObjectInput{Bucket: aws.String(e.bucket), Key: aws.String("bad")}); err == nil {
		t.Error("object with a wrong checksum was stored")
	}
}

func TestMultipartChecksumSurvivesTheFront(t *testing.T) {
	e := ckStart(t)
	ctx := t.Context()
	for name, c := range map[string]*awss3.Client{"http": e.plain, "https": e.tls} {
		key := "mp-" + name
		cr, err := c.CreateMultipartUpload(ctx, &awss3.CreateMultipartUploadInput{
			Bucket: aws.String(e.bucket), Key: aws.String(key), ChecksumAlgorithm: types.ChecksumAlgorithmCrc32,
		})
		if err != nil {
			t.Fatalf("%s CreateMultipartUpload: %v", name, err)
		}
		if cr.ChecksumAlgorithm != types.ChecksumAlgorithmCrc32 {
			t.Errorf("%s CreateMultipartUpload algorithm = %q, want CRC32", name, cr.ChecksumAlgorithm)
		}
		var parts []types.CompletedPart
		for i, body := range [][]byte{bytes.Repeat([]byte("x"), 5<<20), []byte("tail")} {
			up, err := c.UploadPart(ctx, &awss3.UploadPartInput{
				Bucket: aws.String(e.bucket), Key: aws.String(key), UploadId: cr.UploadId, PartNumber: aws.Int32(int32(i + 1)),
				Body: bytes.NewReader(body), ChecksumAlgorithm: types.ChecksumAlgorithmCrc32,
			})
			if err != nil {
				t.Fatalf("%s UploadPart %d: %v", name, i+1, err)
			}
			if aws.ToString(up.ChecksumCRC32) == "" {
				t.Errorf("%s UploadPart %d returned no x-amz-checksum-crc32", name, i+1)
			}
			parts = append(parts, types.CompletedPart{PartNumber: aws.Int32(int32(i + 1)), ETag: up.ETag, ChecksumCRC32: up.ChecksumCRC32})
		}
		co, err := c.CompleteMultipartUpload(ctx, &awss3.CompleteMultipartUploadInput{
			Bucket: aws.String(e.bucket), Key: aws.String(key), UploadId: cr.UploadId,
			MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
		})
		if err != nil {
			t.Fatalf("%s CompleteMultipartUpload: %v", name, err)
		}
		sum := aws.ToString(co.ChecksumCRC32)
		if sum == "" {
			t.Fatalf("%s CompleteMultipartUpload returned no composite checksum", name)
		}
		t.Run(name, func(t *testing.T) {
			e.ckCheckStored(t, key, map[types.ChecksumAlgorithm]string{types.ChecksumAlgorithmCrc32: sum})
		})
	}
}
