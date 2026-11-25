// Package s3 is the S3 data-plane front of Object Storage (transport).
//
// It authenticates every request with a Nebius access key (SigV4 against
// iam.v2 AccessKey secrets, credential scope in the bucket region) or treats
// it as anonymous, authorizes it against Nebius buckets, IAM permits and
// bucket policies (domain.S3Gate), and applies the bucket's Nebius settings:
// CORS (preflight and response headers), max_size_bytes, default/forced
// storage classes. Object traffic is relayed to the object store (RustFS)
// re-signed with the store's own credentials; bucket management and bucket
// configuration (versioning, lifecycle, CORS) are answered from and written
// to the Nebius bucket records.
package s3

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore/rustfs"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/pkg/reqctx"
	"github.com/kfirgollan/NebiusMocker/pkg/sigv4"
	"github.com/kfirgollan/NebiusMocker/services/storage/domain"
)

// Upstream relays an authorized request to the object store.
type Upstream interface {
	Forward(w http.ResponseWriter, fr rustfs.ForwardRequest)
}

// NoDataPlane is the Upstream of the in-memory object store: requests are
// authenticated and authorized, then answered with NotImplemented.
type NoDataPlane struct{}

// Forward implements Upstream.
func (NoDataPlane) Forward(w http.ResponseWriter, _ rustfs.ForwardRequest) {
	e := toS3Error(notImplemented("the S3 data plane needs the RustFS object store (NEBIUS_MOCKER_OBJSTORE=rustfs)"))
	writeXML(w, e.status, e)
}

// BucketOps are the bucket operations the front performs (satisfied by
// *domain.BucketService).
type BucketOps interface {
	CreateFromS3(ctx context.Context, projectID, name string) (*storagepb.Bucket, error)
	DeleteFromS3(ctx context.Context, id string) error
	UpdateSpecFromS3(ctx context.Context, id string, mutate func(*storagepb.BucketSpec)) error
	CheckCapacity(ctx context.Context, b *storagepb.Bucket, incoming int64) error
	// ObjectSize takes the object-store bucket (Backend), not the Nebius name.
	ObjectSize(ctx context.Context, bucket, key, versionID string) (int64, error)
	// Backend is the object-store bucket that holds a bucket's objects.
	Backend(ctx context.Context, b *storagepb.Bucket) (string, error)
}

// Config configures a Server.
type Config struct {
	// Listen is the address, e.g. ":4580" or "127.0.0.1:0".
	Listen string
	// Domain is the virtual-hosted-style suffix ("<bucket>.<Domain>").
	Domain string
	// Region is the buckets' region: requests must be signed for it, and
	// GetBucketLocation returns it.
	Region string
}

// Server is the S3 HTTP front.
type Server struct {
	cfg      Config
	gate     *domain.S3Gate
	buckets  BucketOps
	upstream Upstream
	clk      clock.Clock
	log      *slog.Logger
	peers    *peer.Directory // data-plane audit sink lookup (AuditTo)

	mu   sync.Mutex
	srv  *http.Server
	addr net.Addr
	wg   sync.WaitGroup
}

// NewServer returns a server; Start begins listening.
func NewServer(cfg Config, gate *domain.S3Gate, buckets BucketOps, upstream Upstream, clk clock.Clock, log *slog.Logger) *Server {
	return &Server{cfg: cfg, gate: gate, buckets: buckets, upstream: upstream, clk: clk, log: log}
}

// Start listens on cfg.Listen and serves until Stop.
func (s *Server) Start(ctx context.Context) error {
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", s.cfg.Listen)
	if err != nil {
		return fmt.Errorf("s3 listen %s: %w", s.cfg.Listen, err)
	}
	srv := &http.Server{Handler: s, ReadHeaderTimeout: 30 * time.Second}
	s.mu.Lock()
	s.srv, s.addr = srv, ln.Addr()
	s.mu.Unlock()
	s.wg.Go(func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.ErrorContext(ctx, "s3 server stopped", "error", err)
		}
	})
	s.log.InfoContext(ctx, "s3 front listening", "addr", ln.Addr().String())
	return nil
}

// Addr is the bound address (nil before Start).
func (s *Server) Addr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addr
}

// Stop shuts the listener down, waiting for in-flight requests until ctx ends.
func (s *Server) Stop(ctx context.Context) error {
	s.mu.Lock()
	srv := s.srv
	s.mu.Unlock()
	if srv == nil {
		return nil
	}
	err := srv.Shutdown(ctx)
	s.wg.Wait()
	return err
}

