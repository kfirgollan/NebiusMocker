package rustfs_test

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore/rustfs"
	"github.com/kfirgollan/NebiusMocker/pkg/sigv4"
)

const (
	ckAccess = "rustfs-access"
	ckSecret = "rustfs-secret-rustfs-secret-0000"
)

// ckSeen is what the fake RustFS decoded from one forwarded request.
type ckSeen struct {
	header  http.Header
	length  int64
	body    string
	trailer http.Header
	err     error
}

// ckBackend verifies the forwarder's signature and decodes aws-chunked
// bodies the way RustFS does.
func ckBackend(t *testing.T) (*httptest.Server, *ckSeen) {
	t.Helper()
	seen := &ckSeen{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.header, seen.length = r.Header.Clone(), r.ContentLength
		a, err := sigv4.Parse(r)
		if err == nil {
			err = sigv4.Verify(r, a, ckSecret, clock.Real{}.Now())
		}
		var body []byte
		if err == nil && a.Streaming() {
			var cr *sigv4.ChunkedReader
			if cr, err = sigv4.NewChunkedReader(r.Body, a, ckSecret); err == nil {
				if body, err = io.ReadAll(cr); err == nil {
					seen.trailer = cr.Trailer()
				}
			}
		} else if err == nil {
			body, err = io.ReadAll(r.Body)
		}
		seen.body, seen.err = string(body), err
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv, seen
}

func ckForwarder(t *testing.T, endpoint string) *rustfs.Forwarder {
	t.Helper()
	c, err := rustfs.New(rustfs.Config{Endpoint: endpoint, AccessKey: ckAccess, SecretKey: ckSecret})
	if err != nil {
		t.Fatal(err)
	}
	return c.Forwarder(clock.Real{}, slog.New(slog.DiscardHandler))
}

// ckClientRequest is a client's aws-chunked PUT as the S3 front received it.
func ckClientRequest(t *testing.T) *http.Request {
	t.Helper()
	in := httptest.NewRequestWithContext(t.Context(), http.MethodPut, "/b/k", http.NoBody)
	in.Header.Set("Content-Encoding", "aws-chunked,gzip")
	in.Header.Set("X-Amz-Content-Sha256", sigv4.StreamingUnsigned)
	in.Header.Set("X-Amz-Trailer", "x-amz-checksum-crc32")
	in.Header.Set("X-Amz-Decoded-Content-Length", "999")
	in.Header.Set("X-Amz-Sdk-Checksum-Algorithm", "CRC32")
	return in
}

func TestForwardRelaysTrailers(t *testing.T) {
	t.Parallel()
	big := strings.Repeat("0123456789abcdef", 10000) // several 64 KiB chunks
	for name, payload := range map[string]string{"small": "hello world", "multi-chunk": big, "empty": ""} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv, seen := ckBackend(t)
			trailer := func() http.Header { return http.Header{"X-Amz-Checksum-Crc32": {"DUoRhQ=="}} }
			rec := httptest.NewRecorder()
			ckForwarder(t, srv.URL).Forward(rec, rustfs.ForwardRequest{
				In: ckClientRequest(t), Bucket: "b", Key: "k", Body: strings.NewReader(payload),
				ContentLength: int64(len(payload)), PayloadHash: sigv4.UnsignedPayload, Trailer: trailer,
			})
			if rec.Code != http.StatusOK || seen.err != nil {
				t.Fatalf("forward: status %d, backend error %v", rec.Code, seen.err)
			}
			if seen.body != payload {
				t.Errorf("backend body: %d bytes, want %d", len(seen.body), len(payload))
			}
			if got := seen.trailer.Get("X-Amz-Checksum-Crc32"); got != "DUoRhQ==" {
				t.Errorf("backend trailer = %v, want x-amz-checksum-crc32: DUoRhQ==", seen.trailer)
			}
			h := seen.header
			if h.Get("X-Amz-Content-Sha256") != sigv4.StreamingUnsigned || h.Get("X-Amz-Trailer") != "x-amz-checksum-crc32" ||
				h.Get("Content-Encoding") != "aws-chunked,gzip" || h.Get("X-Amz-Sdk-Checksum-Algorithm") != "CRC32" {
				t.Errorf("backend headers = %v", h)
			}
			if want := len(payload); h.Get("X-Amz-Decoded-Content-Length") != strconv.Itoa(want) {
				t.Errorf("x-amz-decoded-content-length = %q, want %d", h.Get("X-Amz-Decoded-Content-Length"), want)
			}
		})
	}
}

// A body that fails before its trailers aborts the relay instead of
// sending a body without them.
func TestForwardTrailerMissingFailsRelay(t *testing.T) {
	t.Parallel()
	srv, _ := ckBackend(t)
	rec := httptest.NewRecorder()
	ckForwarder(t, srv.URL).Forward(rec, rustfs.ForwardRequest{
		In: ckClientRequest(t), Bucket: "b", Key: "k", Body: strings.NewReader("abc"), ContentLength: 3,
		Trailer: func() http.Header { return nil },
	})
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

// Without a Trailer func (or declared trailers) the body is relayed as is.
func TestForwardWithoutTrailerIsPlain(t *testing.T) {
	t.Parallel()
	srv, seen := ckBackend(t)
	in := ckClientRequest(t)
	rec := httptest.NewRecorder()
	ckForwarder(t, srv.URL).Forward(rec, rustfs.ForwardRequest{
		In: in, Bucket: "b", Key: "k", Body: bytes.NewReader([]byte("abc")), ContentLength: 3,
	})
	if rec.Code != http.StatusOK || seen.err != nil || seen.body != "abc" || seen.length != 3 {
		t.Fatalf("plain forward: %d %v %q %d", rec.Code, seen.err, seen.body, seen.length)
	}
	if h := seen.header; h.Get("X-Amz-Trailer") != "" || h.Get("Content-Encoding") != "gzip" || h.Get("X-Amz-Content-Sha256") != sigv4.UnsignedPayload {
		t.Errorf("plain forward headers = %v", h)
	}
}
