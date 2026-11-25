package s3

import (
	"bytes"
	"net/http"
	"strconv"
	"strings"

	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"

	"github.com/kfirgollan/NebiusMocker/services/storage/domain"
)

// responseWriter applies the front's response rules to every response,
// whether relayed from the store or produced by the front:
//   - CORS: Access-Control-* headers from the bucket's Nebius CORS rules
//     (any the store sent are dropped), and Vary for requests with an Origin;
//   - storage classes: backend class names are translated to Nebius names in
//     the x-amz-storage-class header and, for listings (rewriteBody), in the
//     XML body.
type responseWriter struct {
	http.ResponseWriter
	cors        *domain.CORSMatch
	vary        bool
	rewriteBody bool
	// fsClass reports objects in the FILESYSTEM class (filesystem buckets);
	// names maps the store's bucket name back to the Nebius one in bodies.
	fsClass bool
	names   *strings.Replacer
	// bulkDelete is the per-key outcome of a DeleteObjects request (nil otherwise).
	bulkDelete *bulkDeleteOutcome

	wroteHeader bool
	status      int
	buf         bytes.Buffer
}

func (w *responseWriter) WriteHeader(code int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	h := w.Header()
	for k := range h {
		if strings.HasPrefix(k, "Access-Control-") {
			delete(h, k)
		}
	}
	if w.vary {
		h.Set("Vary", "Origin, Access-Control-Request-Headers, Access-Control-Request-Method")
	}
	if w.cors != nil {
		setCORSHeaders(h, *w.cors)
	}
	if c := h.Get("X-Amz-Storage-Class"); c != "" {
		h.Set("X-Amz-Storage-Class", domain.BackendToS3Class(c))
		if w.fsClass {
			h.Set("X-Amz-Storage-Class", domain.S3ClassName(storagepb.StorageClass_FILESYSTEM))
		}
	}
	if w.rewriteBody {
		w.status = code
		return
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *responseWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if w.rewriteBody {
		return w.buf.Write(p)
	}
	return w.ResponseWriter.Write(p)
}

// Backend storage-class elements in listing bodies and their Nebius names.
var classElements = strings.NewReplacer(
	"<StorageClass>"+domain.BackendClass(storagepb.StorageClass_ENHANCED_THROUGHPUT)+"</StorageClass>",
	"<StorageClass>"+domain.S3ClassName(storagepb.StorageClass_ENHANCED_THROUGHPUT)+"</StorageClass>",
)

// finish flushes a buffered (rewritten) body.
func (w *responseWriter) finish() {
	if !w.rewriteBody || !w.wroteHeader {
		return
	}
	body := classElements.Replace(w.buf.String())
	if w.names != nil {
		body = w.names.Replace(body)
	}
	out := []byte(body)
	w.ResponseWriter.Header().Set("Content-Length", strconv.Itoa(len(out)))
	w.ResponseWriter.WriteHeader(w.status)
	_, _ = w.ResponseWriter.Write(out) // a gone client needs no handling
}

func setCORSHeaders(h http.Header, m domain.CORSMatch) {
	h.Set("Access-Control-Allow-Origin", m.AllowOrigin)
	h.Set("Access-Control-Allow-Methods", strings.Join(m.AllowMethods, ", "))
	if len(m.AllowHeaders) > 0 {
		h.Set("Access-Control-Allow-Headers", strings.Join(m.AllowHeaders, ", "))
	}
	if len(m.ExposeHeaders) > 0 {
		h.Set("Access-Control-Expose-Headers", strings.Join(m.ExposeHeaders, ", "))
	}
	if m.MaxAgeSeconds != nil {
		h.Set("Access-Control-Max-Age", strconv.Itoa(int(*m.MaxAgeSeconds)))
	}
	if m.AllowCredentials {
		h.Set("Access-Control-Allow-Credentials", "true")
	}
}

// preflight answers a CORS preflight (OPTIONS) request from the bucket's
// CORS rules, without authentication, as S3 does.
func (s *Server) preflight(w http.ResponseWriter, r *http.Request, t target) {
	origin, method := r.Header.Get("Origin"), r.Header.Get("Access-Control-Request-Method")
	fail := func(code string, status int, msg string) { s.fail(w, r, coded(code, status, msg)) }
	switch {
	case t.Bucket == "":
		fail("BadRequest", http.StatusBadRequest, "CORS preflight requests need a bucket")
		return
	case origin == "":
		fail("BadRequest", http.StatusBadRequest, "Insufficient information. Origin request header needed.")
		return
	case method == "":
		fail("BadRequest", http.StatusBadRequest, "Invalid Access-Control-Request-Method: null")
		return
	}
	b, err := s.gate.Bucket(r.Context(), t.Bucket)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Vary", "Origin, Access-Control-Request-Headers, Access-Control-Request-Method")
	cors := b.GetSpec().GetCors()
	if len(cors.GetRules()) == 0 {
		fail("AccessForbidden", http.StatusForbidden, "CORSResponse: CORS is not enabled for this bucket.")
		return
	}
	var headers []string
	for h := range strings.SplitSeq(r.Header.Get("Access-Control-Request-Headers"), ",") {
		if h = strings.TrimSpace(h); h != "" {
			headers = append(headers, h)
		}
	}
	m, ok := domain.MatchCORS(cors, origin, method, headers)
	if !ok {
		fail("AccessForbidden", http.StatusForbidden, "CORSResponse: This CORS request is not allowed. This is usually because the evalution of Origin, request method / Access-Control-Request-Method or Access-Control-Request-Headers are not whitelisted by the resource's CORS spec.")
		return
	}
	setCORSHeaders(w.Header(), m)
	w.WriteHeader(http.StatusOK)
}
