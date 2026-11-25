// Package sigv4 verifies AWS Signature Version 4 on incoming S3 requests:
// header ("Authorization: AWS4-HMAC-SHA256 …") and presigned-URL forms, plus
// the aws-chunked streaming payload (STREAMING-* content hashes).
//
// It is a small, well-specified algorithm, so NebiusMocker implements it
// rather than depending on a server-side library. Reference:
// https://docs.aws.amazon.com/AmazonS3/latest/API/sig-v4-authenticating-requests.html
package sigv4

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Algorithm is the only supported signing algorithm.
const Algorithm = "AWS4-HMAC-SHA256"

// Payload hash markers (x-amz-content-sha256 values).
const (
	UnsignedPayload        = "UNSIGNED-PAYLOAD"
	StreamingSigned        = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD"
	StreamingSignedTrailer = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER"
	StreamingUnsigned      = "STREAMING-UNSIGNED-PAYLOAD-TRAILER"
)

const (
	amzDateLayout = "20060102T150405Z"
	dateLayout    = "20060102"
	// MaxSkew is how far a header-signed request's date may be from now.
	MaxSkew = 15 * time.Minute
	// MaxPresignExpiry is the longest presigned-URL lifetime S3 accepts.
	MaxPresignExpiry = 7 * 24 * time.Hour
	emptySHA256      = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

// Errors. They map to the S3 error codes in their names.
var (
	// ErrMissingAuth: the request carries no SigV4 credentials (anonymous).
	ErrMissingAuth = errors.New("sigv4: request is not signed")
	// ErrMalformed: the authorization data cannot be parsed (AuthorizationHeaderMalformed / AuthorizationQueryParametersError).
	ErrMalformed = errors.New("sigv4: malformed authorization")
	// ErrSignatureMismatch: SignatureDoesNotMatch.
	ErrSignatureMismatch = errors.New("sigv4: signature does not match")
	// ErrTimeSkewed: RequestTimeTooSkewed.
	ErrTimeSkewed = errors.New("sigv4: request time too skewed")
	// ErrExpired: a presigned URL has expired (AccessDenied "Request has expired").
	ErrExpired = errors.New("sigv4: presigned request has expired")
	// ErrWrongRegion: the credential scope names a region other than the server's (AuthorizationHeaderMalformed).
	ErrWrongRegion = errors.New("sigv4: credential scope region is wrong")
)

// RegionError is the typed form of ErrWrongRegion.
type RegionError struct {
	// Got is the region in the credential scope; Want is the server's region.
	Got, Want string
	// Presigned selects the X-Amz-Credential wording of the message.
	Presigned bool
}

// Error mimics the AWS message for the header and presigned forms.
func (e *RegionError) Error() string {
	msg := fmt.Sprintf("the region '%s' is wrong; expecting '%s'", e.Got, e.Want)
	if e.Presigned {
		return "Error parsing the X-Amz-Credential parameter; " + msg
	}
	return msg
}

// Unwrap returns ErrWrongRegion.
func (e *RegionError) Unwrap() error { return ErrWrongRegion }

// CheckRegion reports a *RegionError when a's credential scope region is not
// want. It is separate from Verify: the signature itself does not depend on
// the region being the server's.
func CheckRegion(a Auth, want string) error {
	if a.Region == want {
		return nil
	}
	return &RegionError{Got: a.Region, Want: want, Presigned: a.Presigned}
}

// Auth is the parsed authorization of one request.
type Auth struct {
	AccessKeyID   string
	Date          string // credential scope date, yyyymmdd
	Region        string
	Service       string
	SignedHeaders []string
	Signature     string
	// AmzDate is the request timestamp (X-Amz-Date or Date).
	AmzDate time.Time
	// PayloadHash is x-amz-content-sha256 (header form) or UNSIGNED-PAYLOAD (presigned).
	PayloadHash string
	Presigned   bool
	Expires     time.Duration // presigned only
}

// Scope returns the credential scope "date/region/service/aws4_request".
func (a Auth) Scope() string {
	return a.Date + "/" + a.Region + "/" + a.Service + "/aws4_request"
}

// Streaming reports whether the body is aws-chunked encoded.
func (a Auth) Streaming() bool {
	return strings.HasPrefix(a.PayloadHash, "STREAMING-")
}

// Parse extracts the SigV4 authorization of r without verifying it.
func Parse(r *http.Request) (Auth, error) {
	if h := r.Header.Get("Authorization"); h != "" {
		return parseHeader(r, h)
	}
	if r.URL.Query().Get("X-Amz-Algorithm") != "" {
		return parseQuery(r)
	}
	return Auth{}, ErrMissingAuth
}

func parseHeader(r *http.Request, h string) (Auth, error) {
	alg, rest, ok := strings.Cut(h, " ")
	if !ok || alg != Algorithm {
		return Auth{}, fmt.Errorf("%w: unsupported algorithm in Authorization header", ErrMalformed)
	}
	fields := map[string]string{}
	for part := range strings.SplitSeq(rest, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			return Auth{}, fmt.Errorf("%w: bad Authorization component %q", ErrMalformed, part)
		}
		fields[k] = v
	}
	a, err := parseCredential(fields["Credential"])
	if err != nil {
		return Auth{}, err
	}
	a.SignedHeaders = strings.Split(fields["SignedHeaders"], ";")
	a.Signature = fields["Signature"]
	if a.Signature == "" || fields["SignedHeaders"] == "" {
		return Auth{}, fmt.Errorf("%w: missing SignedHeaders or Signature", ErrMalformed)
	}
	if a.AmzDate, err = requestDate(r); err != nil {
		return Auth{}, err
	}
	a.PayloadHash = r.Header.Get("X-Amz-Content-Sha256")
	if a.PayloadHash == "" {
		return Auth{}, fmt.Errorf("%w: missing x-amz-content-sha256", ErrMalformed)
	}
	return a, nil
}