// ServeHTTP authenticates, authorizes and serves one S3 request.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	t := parseTarget(r, s.cfg.Domain)
	if r.Method == http.MethodOptions {
		s.preflight(w, r, t)
		return
	}
	sr := &statusRecorder{ResponseWriter: w}
	rw := &responseWriter{ResponseWriter: sr}
	defer func() { s.recordAccess(r, t, sr, rw.bulkDelete) }() // after finish: the final status
	defer rw.finish()
	if origin := r.Header.Get("Origin"); origin != "" && t.Bucket != "" {
		rw.vary = true
		if b, err := s.gate.Bucket(r.Context(), t.Bucket); err == nil {
			if m, ok := domain.MatchCORS(b.GetSpec().GetCors(), origin, r.Method, nil); ok {
				rw.cors = &m
			}
		}
	}
	if err := s.handle(rw, r, t); err != nil {
		s.fail(rw, r, err)
	}
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	e := toS3Error(err)
	e.Resource = r.URL.Path
	if e.status >= http.StatusInternalServerError && e.status != http.StatusNotImplemented {
		s.log.ErrorContext(r.Context(), "s3 request failed", "method", r.Method, "path", r.URL.Path, "error", err)
	} else {
		s.log.DebugContext(r.Context(), "s3 request rejected", "method", r.Method, "path", r.URL.Path, "code", e.Code, "error", err)
	}
	writeXML(w, e.status, e)
}

// body is the request payload as it will be forwarded.
type body struct {
	r      io.Reader
	length int64
	hash   string
}

// authenticate verifies the SigV4 signature with the access key's secret and
// returns the principal and the (decoded) payload. An unsigned request is
// anonymous: principal "".
func (s *Server) authenticate(r *http.Request) (string, body, error) {
	a, err := sigv4.Parse(r)
	if errors.Is(err, sigv4.ErrMissingAuth) {
		return "", body{r: r.Body, length: max(r.ContentLength, 0), hash: sigv4.UnsignedPayload}, nil
	}
	if err != nil {
		return "", body{}, err
	}
	if err := sigv4.CheckRegion(a, s.cfg.Region); err != nil {
		return "", body{}, err
	}
	key, err := s.gate.ResolveKey(r.Context(), a.AccessKeyID)
	if err != nil {
		return "", body{}, err
	}
	if err := sigv4.Verify(r, a, key.Secret, s.clk.Now()); err != nil {
		return "", body{}, err
	}
	b := body{r: r.Body, length: max(r.ContentLength, 0), hash: a.PayloadHash}
	if a.Streaming() {
		cr, err := sigv4.NewChunkedReader(r.Body, a, key.Secret)
		if err != nil {
			return "", body{}, err
		}
		n, err := strconv.ParseInt(r.Header.Get("X-Amz-Decoded-Content-Length"), 10, 64)
		if err != nil || n < 0 {
			return "", body{}, errs.BadRequest(errs.Violation{Field: "x-amz-decoded-content-length", Message: "required for aws-chunked uploads"})
		}
		b = body{r: cr, length: n, hash: sigv4.UnsignedPayload}
	}
	return key.PrincipalID, b, nil
}

func (s *Server) handle(w *responseWriter, r *http.Request, t target) error {
	principal, b, err := s.authenticate(r)
	if err != nil {
		return err
	}
	op, err := classify(r, t)
	if err != nil {
		return err
	}
	// Operations started for S3 calls record the key's account as created_by.
	ctx := reqctx.WithInfo(r.Context(), &reqctx.Info{RequestID: reqctx.RequestID(r.Context()), Principal: reqctx.Principal{ID: principal}})
	r = r.WithContext(ctx)
	if op.action == domain.ActListBuckets {
		return s.listBuckets(ctx, w, principal)
	}
	if op.action == domain.ActCreateBucket {
		return s.createBucket(w, r, t.Bucket, principal, b)
	}
	if op.action == domain.ActDeleteObjects {
		return s.deleteObjects(ctx, w, r, t, principal, b)
	}
	acc, src, err := s.authorize(ctx, principal, t.Bucket, op)
	if err != nil {
		return err
	}
	if err := checkFilesystemAction(acc.Bucket, op.action); err != nil {
		return err
	}
	if h, ok := configHandlers[op.action]; ok {
		return h(ctx, s, configReq{w: w, b: acc.Bucket, bd: b})
	}
	if t.store, err = s.buckets.Backend(ctx, acc.Bucket); err != nil {
		return err
	}
	if err := s.resolveCopySource(ctx, r, op.copy, src); err != nil {
		return err
	}
	w.filesystem(acc.Bucket, t)
	return s.forward(w, r, t, op, acc.Bucket, b)
}

