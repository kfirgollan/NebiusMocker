package sigv4_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"

	"github.com/kfirgollan/NebiusMocker/pkg/sigv4"
)

const (
	keyID  = "NMKTESTKEY0000000001"
	secret = "s3cr3t-s3cr3t-s3cr3t-s3cr3t-s3cr3t-0000"
	region = "eu-north1"
)

var signedAt = time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

// verifier is a server that verifies every request with secret at time now
// and replies with the decoded body (or the error text and 403).
func verifier(t *testing.T, now time.Time) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a, err := sigv4.Parse(r)
		if err == nil {
			err = sigv4.Verify(r, a, secret, now)
		}
		var body []byte
		if err == nil && a.Streaming() {
			var cr io.Reader
			if cr, err = sigv4.NewChunkedReader(r.Body, a, secret); err == nil {
				body, err = io.ReadAll(cr)
			}
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func sdkSign(t *testing.T, req *http.Request, payloadHash, withSecret string) {
	t.Helper()
	s := v4.NewSigner(func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true })
	if err := s.SignHTTP(t.Context(), aws.Credentials{AccessKeyID: keyID, SecretAccessKey: withSecret}, req, payloadHash, "s3", region, signedAt); err != nil {
		t.Fatalf("SignHTTP: %v", err)
	}
}

func hashOf(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func do(t *testing.T, req *http.Request) (code int, body string) {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }() // test client
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestHeaderSignatures(t *testing.T) {
	t.Parallel()
	srv := verifier(t, signedAt.Add(time.Minute))
	body := []byte("hello object")
	tests := []struct {
		name     string
		method   string
		path     string // already escaped, as an S3 client sends it
		query    string
		body     []byte
		hash     string
		secret   string
		tamper   func(*http.Request)
		wantCode int
		wantErr  string
	}{
		{name: "get object", method: "GET", path: "/bucket/dir/a.txt", hash: hashOf(nil), secret: secret, wantCode: 200},
		{name: "odd key and query", method: "GET", path: "/bucket/a%20b%2Bc%21%28%29.txt", query: "list-type=2&prefix=a%2Fb&delimiter=%2F&x=", hash: hashOf(nil), secret: secret, wantCode: 200},
		{name: "put with payload hash", method: "PUT", path: "/bucket/k", body: body, hash: hashOf(body), secret: secret, wantCode: 200},
		{name: "unsigned payload", method: "PUT", path: "/bucket/k", body: body, hash: sigv4.UnsignedPayload, secret: secret, wantCode: 200},
		{name: "wrong secret", method: "GET", path: "/bucket/k", hash: hashOf(nil), secret: "other", wantCode: 403, wantErr: "signature does not match"},
		{name: "tampered header", method: "GET", path: "/bucket/k", hash: hashOf(nil), secret: secret, tamper: func(r *http.Request) { r.Header.Set("X-Amz-Date", "20261125T120001Z") }, wantCode: 403, wantErr: "signature does not match"},
		{name: "tampered path", method: "GET", path: "/bucket/k", hash: hashOf(nil), secret: secret, tamper: func(r *http.Request) { r.URL.Path = "/bucket/other" }, wantCode: 403, wantErr: "signature does not match"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			u := srv.URL + tt.path
			if tt.query != "" {
				u += "?" + tt.query
			}
			req, err := http.NewRequestWithContext(t.Context(), tt.method, u, bytes.NewReader(tt.body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("X-Amz-Content-Sha256", tt.hash)
			sdkSign(t, req, tt.hash, tt.secret)
			if tt.tamper != nil {
				tt.tamper(req)
			}
			code, msg := do(t, req)
			if code != tt.wantCode || !strings.Contains(msg, tt.wantErr) {
				t.Fatalf("status %d %q, want %d containing %q", code, msg, tt.wantCode, tt.wantErr)
			}
		})
	}
}

func TestTimeSkew(t *testing.T) {
	t.Parallel()
	srv := verifier(t, signedAt.Add(20*time.Minute))
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/b/k", http.NoBody)
	req.Header.Set("X-Amz-Content-Sha256", hashOf(nil))
	sdkSign(t, req, hashOf(nil), secret)
	if code, msg := do(t, req); code != 403 || !strings.Contains(msg, "skewed") {
		t.Fatalf("status %d %q, want 403 skewed", code, msg)
	}
}

func TestPresigned(t *testing.T) {
	t.Parallel()
	presign := func(t *testing.T, srvURL string) string {
		t.Helper()
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, srvURL+"/bucket/a%20b.txt?X-Amz-Expires=600", http.NoBody)
		s := v4.NewSigner(func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true })
		u, _, err := s.PresignHTTP(t.Context(), aws.Credentials{AccessKeyID: keyID, SecretAccessKey: secret}, req, sigv4.UnsignedPayload, "s3", region, signedAt)
		if err != nil {
			t.Fatalf("PresignHTTP: %v", err)
		}
		return u
	}
	valid := verifier(t, signedAt.Add(5*time.Minute))
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, presign(t, valid.URL), http.NoBody)
	if code, msg := do(t, req); code != 200 {
		t.Fatalf("valid presigned URL: %d %q", code, msg)
	}
	expired := verifier(t, signedAt.Add(11*time.Minute))
	req, _ = http.NewRequestWithContext(t.Context(), http.MethodGet, presign(t, expired.URL), http.NoBody)
	if code, msg := do(t, req); code != 403 || !strings.Contains(msg, "expired") {
		t.Fatalf("expired presigned URL: %d %q, want 403 expired", code, msg)
	}
}