func parseQuery(r *http.Request) (Auth, error) {
	q := r.URL.Query()
	if q.Get("X-Amz-Algorithm") != Algorithm {
		return Auth{}, fmt.Errorf("%w: unsupported X-Amz-Algorithm", ErrMalformed)
	}
	a, err := parseCredential(q.Get("X-Amz-Credential"))
	if err != nil {
		return Auth{}, err
	}
	a.Presigned = true
	a.SignedHeaders = strings.Split(q.Get("X-Amz-SignedHeaders"), ";")
	a.Signature = q.Get("X-Amz-Signature")
	if a.Signature == "" || q.Get("X-Amz-SignedHeaders") == "" {
		return Auth{}, fmt.Errorf("%w: missing X-Amz-SignedHeaders or X-Amz-Signature", ErrMalformed)
	}
	if a.AmzDate, err = time.Parse(amzDateLayout, q.Get("X-Amz-Date")); err != nil {
		return Auth{}, fmt.Errorf("%w: bad X-Amz-Date", ErrMalformed)
	}
	secs, err := strconv.Atoi(q.Get("X-Amz-Expires"))
	if err != nil || secs < 1 || time.Duration(secs)*time.Second > MaxPresignExpiry {
		return Auth{}, fmt.Errorf("%w: X-Amz-Expires must be 1..604800 seconds", ErrMalformed)
	}
	a.Expires = time.Duration(secs) * time.Second
	a.PayloadHash = UnsignedPayload
	if h := q.Get("X-Amz-Content-Sha256"); h != "" {
		a.PayloadHash = h
	}
	return a, nil
}

func parseCredential(c string) (Auth, error) {
	parts := strings.Split(c, "/")
	if len(parts) != 5 || parts[4] != "aws4_request" || parts[0] == "" {
		return Auth{}, fmt.Errorf("%w: credential must be <key>/<date>/<region>/<service>/aws4_request", ErrMalformed)
	}
	return Auth{AccessKeyID: parts[0], Date: parts[1], Region: parts[2], Service: parts[3]}, nil
}

func requestDate(r *http.Request) (time.Time, error) {
	if v := r.Header.Get("X-Amz-Date"); v != "" {
		t, err := time.Parse(amzDateLayout, v)
		if err != nil {
			return time.Time{}, fmt.Errorf("%w: bad X-Amz-Date", ErrMalformed)
		}
		return t, nil
	}
	t, err := http.ParseTime(r.Header.Get("Date"))
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: missing X-Amz-Date", ErrMalformed)
	}
	return t.UTC(), nil
}

// Verify checks a's signature over r with secret, and the request time against now.
func Verify(r *http.Request, a Auth, secret string, now time.Time) error {
	if a.AmzDate.Format(dateLayout) != a.Date {
		return fmt.Errorf("%w: credential date %s does not match request date", ErrMalformed, a.Date)
	}
	if err := checkTime(a, now); err != nil {
		return err
	}
	canonical, err := canonicalRequest(r, a)
	if err != nil {
		return err
	}
	want := Sign(SigningKey(secret, a.Date, a.Region, a.Service), stringToSign(a, canonical))
	if !hmac.Equal([]byte(want), []byte(a.Signature)) {
		return ErrSignatureMismatch
	}
	return nil
}