func (s *Server) listBuckets(ctx context.Context, w http.ResponseWriter, principal string) error {
	if principal == "" {
		return sigv4.ErrMissingAuth
	}
	buckets, err := s.gate.VisibleBuckets(ctx, principal)
	if err != nil {
		return err
	}
	writeXML(w, http.StatusOK, listBucketsResult(principal, buckets))
	return nil
}

// authorize authorizes the request and, for a copy, reading the source; it
// returns the source bucket of a copy (else nil).
func (s *Server) authorize(ctx context.Context, principal, bucket string, op operation) (domain.Access, *storagepb.Bucket, error) {
	acc, err := s.gate.Authorize(ctx, domain.S3Request{Principal: principal, Bucket: bucket, Action: op.action, Target: op.target})
	if err != nil {
		return acc, nil, err
	}
	s.auditLog(ctx, principal, acc)
	if op.copy == nil {
		return acc, nil, nil
	}
	src, err := s.gate.Authorize(ctx, domain.S3Request{Principal: principal, Bucket: op.copy.Bucket, Action: domain.ActGetObject, Target: op.copy.Key})
	if err != nil {
		return acc, nil, err
	}
	s.auditLog(ctx, principal, src)
	return acc, src.Bucket, nil
}

func (s *Server) auditLog(ctx context.Context, principal string, acc domain.Access) {
	if d := acc.Decision; !d.Allowed {
		s.log.WarnContext(ctx, "authz denied", "method", "s3", "principal", principal,
			"permission", string(d.Permission), "target", d.Target, "enforced", false, "reason", d.Reason)
	}
}

// forward applies the bucket's data-plane settings and relays to the store.
func (s *Server) forward(w *responseWriter, r *http.Request, t target, op operation, b *storagepb.Bucket, bd body) error {
	ctx := r.Context()
	if slices.Contains(classedUploads, op.action) {
		class, err := effectiveClass(b, r.Header.Get("X-Amz-Storage-Class"))
		if err != nil {
			return err
		}
		r.Header.Set("X-Amz-Storage-Class", domain.BackendClass(class))
	}
	w.rewriteBody = slices.Contains(classListings, op.action) || w.names != nil && slices.Contains(namedResponses, op.action)
	if op.action.AddsBytes() {
		if err := s.checkCapacity(ctx, b, t, op, bd.length); err != nil {
			return err
		}
	}
	s.upstream.Forward(w, rustfs.ForwardRequest{
		In: r, Bucket: t.storeBucket(), Key: t.Key, Body: bd.r, ContentLength: bd.length, PayloadHash: bd.hash,
	})
	return nil
}

// classedUploads create objects with a storage class; classListings report
// storage classes in their XML bodies.
var (
	classedUploads = []domain.S3Action{domain.ActPutObject, domain.ActCopyObject, domain.ActCreateMultipartUpload}
	classListings  = []domain.S3Action{domain.ActListObjects, domain.ActListObjectVersions, domain.ActListMultipartUploads, domain.ActListParts}
)

// effectiveClass is the storage class of an upload: the bucket default when
// forced or when the request names none, else the request's class. Objects of
// filesystem buckets are always in the FILESYSTEM class.
func effectiveClass(b *storagepb.Bucket, header string) (storagepb.StorageClass, error) {
	if domain.IsFilesystemBucket(b) {
		if header != "" && header != domain.S3ClassName(storagepb.StorageClass_FILESYSTEM) {
			return 0, coded("InvalidStorageClass", http.StatusBadRequest, "objects of filesystem buckets are stored in the FILESYSTEM class, not "+header)
		}
		return storagepb.StorageClass_FILESYSTEM, nil
	}
	spec := b.GetSpec()
	def := spec.GetDefaultStorageClass()
	if def == storagepb.StorageClass_STORAGE_CLASS_UNSPECIFIED {
		def = storagepb.StorageClass_STANDARD
	}
	if spec.GetForceStorageClass() || header == "" {
		return def, nil
	}
	return domain.ParseS3Class(header)
}

func (s *Server) checkCapacity(ctx context.Context, b *storagepb.Bucket, t target, op operation, length int64) error {
	if b.GetSpec().GetMaxSizeBytes() == 0 && !domain.IsFilesystemBucket(b) {
		return nil
	}
	incoming := length
	if op.copy != nil {
		n, err := s.copySize(ctx, op.copy)
		if errors.Is(err, objstore.ErrNotFound) {
			return nil // the store answers NoSuchKey
		}
		if err != nil {
			return err
		}
		incoming = n
	}
	// An overwrite in a never-versioned bucket (filesystem buckets are never
	// versioned) replaces the old bytes.
	if (op.action == domain.ActPutObject || op.action == domain.ActCopyObject) &&
		b.GetSpec().GetVersioningPolicy() == storagepb.VersioningPolicy_DISABLED {
		if old, err := s.buckets.ObjectSize(ctx, t.storeBucket(), t.Key, ""); err == nil {
			incoming -= old
		}
	}
	return s.buckets.CheckCapacity(ctx, b, incoming)
}