func TestParseErrors(t *testing.T) {
	t.Parallel()
	for name, h := range map[string]string{
		"algorithm":  "AWS4-HMAC-SHA1 Credential=a/20261125/r/s3/aws4_request, SignedHeaders=host, Signature=x",
		"credential": "AWS4-HMAC-SHA256 Credential=a/20261125/s3, SignedHeaders=host, Signature=x",
		"signature":  "AWS4-HMAC-SHA256 Credential=a/20261125/r/s3/aws4_request, SignedHeaders=host",
	} {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/b/k", http.NoBody)
		r.Header.Set("Authorization", h)
		r.Header.Set("X-Amz-Date", "20261125T120000Z")
		r.Header.Set("X-Amz-Content-Sha256", sigv4.UnsignedPayload)
		if _, err := sigv4.Parse(r); !errors.Is(err, sigv4.ErrMalformed) {
			t.Errorf("%s: Parse err = %v, want ErrMalformed", name, err)
		}
	}
	if _, err := sigv4.Parse(httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/b/k", http.NoBody)); !errors.Is(err, sigv4.ErrMissingAuth) {
		t.Errorf("anonymous: err = %v, want ErrMissingAuth", err)
	}
}

// encodeChunks builds a signed aws-chunked body chained from seed.
func encodeChunks(t *testing.T, seed string, chunks [][]byte, sigFor func(prev string, data []byte) string) []byte {
	t.Helper()
	var b bytes.Buffer
	prev := seed
	for _, c := range append(chunks, nil) {
		sig := sigFor(prev, c)
		fmt.Fprintf(&b, "%x;chunk-signature=%s\r\n", len(c), sig)
		b.Write(c)
		b.WriteString("\r\n")
		prev = sig
	}
	return b.Bytes()
}

func TestStreamingSigned(t *testing.T) {
	t.Parallel()
	srv := verifier(t, signedAt)
	chunks := [][]byte{bytes.Repeat([]byte("a"), 70), []byte("tail")}
	decoded := 0
	for _, c := range chunks {
		decoded += len(c)
	}
	key := sigv4.SigningKey(secret, "20261125", region, "s3")
	scope := "20261125/" + region + "/s3/aws4_request"
	sigFor := func(prev string, data []byte) string {
		return sigv4.Sign(key, "AWS4-HMAC-SHA256-PAYLOAD\n20261125T120000Z\n"+scope+"\n"+prev+"\n"+hashOf(nil)+"\n"+hashOf(data))
	}
	send := func(t *testing.T, corrupt bool) (int, string) {
		t.Helper()
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodPut, srv.URL+"/bucket/big", http.NoBody)
		req.Header.Set("X-Amz-Content-Sha256", sigv4.StreamingSigned)
		req.Header.Set("X-Amz-Decoded-Content-Length", strconv.Itoa(decoded))
		req.Header.Set("Content-Encoding", "aws-chunked")
		sdkSign(t, req, sigv4.StreamingSigned, secret)
		seed := req.Header.Get("Authorization")
		seed = seed[strings.LastIndex(seed, "Signature=")+len("Signature="):]
		body := encodeChunks(t, seed, chunks, sigFor)
		if corrupt {
			body[len(body)/2] ^= 1
		}
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.ContentLength = int64(len(body))
		return do(t, req)
	}
	if code, got := send(t, false); code != 200 || got != strings.Repeat("a", 70)+"tail" {
		t.Fatalf("signed stream: %d %q", code, got)
	}
	if code, _ := send(t, true); code != 403 {
		t.Fatalf("corrupted signed stream: status %d, want 403", code)
	}
}