func checkTime(a Auth, now time.Time) error {
	if a.Presigned {
		if now.Before(a.AmzDate.Add(-MaxSkew)) {
			return ErrTimeSkewed
		}
		if now.After(a.AmzDate.Add(a.Expires)) {
			return ErrExpired
		}
		return nil
	}
	if d := now.Sub(a.AmzDate); d > MaxSkew || d < -MaxSkew {
		return ErrTimeSkewed
	}
	return nil
}

func stringToSign(a Auth, canonical string) string {
	sum := sha256.Sum256([]byte(canonical))
	return Algorithm + "\n" + a.AmzDate.Format(amzDateLayout) + "\n" + a.Scope() + "\n" + hex.EncodeToString(sum[:])
}

// SigningKey derives the SigV4 signing key.
func SigningKey(secret, date, region, service string) []byte {
	k := hmacSHA256([]byte("AWS4"+secret), date)
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, service)
	return hmacSHA256(k, "aws4_request")
}

// Sign returns the hex HMAC-SHA256 of stringToSign under key.
func Sign(key []byte, stringToSign string) string {
	return hex.EncodeToString(hmacSHA256(key, stringToSign))
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

func canonicalRequest(r *http.Request, a Auth) (string, error) {
	headers, err := canonicalHeaders(r, a.SignedHeaders)
	if err != nil {
		return "", err
	}
	return strings.Join([]string{
		r.Method,
		canonicalURI(r),
		canonicalQuery(r.URL.RawQuery, skipSignature(a)),
		headers,
		strings.Join(a.SignedHeaders, ";"),
		a.PayloadHash,
	}, "\n"), nil
}

// canonicalURI is the path exactly as the client sent it (S3 does not
// normalize or double-encode).
func canonicalURI(r *http.Request) string {
	p := r.URL.EscapedPath()
	if r.RequestURI != "" && strings.HasPrefix(r.RequestURI, "/") {
		p, _, _ = strings.Cut(r.RequestURI, "?")
	}
	if p == "" {
		return "/"
	}
	return p
}

// skipSignature is the query parameter excluded from the canonical query:
// a presigned URL's own signature.
func skipSignature(a Auth) string {
	if a.Presigned {
		return "X-Amz-Signature"
	}
	return ""
}

func canonicalQuery(raw, skip string) string {
	type kv struct{ k, v string }
	var pairs []kv
	for part := range strings.SplitSeq(raw, "&") {
		if part == "" {
			continue
		}
		k, v, _ := strings.Cut(part, "=")
		k, _ = url.QueryUnescape(k)
		v, _ = url.QueryUnescape(v)
		if skip != "" && k == skip {
			continue
		}
		pairs = append(pairs, kv{Escape(k), Escape(v)})
	}
	slices.SortFunc(pairs, func(a, b kv) int {
		if c := strings.Compare(a.k, b.k); c != 0 {
			return c
		}
		return strings.Compare(a.v, b.v)
	})
	out := make([]string, len(pairs))
	for i, p := range pairs {
		out[i] = p.k + "=" + p.v
	}
	return strings.Join(out, "&")
}

func canonicalHeaders(r *http.Request, signed []string) (string, error) {
	var b strings.Builder
	for _, name := range signed {
		if name != strings.ToLower(name) || name == "" {
			return "", fmt.Errorf("%w: signed header names must be lower-case", ErrMalformed)
		}
		var values []string
		switch name {
		case "host":
			values = []string{r.Host}
		case "content-length":
			values = r.Header.Values("Content-Length")
			if len(values) == 0 && r.ContentLength >= 0 {
				values = []string{strconv.FormatInt(r.ContentLength, 10)}
			}
		case "transfer-encoding":
			values = r.TransferEncoding
		default:
			values = r.Header.Values(name)
		}
		for i, v := range values {
			values[i] = strings.Join(strings.Fields(v), " ")
		}
		b.WriteString(name + ":" + strings.Join(values, ",") + "\n")
	}
	return b.String(), nil
}

// Escape URI-encodes s the SigV4 way: everything except A-Z a-z 0-9 - _ . ~.
func Escape(s string) string {
	return escape(s, "")
}

// EscapePath is Escape that keeps "/" (for object keys in paths).
func EscapePath(s string) string {
	return escape(s, "/")
}

// escape percent-encodes s, keeping unreserved characters and those in keep.
func escape(s, keep string) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	for i := range len(s) {
		c := s[i]
		if isUnreserved(c) || strings.IndexByte(keep, c) >= 0 {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hexDigits[c>>4])
		b.WriteByte(hexDigits[c&15])
	}
	return b.String()
}

func isUnreserved(c byte) bool {
	return 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '-' || c == '_' || c == '.' || c == '~'
}
