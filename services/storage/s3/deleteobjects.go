package s3

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // G501: Content-MD5 is the S3 wire format of DeleteObjects, not a security hash
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/kfirgollan/NebiusMocker/pkg/objstore/rustfs"
	"github.com/kfirgollan/NebiusMocker/services/storage/domain"
)

// bulkDeleteOutcome is the per-key result of a DeleteObjects request (for
// the request's audit event): keys deleted, and keys that failed with their
// S3 error codes.
type bulkDeleteOutcome struct {
	Deleted []string
	Errors  map[string]string
}

// deleteRequest is the DeleteObjects body.
type deleteRequest struct {
	XMLName xml.Name       `xml:"Delete"`
	Quiet   bool           `xml:"Quiet,omitempty"`
	Objects []deleteObject `xml:"Object"`
}

type deleteObject struct {
	Key       string `xml:"Key"`
	VersionID string `xml:"VersionId,omitempty"`
}

// deleteError is one <Error> entry of a DeleteResult.
type deleteError struct {
	XMLName   xml.Name `xml:"Error"`
	Key       string   `xml:"Key"`
	VersionID string   `xml:"VersionId,omitempty"`
	Code      string   `xml:"Code"`
	Message   string   `xml:"Message"`
}

type deleteResult struct {
	XMLName xml.Name      `xml:"DeleteResult"`
	Xmlns   string        `xml:"xmlns,attr,omitempty"`
	Errors  []deleteError `xml:"Error"`
}

// maxDeleteKeys is S3's limit per DeleteObjects request.
const maxDeleteKeys = 1000

// deleteObjects serves bulk DeleteObjects. Its keys are in the body, so each
// key is authorized on its own (IAM, or a bucket policy rule whose path
// matches the key). Only allowed keys are relayed to the store; denied keys
// are reported as AccessDenied entries of the DeleteResult, as S3 reports
// per-object failures.
func (s *Server) deleteObjects(ctx context.Context, w *responseWriter, r *http.Request, t target, principal string, bd body) error {
	raw, err := readBody(bd)
	if err != nil {
		return err
	}
	var req deleteRequest
	if err := xml.Unmarshal(raw, &req); err != nil || len(req.Objects) == 0 { //nolint:gosec // G709: size-bounded (1 MiB) document into plain string fields; encoding/xml expands no entities
		return coded("MalformedXML", http.StatusBadRequest, "The XML you provided was not well-formed or did not validate against our published schema")
	}
	if len(req.Objects) > maxDeleteKeys {
		return coded("MalformedXML", http.StatusBadRequest, "DeleteObjects accepts at most 1000 keys")
	}
	keys := make([]string, len(req.Objects))
	for i, o := range req.Objects {
		keys[i] = o.Key
	}
	acc, allowed, err := s.gate.AuthorizeObjects(ctx, principal, t.Bucket, domain.ActDeleteObject, keys)
	if err != nil {
		return err
	}
	s.auditLog(ctx, principal, acc)
	if t.store, err = s.buckets.Backend(ctx, acc.Bucket); err != nil {
		return err
	}
	out := &bulkDeleteOutcome{Errors: map[string]string{}}
	w.bulkDelete = out
	var permitted []deleteObject
	var denied []deleteError
	for i, o := range req.Objects {
		if allowed[i] {
			permitted = append(permitted, o)
			continue
		}
		denied = append(denied, deleteError{Key: o.Key, VersionID: o.VersionID, Code: "AccessDenied", Message: "Access Denied"})
		out.Errors[o.Key] = "AccessDenied"
	}
	if len(permitted) == 0 {
		writeXML(w, http.StatusOK, deleteResult{Xmlns: s3NS, Errors: denied})
		return nil
	}
	return s.relayDelete(w, r, t, deleteRequest{Quiet: req.Quiet, Objects: permitted}, denied, out)
}

// relayDelete sends the allowed keys to the store and adds the denied keys'
// errors to its DeleteResult.
func (s *Server) relayDelete(w *responseWriter, r *http.Request, t target, req deleteRequest, denied []deleteError, out *bulkDeleteOutcome) error {
	body, err := xml.Marshal(req)
	if err != nil {
		return err
	}
	sum := md5.Sum(body) //nolint:gosec // G401: Content-MD5 wire format
	sha := sha256.Sum256(body)
	r.Header.Set("Content-Md5", base64.StdEncoding.EncodeToString(sum[:]))
	for k := range r.Header {
		if strings.HasPrefix(k, "X-Amz-Checksum-") || k == "X-Amz-Sdk-Checksum-Algorithm" {
			r.Header.Del(k) // checksums of the client's body, not of the filtered one
		}
	}
	rec := &recorder{header: http.Header{}}
	s.upstream.Forward(rec, rustfs.ForwardRequest{
		In: r, Bucket: t.storeBucket(), Body: bytes.NewReader(body), ContentLength: int64(len(body)), PayloadHash: hex.EncodeToString(sha[:]),
	})
	result := rec.body.Bytes()
	if rec.status == http.StatusOK {
		var stored deleteResult
		if err := xml.Unmarshal(result, &stored); err == nil {
			for _, e := range stored.Errors {
				out.Errors[e.Key] = e.Code
			}
		}
		for _, o := range req.Objects {
			if _, failed := out.Errors[o.Key]; !failed {
				out.Deleted = append(out.Deleted, o.Key)
			}
		}
		result = withErrors(result, denied)
	}
	for k, vs := range rec.header {
		w.Header()[k] = vs
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(result)))
	w.WriteHeader(rec.statusOrOK())
	_, _ = w.Write(result) // a gone client needs no handling
	return nil
}

// withErrors inserts <Error> entries before </DeleteResult>.
func withErrors(result []byte, errs []deleteError) []byte {
	if len(errs) == 0 {
		return result
	}
	var extra bytes.Buffer
	for _, e := range errs {
		b, err := xml.Marshal(e)
		if err == nil {
			extra.Write(b)
		}
	}
	end := bytes.LastIndex(result, []byte("</DeleteResult>"))
	if end < 0 {
		return result
	}
	out := make([]byte, 0, len(result)+extra.Len())
	out = append(out, result[:end]...)
	out = append(out, extra.Bytes()...)
	return append(out, result[end:]...)
}

// recorder buffers a relayed response.
type recorder struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (r *recorder) Header() http.Header { return r.header }

func (r *recorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
}

func (r *recorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.body.Write(p)
}

func (r *recorder) statusOrOK() int {
	if r.status == 0 {
		return http.StatusOK
	}
	return r.status
}

var _ io.Writer = (*recorder)(nil)