func TestStreamingUnsignedTrailer(t *testing.T) {
	t.Parallel()
	srv := verifier(t, signedAt)
	body := "5\r\nhello\r\n6\r\n world\r\n0\r\nx-amz-checksum-crc32:AAAAAA==\r\n\r\n"
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPut, srv.URL+"/bucket/k", strings.NewReader(body))
	req.Header.Set("X-Amz-Content-Sha256", sigv4.StreamingUnsigned)
	req.Header.Set("X-Amz-Trailer", "x-amz-checksum-crc32")
	sdkSign(t, req, sigv4.StreamingUnsigned, secret)
	if code, got := do(t, req); code != 200 || got != "hello world" {
		t.Fatalf("unsigned trailer stream: %d %q", code, got)
	}
}

func TestEscape(t *testing.T) {
	t.Parallel()
	if got, want := sigv4.EscapePath("dir/a b+c~_.-é"), "dir/a%20b%2Bc~_.-%C3%A9"; got != want {
		t.Errorf("EscapePath = %q, want %q", got, want)
	}
	if got, want := sigv4.Escape("a/b"), "a%2Fb"; got != want {
		t.Errorf("Escape = %q, want %q", got, want)
	}
}

func TestCheckRegion(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		auth    sigv4.Auth
		wantErr string // empty: no error
	}{
		{"match", sigv4.Auth{Region: "eu-north1"}, ""},
		{"header mismatch", sigv4.Auth{Region: "us-east-1"}, "the region 'us-east-1' is wrong; expecting 'eu-north1'"},
		{"presigned mismatch", sigv4.Auth{Region: "us-east-1", Presigned: true}, "Error parsing the X-Amz-Credential parameter; the region 'us-east-1' is wrong; expecting 'eu-north1'"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := sigv4.CheckRegion(tt.auth, "eu-north1")
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("CheckRegion = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, sigv4.ErrWrongRegion) {
				t.Fatalf("CheckRegion = %v, want ErrWrongRegion", err)
			}
			re, ok := errors.AsType[*sigv4.RegionError](err)
			if !ok || re.Got != "us-east-1" || re.Want != "eu-north1" || re.Presigned != tt.auth.Presigned {
				t.Fatalf("RegionError = %+v, ok=%v", re, ok)
			}
			if err.Error() != tt.wantErr {
				t.Fatalf("message = %q, want %q", err.Error(), tt.wantErr)
			}
		})
	}
}

// TestRegionIsSeparateFromSignature: a request signed for another region still
// verifies; only CheckRegion rejects it.
func TestRegionIsSeparateFromSignature(t *testing.T) {
	t.Parallel()
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://s3.example.test/bucket/key", http.NoBody)
	req.Header.Set("X-Amz-Content-Sha256", sigv4.UnsignedPayload)
	s := v4.NewSigner(func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true })
	if err := s.SignHTTP(t.Context(), aws.Credentials{AccessKeyID: keyID, SecretAccessKey: secret}, req, sigv4.UnsignedPayload, "s3", "us-east-1", signedAt); err != nil {
		t.Fatalf("SignHTTP: %v", err)
	}
	a, err := sigv4.Parse(req)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if a.Region != "us-east-1" {
		t.Fatalf("Region = %q, want us-east-1", a.Region)
	}
	if err := sigv4.Verify(req, a, secret, signedAt); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if err := sigv4.CheckRegion(a, region); !errors.Is(err, sigv4.ErrWrongRegion) {
		t.Fatalf("CheckRegion = %v, want ErrWrongRegion", err)
	}
}