// copySize is the number of bytes a copy adds.
func (s *Server) copySize(ctx context.Context, cs *copySource) (int64, error) {
	if cs.Length >= 0 {
		return cs.Length, nil
	}
	return s.buckets.ObjectSize(ctx, cs.storeBucket(), cs.Key, cs.VersionID)
}

// createBucket serves S3 CreateBucket (aws s3 mb): a Nebius bucket with
// default settings in the project of the key's service account.
func (s *Server) createBucket(w http.ResponseWriter, r *http.Request, name, principal string, bd body) error {
	if principal == "" {
		return sigv4.ErrMissingAuth
	}
	raw, err := readBody(bd)
	if err != nil {
		return err
	}
	if len(raw) > 0 {
		var cfg struct {
			LocationConstraint string `xml:"LocationConstraint"`
		}
		if err := xml.Unmarshal(raw, &cfg); err != nil { //nolint:gosec // G709: a size-bounded (1 MiB) document into a plain string struct; encoding/xml expands no entities
			return coded("MalformedXML", http.StatusBadRequest, "The XML you provided was not well-formed or did not validate against our published schema")
		}
		if c := cfg.LocationConstraint; c != "" && c != s.cfg.Region {
			return coded("InvalidLocationConstraint", http.StatusBadRequest, fmt.Sprintf("The specified location-constraint %q is not valid; this endpoint serves %s", c, s.cfg.Region))
		}
	}
	ctx := r.Context()
	project, d, err := s.gate.AuthorizeProject(ctx, principal, domain.PermBucketCreate)
	if err != nil {
		return err
	}
	s.auditLog(ctx, principal, domain.Access{Decision: d})
	if _, err := s.buckets.CreateFromS3(ctx, project, name); err != nil {
		if errs.IsKind(err, errs.KindBadRequest) {
			return coded("InvalidBucketName", http.StatusBadRequest, domainMessage(err))
		}
		return err
	}
	w.Header().Set("Location", "/"+name)
	w.WriteHeader(http.StatusOK)
	return nil
}

// maxConfigBody bounds bucket configuration documents.
const maxConfigBody = 1 << 20

// readBody reads a configuration body handled by the front itself and checks
// it against the signed payload hash (the store re-checks relayed bodies).
func readBody(bd body) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(bd.r, maxConfigBody+1))
	if err != nil {
		return nil, coded("IncompleteBody", http.StatusBadRequest, "read request body: "+err.Error())
	}
	if len(raw) > maxConfigBody {
		return nil, coded("MalformedXML", http.StatusBadRequest, "the configuration document is too large")
	}
	if h := bd.hash; h != "" && h != sigv4.UnsignedPayload && !strings.HasPrefix(h, "STREAMING-") {
		sum := sha256.Sum256(raw)
		if !strings.EqualFold(hex.EncodeToString(sum[:]), h) {
			return nil, coded("XAmzContentSHA256Mismatch", http.StatusBadRequest, "The provided 'x-amz-content-sha256' header does not match what was computed.")
		}
	}
	return raw, nil
}

const s3NS = "http://s3.amazonaws.com/doc/2006-03-01/"

type locationConstraint struct {
	XMLName xml.Name `xml:"LocationConstraint"`
	Xmlns   string   `xml:"xmlns,attr"`
	Region  string   `xml:",chardata"`
}

type listAllMyBuckets struct {
	XMLName xml.Name     `xml:"ListAllMyBucketsResult"`
	Xmlns   string       `xml:"xmlns,attr"`
	Owner   owner        `xml:"Owner"`
	Buckets []bucketItem `xml:"Buckets>Bucket"`
}

type owner struct {
	ID          string `xml:"ID"`
	DisplayName string `xml:"DisplayName"`
}

type bucketItem struct {
	Name         string `xml:"Name"`
	CreationDate string `xml:"CreationDate"`
}

func listBucketsResult(principal string, buckets []*storagepb.Bucket) listAllMyBuckets {
	out := listAllMyBuckets{Xmlns: s3NS, Owner: owner{ID: principal, DisplayName: principal}}
	for _, b := range buckets {
		out.Buckets = append(out.Buckets, bucketItem{
			Name:         b.GetMetadata().GetName(),
			CreationDate: b.GetMetadata().GetCreatedAt().AsTime().UTC().Format("2006-01-02T15:04:05.000Z"),
		})
	}
	return out
}
