package rustfs

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/sigv4"
)

// ForwardRequest is an S3 request that the caller has already authenticated
// and authorized, to be relayed to RustFS path-style.
type ForwardRequest struct {
	// In is the client's request; its method, query and headers are relayed.
	In     *http.Request
	Bucket string
	Key    string // "" for bucket-level requests
	// Body and ContentLength replace In's (e.g. a decoded aws-chunked body).
	Body          io.Reader
	ContentLength int64
	// PayloadHash is the x-amz-content-sha256 to sign with: the client's
	// declared hash (RustFS re-checks the body) or sigv4.UnsignedPayload.
	PayloadHash string
}

// Forwarder relays S3 requests to RustFS, re-signed with the RustFS
// credentials. The client's own signature is never forwarded.
type Forwarder struct {
	endpoint *url.URL
	creds    aws.Credentials
	region   string
	signer   *v4.Signer
	client   *http.Client
	clk      clock.Clock
	log      *slog.Logger
}

// Forwarder returns a Forwarder to the client's server.
func (c *Client) Forwarder(clk clock.Clock, log *slog.Logger) *Forwarder {
	return &Forwarder{
		endpoint: c.endpoint,
		creds:    aws.Credentials{AccessKeyID: c.cfg.AccessKey, SecretAccessKey: c.cfg.SecretKey},
		region:   c.cfg.Region,
		signer:   v4.NewSigner(func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true }),
		client:   c.cfg.HTTPClient,
		clk:      clk,
		log:      log,
	}
}

// droppedHeaders are client headers that must not reach RustFS: the client's
// auth, hop-by-hop headers, and aws-chunked framing the caller has decoded.
var droppedHeaders = map[string]bool{
	"Authorization": true, "X-Amz-Date": true, "X-Amz-Content-Sha256": true, "X-Amz-Security-Token": true,
	"X-Amz-Decoded-Content-Length": true, "X-Amz-Trailer": true, "Content-Length": true, "Expect": true,
	"Connection": true, "Keep-Alive": true, "Proxy-Authorization": true, "Proxy-Connection": true,
	"Te": true, "Trailer": true, "Transfer-Encoding": true, "Upgrade": true,
}

// presignParams are the query parameters of a presigned URL.
var presignParams = []string{
	"X-Amz-Algorithm", "X-Amz-Credential", "X-Amz-Date", "X-Amz-Expires",
	"X-Amz-SignedHeaders", "X-Amz-Signature", "X-Amz-Security-Token", "X-Amz-Content-Sha256",
}

// Forward relays fr and streams RustFS's response to w.
func (f *Forwarder) Forward(w http.ResponseWriter, fr ForwardRequest) {
	out, err := f.outgoing(fr)
	if err != nil {
		f.fail(fr.In.Context(), w, err)
		return
	}
	resp, err := f.client.Do(out)
	if err != nil {
		f.fail(fr.In.Context(), w, err)
		return
	}
	defer func() { _ = resp.Body.Close() }() // response fully relayed below
	for k, vs := range resp.Header {
		if k == "Connection" || k == "Keep-Alive" || k == "Transfer-Encoding" {
			continue
		}
		w.Header()[k] = vs
	}
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, resp.Body); err != nil {
		f.log.WarnContext(fr.In.Context(), "relay rustfs response", "error", err)
	}
}

func (f *Forwarder) outgoing(fr ForwardRequest) (*http.Request, error) {
	in := fr.In
	path := "/" + fr.Bucket
	if fr.Key != "" {
		path += "/" + fr.Key
	}
	q := in.URL.Query()
	for _, p := range presignParams {
		q.Del(p)
	}
	u := *f.endpoint
	u.Path = strings.TrimRight(u.Path, "/") + path
	u.RawPath = strings.TrimRight(f.endpoint.EscapedPath(), "/") + sigv4.EscapePath(path)
	u.RawQuery = strings.ReplaceAll(q.Encode(), "+", "%20")
	out, err := http.NewRequestWithContext(in.Context(), in.Method, u.String(), fr.Body)
	if err != nil {
		return nil, err
	}
	out.ContentLength = fr.ContentLength
	if fr.ContentLength == 0 {
		out.Body = http.NoBody
	}
	for k, vs := range in.Header {
		if !droppedHeaders[k] {
			out.Header[k] = vs
		}
	}
	if enc := withoutAWSChunked(in.Header.Get("Content-Encoding")); enc != "" {
		out.Header.Set("Content-Encoding", enc)
	} else {
		out.Header.Del("Content-Encoding")
	}
	payload := fr.PayloadHash
	if payload == "" {
		payload = sigv4.UnsignedPayload
	}
	out.Header.Set("X-Amz-Content-Sha256", payload)
	if err := f.signer.SignHTTP(in.Context(), f.creds, out, payload, "s3", f.region, f.clk.Now()); err != nil {
		return nil, err
	}
	return out, nil
}

func withoutAWSChunked(enc string) string {
	var keep []string
	for e := range strings.SplitSeq(enc, ",") {
		if e = strings.TrimSpace(e); e != "" && !strings.EqualFold(e, "aws-chunked") {
			keep = append(keep, e)
		}
	}
	return strings.Join(keep, ",")
}

func (f *Forwarder) fail(ctx context.Context, w http.ResponseWriter, err error) {
	f.log.ErrorContext(ctx, "forward to rustfs", "error", err)
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>ServiceUnavailable</Code><Message>the object storage backend (RustFS) is unavailable</Message></Error>`)
}
