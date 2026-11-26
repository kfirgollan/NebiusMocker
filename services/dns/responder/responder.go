// Package responder is the DNS module's data plane: an authoritative DNS
// server (github.com/miekg/dns) on UDP and TCP that answers queries from the
// mock's zones and records, so that names created through the API resolve
// with ordinary DNS clients (dig, the Go resolver, ...).
//
// It plays the role of the network resolver for names under the mock's
// zones only: it does not recurse, and refuses names no zone covers.
package responder

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/miekg/dns"

	"github.com/kfirgollan/NebiusMocker/services/dns/domain"
)

// Source finds the zone authoritative for a name (satisfied by *domain.Service).
type Source interface {
	FindZone(ctx context.Context, name string) (domain.ZoneData, bool, error)
}

// Server serves DNS over UDP and TCP on one address.
type Server struct {
	listen string
	src    Source
	log    *slog.Logger

	mu   sync.Mutex
	udp  *dns.Server
	tcp  *dns.Server
	addr *net.UDPAddr
	wg   sync.WaitGroup
}

// New returns a server for listen ("host:port"; port 0 picks a free port
// shared by UDP and TCP).
func New(listen string, src Source, log *slog.Logger) *Server {
	return &Server{listen: listen, src: src, log: log}
}

// bindAttempts bounds the retries when a random UDP port is taken for TCP.
const bindAttempts = 10

// errStoppedEarly is a serve loop that returned before it reported serving.
var errStoppedEarly = errors.New("dns responder stopped before serving")

// Start binds both sockets and returns once both servers serve; they serve
// until Stop.
//
// Waiting matters: a miekg/dns server shut down before its ActivateAndServe
// has run reports "server not started" and is left to serve forever, so a
// Stop right after Start would wait for it without bound.
func (s *Server) Start(ctx context.Context) error {
	pc, ln, err := bind(ctx, s.listen)
	if err != nil {
		return err
	}
	// The miekg/dns handler has no context: queries run under Start's.
	h := dns.HandlerFunc(func(w dns.ResponseWriter, req *dns.Msg) { s.serve(ctx, w, req) })
	udp := &dns.Server{PacketConn: pc, Handler: h}
	tcp := &dns.Server{Listener: ln, Handler: h}
	servers := []*dns.Server{udp, tcp}
	// Each server reports once: nil when it serves, or why it stopped first.
	ready := make(chan error, len(servers))
	for _, srv := range servers {
		var once sync.Once
		report := func(err error) { once.Do(func() { ready <- err }) }
		srv.NotifyStartedFunc = func() { report(nil) }
		s.wg.Go(func() {
			err := srv.ActivateAndServe()
			if err != nil {
				s.log.ErrorContext(ctx, "dns responder stopped", "error", err)
			}
			report(errors.Join(errStoppedEarly, err))
		})
	}
	if err := awaitServing(ctx, ready, len(servers)); err != nil {
		_ = pc.Close() // ends both serve loops, whether or not they got to serve
		_ = ln.Close()
		s.wg.Wait()
		return err
	}
	s.mu.Lock()
	s.udp, s.tcp = udp, tcp
	s.addr, _ = pc.LocalAddr().(*net.UDPAddr)
	s.mu.Unlock()
	s.log.InfoContext(ctx, "dns responder listening", "addr", pc.LocalAddr().String())
	return nil
}

// awaitServing waits for n servers to report on ready, failing on the first
// error or when ctx ends.
func awaitServing(ctx context.Context, ready <-chan error, n int) error {
	for range n {
		select {
		case err := <-ready:
			if err != nil {
				return err
			}
		case <-ctx.Done():
			return fmt.Errorf("dns responder start: %w", ctx.Err())
		}
	}
	return nil
}

// bind opens UDP and TCP on the same port.
func bind(ctx context.Context, listen string) (net.PacketConn, net.Listener, error) {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return nil, nil, fmt.Errorf("dns listen %q: %w", listen, err)
	}
	var lc net.ListenConfig
	for range bindAttempts {
		pc, err := lc.ListenPacket(ctx, "udp", listen)
		if err != nil {
			return nil, nil, fmt.Errorf("dns listen udp %s: %w", listen, err)
		}
		udpPort := pc.LocalAddr().(*net.UDPAddr).Port //nolint:forcetypeassert // a "udp" PacketConn has a *net.UDPAddr
		ln, err := lc.Listen(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(udpPort)))
		if err == nil {
			return pc, ln, nil
		}
		_ = pc.Close() // retry on another port, or report the TCP error
		if port != "0" || !errors.Is(err, syscall.EADDRINUSE) {
			return nil, nil, fmt.Errorf("dns listen tcp %s: %w", listen, err)
		}
	}
	return nil, nil, fmt.Errorf("dns listen %s: no port free for both UDP and TCP", listen)
}

// Addr is the bound UDP address (TCP uses the same port), nil before Start.
func (s *Server) Addr() *net.UDPAddr {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addr
}

// Stop shuts both servers down.
func (s *Server) Stop(ctx context.Context) error {
	s.mu.Lock()
	udp, tcp := s.udp, s.tcp
	s.mu.Unlock()
	if udp == nil {
		return nil
	}
	err := errors.Join(udp.ShutdownContext(ctx), tcp.ShutdownContext(ctx))
	s.wg.Wait()
	return err
}

