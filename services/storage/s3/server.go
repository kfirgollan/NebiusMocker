// Package s3 is the S3 data-plane front of Object Storage (transport).
//
// It authenticates every request with a Nebius access key (SigV4 against
// iam.v2 AccessKey secrets), authorizes it against Nebius buckets and IAM
// permits (domain.S3Gate), and relays it to the object store (RustFS)
// re-signed with the store's own credentials. Handlers make no decisions of
// their own beyond mapping requests to permissions.
package s3

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore/rustfs"
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

// Config configures a Server.
type Config struct {
	// Listen is the address, e.g. ":4580" or "127.0.0.1:0".
	Listen string
	// Domain is the virtual-hosted-style suffix ("<bucket>.<Domain>").
	Domain string
	// Region is returned by GetBucketLocation.
	Region string
}

// Server is the S3 HTTP front.
type Server struct {
	cfg      Config
	gate     *domain.S3Gate
	upstream Upstream
	clk      clock.Clock
	log      *slog.Logger

	mu   sync.Mutex
	srv  *http.Server
	addr net.Addr
	wg   sync.WaitGroup
}

// NewServer returns a server; Start begins listening.
func NewServer(cfg Config, gate *domain.S3Gate, upstream Upstream, clk clock.Clock, log *slog.Logger) *Server {
	return &Server{cfg: cfg, gate: gate, upstream: upstream, clk: clk, log: log}
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

// ServeHTTP authenticates, authorizes and relays one S3 request.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	t := parseTarget(r, s.cfg.Domain)
	auth, principal, body, err := s.authenticate(r)
	if err == nil {
		err = s.serve(w, r, t, auth, principal, body)
	}
	if err != nil {
		e := toS3Error(err)
		e.Resource = r.URL.Path
		if e.status >= http.StatusInternalServerError && e.status != http.StatusNotImplemented {
			s.log.ErrorContext(ctx, "s3 request failed", "method", r.Method, "path", r.URL.Path, "error", err)
		} else {
			s.log.DebugContext(ctx, "s3 request rejected", "method", r.Method, "path", r.URL.Path, "code", e.Code, "error", err)
		}
		writeXML(w, e.status, e)
	}
}

// body is the request payload as it will be forwarded.
type body struct {
	r      io.Reader
	length int64
	hash   string
}

// authenticate verifies the SigV4 signature with the access key's secret and
// returns the principal and the (decoded) payload.
func (s *Server) authenticate(r *http.Request) (sigv4.Auth, string, body, error) {
	a, err := sigv4.Parse(r)
	if err != nil {
		return a, "", body{}, err
	}
	key, err := s.gate.ResolveKey(r.Context(), a.AccessKeyID)
	if err != nil {
		return a, "", body{}, err
	}
	if err := sigv4.Verify(r, a, key.Secret, s.clk.Now()); err != nil {
		return a, "", body{}, err
	}
	b := body{r: r.Body, length: r.ContentLength, hash: a.PayloadHash}
	if a.Streaming() {
		cr, err := sigv4.NewChunkedReader(r.Body, a, key.Secret)
		if err != nil {
			return a, "", body{}, err
		}
		n, err := strconv.ParseInt(r.Header.Get("X-Amz-Decoded-Content-Length"), 10, 64)
		if err != nil || n < 0 {
			return a, "", body{}, errs.BadRequest(errs.Violation{Field: "x-amz-decoded-content-length", Message: "required for aws-chunked uploads"})
		}
		b = body{r: cr, length: n, hash: sigv4.UnsignedPayload}
	}
	if b.length < 0 {
		b.length = 0
	}
	return a, key.PrincipalID, b, nil
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request, t target, _ sigv4.Auth, principal string, b body) error {
	op, err := classify(r, t)
	if err != nil {
		return err
	}
	ctx := r.Context()
	if op.action == actionListBuckets {
		buckets, err := s.gate.VisibleBuckets(ctx, principal)
		if err != nil {
			return err
		}
		writeXML(w, http.StatusOK, listBucketsResult(principal, buckets))
		return nil
	}
	if err := s.authorize(ctx, principal, t.Bucket, op); err != nil {
		return err
	}
	if op.action == actionGetLocation {
		writeXML(w, http.StatusOK, locationConstraint{Xmlns: s3NS, Region: s.cfg.Region})
		return nil
	}
	s.upstream.Forward(w, rustfs.ForwardRequest{
		In: r, Bucket: t.Bucket, Key: t.Key, Body: b.r, ContentLength: b.length, PayloadHash: b.hash,
	})
	return nil
}

func (s *Server) authorize(ctx context.Context, principal, bucket string, op operation) error {
	acc, err := s.gate.Authorize(ctx, principal, bucket, op.perm)
	if err != nil {
		return err
	}
	s.auditLog(ctx, principal, acc)
	if op.copySource != "" && op.copySource != bucket {
		src, err := s.gate.Authorize(ctx, principal, op.copySource, domain.PermObjectGet)
		if err != nil {
			return err
		}
		s.auditLog(ctx, principal, src)
	}
	return nil
}

func (s *Server) auditLog(ctx context.Context, principal string, acc domain.Access) {
	if d := acc.Decision; !d.Allowed {
		s.log.WarnContext(ctx, "authz denied", "method", "s3", "principal", principal,
			"permission", string(d.Permission), "target", d.Target, "enforced", false, "reason", d.Reason)
	}
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