func (s *Server) serve(ctx context.Context, w dns.ResponseWriter, req *dns.Msg) {
	resp := s.answer(ctx, req)
	if _, udp := w.RemoteAddr().(*net.UDPAddr); udp {
		size := dns.MinMsgSize
		if opt := req.IsEdns0(); opt != nil {
			size = max(int(opt.UDPSize()), dns.MinMsgSize)
		}
		resp.Truncate(size)
	}
	if err := w.WriteMsg(resp); err != nil {
		s.log.DebugContext(ctx, "dns: write response", "error", err)
	}
}

// answer builds the response to one query.
func (s *Server) answer(ctx context.Context, req *dns.Msg) *dns.Msg {
	m := new(dns.Msg)
	m.SetReply(req)
	if req.Opcode != dns.OpcodeQuery {
		m.Rcode = dns.RcodeNotImplemented
		return m
	}
	if len(req.Question) != 1 {
		m.Rcode = dns.RcodeFormatError
		return m
	}
	q := req.Question[0]
	if q.Qclass != dns.ClassINET && q.Qclass != dns.ClassANY {
		m.Rcode = dns.RcodeRefused
		return m
	}
	name := strings.ToLower(dns.Fqdn(q.Name))
	zd, ok, err := s.src.FindZone(ctx, name)
	switch {
	case err != nil:
		s.log.WarnContext(ctx, "dns: find zone", "name", name, "error", err)
		m.Rcode = dns.RcodeServerFailure
		return m
	case !ok:
		m.Rcode = dns.RcodeRefused // not authoritative, and no recursion
		return m
	}
	m.Authoritative = true
	z := newZone(ctx, s.log, zd)
	rrs, exists := z.lookup(name)
	if !exists {
		m.Rcode = dns.RcodeNameError
		m.Ns = z.negative()
		return m
	}
	for _, rr := range rrs {
		if q.Qtype == dns.TypeANY || rr.Header().Rrtype == q.Qtype {
			m.Answer = append(m.Answer, rr)
		}
	}
	if len(m.Answer) == 0 {
		m.Ns = z.negative() // NODATA
	}
	return m
}

// zone is a zone's records parsed for serving.
type zone struct {
	apex   string
	negTTL uint32
	soa    dns.RR
	owners map[string][]dns.RR
}

func newZone(ctx context.Context, log *slog.Logger, zd domain.ZoneData) *zone {
	z := &zone{apex: zd.Zone.GetSpec().GetDomainName(), negTTL: domain.EffectiveNegativeTTL(zd.Zone), owners: map[string][]dns.RR{}}
	for _, r := range zd.Records {
		spec := r.GetSpec()
		owner := r.GetStatus().GetEffectiveFqdn()
		rr, err := domain.ParseRR(owner, domain.EffectiveTTL(spec.GetTtl()), spec.GetType(), spec.GetData())
		if err != nil {
			log.WarnContext(ctx, "dns: skip unparsable record", "record", r.GetMetadata().GetId(), "error", err)
			continue
		}
		if _, isSOA := rr.(*dns.SOA); isSOA {
			z.soa = rr
		}
		z.owners[owner] = append(z.owners[owner], rr)
	}
	return z
}

// lookup returns the records at name, synthesizing them from a wildcard
// ("*.<closest encloser>", RFC 4592) when the name does not exist. exists is
// false for a name that does not exist (NXDOMAIN); an empty node, one that
// only has names below it, exists with no records.
func (z *zone) lookup(name string) (rrs []dns.RR, exists bool) {
	if rrs, ok := z.owners[name]; ok {
		return rrs, true
	}
	if z.hasBelow(name) {
		return nil, true
	}
	// Closest encloser: the nearest existing ancestor (the apex always exists).
	for ce := parent(name); ce != ""; ce = parent(ce) {
		if _, ok := z.owners[ce]; !ok && !z.hasBelow(ce) && ce != z.apex {
			continue
		}
		wild, ok := z.owners["*."+ce]
		if !ok {
			return nil, false
		}
		out := make([]dns.RR, 0, len(wild))
		for _, rr := range wild {
			c := dns.Copy(rr)
			c.Header().Name = name
			out = append(out, c)
		}
		return out, true
	}
	return nil, false
}

// hasBelow reports whether some record owner is a strict descendant of name.
func (z *zone) hasBelow(name string) bool {
	for owner := range z.owners {
		if strings.HasSuffix(owner, "."+name) {
			return true
		}
	}
	return false
}

// parent strips the leftmost label ("" past the root).
func parent(name string) string {
	_, rest, ok := strings.Cut(name, ".")
	if !ok || rest == "" {
		return ""
	}
	return rest
}

// negative is the authority section of a negative answer: the zone's SOA,
// with TTL min(SOA TTL, negative TTL) (RFC 2308).
func (z *zone) negative() []dns.RR {
	if z.soa == nil {
		return nil
	}
	soa := dns.Copy(z.soa)
	soa.Header().Ttl = min(soa.Header().Ttl, z.negTTL)
	return []dns.RR{soa}
}
