package responder_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
	"go.uber.org/goleak"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	dnspb "github.com/nebius/gosdk/proto/nebius/dns/v1"

	"github.com/kfirgollan/NebiusMocker/services/dns/domain"
	"github.com/kfirgollan/NebiusMocker/services/dns/responder"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

const zoneDomain = "my.example."

// fakeSource serves one zone, like domain.Service.FindZone does for the
// names it covers.
type fakeSource struct {
	mu   sync.Mutex
	zone domain.ZoneData
	err  error
}

func (f *fakeSource) FindZone(_ context.Context, name string) (domain.ZoneData, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return domain.ZoneData{}, false, f.err
	}
	if name != zoneDomain && !strings.HasSuffix(name, "."+zoneDomain) {
		return domain.ZoneData{}, false, nil
	}
	return f.zone, true, nil
}

func rec(rel string, typ dnspb.RecordSpec_RecordType, ttl int64, data string) *dnspb.Record {
	fqdn := zoneDomain
	if rel != "@" {
		fqdn = rel + "." + zoneDomain
	}
	return &dnspb.Record{
		Metadata: &commonpb.ResourceMetadata{Id: fmt.Sprintf("dnsrecord-%s-%s-%s", rel, typ, data)},
		Spec:     &dnspb.RecordSpec{RelativeName: rel, Type: typ, Ttl: ttl, Data: data},
		Status:   &dnspb.RecordStatus{ZoneDomainName: zoneDomain, EffectiveFqdn: fqdn},
	}
}

func testZone() domain.ZoneData {
	recs := []*dnspb.Record{
		rec("@", dnspb.RecordSpec_SOA, 3600, "ns.internal. hostmaster.nebius.internal. 1 3600 600 604800 60"),
		rec("@", dnspb.RecordSpec_NS, 3600, "ns.internal."),
		rec("www", dnspb.RecordSpec_A, 0, "10.0.0.1"),
		rec("www", dnspb.RecordSpec_A, 120, "10.0.0.2"),
		rec("www", dnspb.RecordSpec_TXT, 30, `"hello"`),
		rec("mail", dnspb.RecordSpec_MX, 300, "10 mx.my.example."),
		rec("*", dnspb.RecordSpec_A, 45, "10.0.0.9"),
		rec("a.b", dnspb.RecordSpec_A, 300, "10.0.1.1"),
		rec("*.wild", dnspb.RecordSpec_TXT, 300, `"w"`),
		rec("broken", dnspb.RecordSpec_A, 300, "not-an-ip"),
	}
	for i := range 20 {
		recs = append(recs, rec("big", dnspb.RecordSpec_TXT, 300, fmt.Sprintf(`"%02d-%s"`, i, strings.Repeat("x", 100))))
	}
	return domain.ZoneData{
		Zone: &dnspb.Zone{
			Metadata: &commonpb.ResourceMetadata{Id: "dnszone-1"},
			Spec:     &dnspb.ZoneSpec{DomainName: zoneDomain, SoaSpec: &dnspb.SoaSpec{NegativeTtl: 60}},
		},
		Records: recs,
	}
}

// start runs a server on a free port and returns its "host:port".
func start(t *testing.T, src responder.Source) string {
	t.Helper()
	srv := responder.New("127.0.0.1:0", src, slog.New(slog.DiscardHandler))
	if err := srv.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()
		if err := srv.Stop(ctx); err != nil {
			t.Errorf("Stop: %v", err)
		}
	})
	addr := srv.Addr()
	if addr == nil {
		t.Fatal("Addr() = nil after Start")
	}
	return addr.String()
}

func query(t *testing.T, network, addr, name string, qtype uint16) *dns.Msg {
	t.Helper()
	m := new(dns.Msg)
	m.SetQuestion(name, qtype)
	return exchange(t, network, addr, m)
}

// queryEDNS is query from a client that advertises a 4096-byte UDP buffer.
func queryEDNS(t *testing.T, addr, name string, qtype uint16) *dns.Msg {
	t.Helper()
	m := new(dns.Msg)
	m.SetQuestion(name, qtype)
	m.SetEdns0(4096, false)
	return exchange(t, "udp", addr, m)
}

func exchange(t *testing.T, network, addr string, m *dns.Msg) *dns.Msg {
	t.Helper()
	qtype := m.Question[0].Qtype
	name := m.Question[0].Name
	c := &dns.Client{Net: network, Timeout: 5 * time.Second}
	resp, _, err := c.ExchangeContext(t.Context(), m, addr)
	if err != nil {
		t.Fatalf("%s query %s %s: %v", network, name, dns.TypeToString[qtype], err)
	}
	return resp
}

// rawUDPSize returns the size of the datagram the server sends for a query
// without EDNS.
func rawUDPSize(t *testing.T, addr, name string, qtype uint16) int {
	t.Helper()
	m := new(dns.Msg)
	m.SetQuestion(name, qtype)
	wire, err := m.Pack()
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}
	var d net.Dialer
	conn, err := d.DialContext(t.Context(), "udp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write(wire); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 65535)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return n
}

func TestAnswers(t *testing.T) {
	t.Parallel()
	addr := start(t, &fakeSource{zone: testZone()})

	type rr struct {
		name string
		typ  uint16
		ttl  uint32
	}
	tests := []struct {
		name      string
		qname     string
		qtype     uint16
		rcode     int
		answers   []rr
		authority []rr // SOA TTLs only matter here
	}{
		{
			name: "A with default and explicit ttl", qname: "www.my.example.", qtype: dns.TypeA, rcode: dns.RcodeSuccess,
			answers: []rr{{"www.my.example.", dns.TypeA, 600}, {"www.my.example.", dns.TypeA, 120}},
		},
		{
			name: "case-insensitive name", qname: "WWW.My.Example.", qtype: dns.TypeA, rcode: dns.RcodeSuccess,
			answers: []rr{{"www.my.example.", dns.TypeA, 600}, {"www.my.example.", dns.TypeA, 120}},
		},
		{
			name: "MX", qname: "mail.my.example.", qtype: dns.TypeMX, rcode: dns.RcodeSuccess,
			answers: []rr{{"mail.my.example.", dns.TypeMX, 300}},
		},
		{
			name: "apex SOA", qname: "my.example.", qtype: dns.TypeSOA, rcode: dns.RcodeSuccess,
			answers: []rr{{"my.example.", dns.TypeSOA, 3600}},
		},
		{
			name: "apex NS", qname: "my.example.", qtype: dns.TypeNS, rcode: dns.RcodeSuccess,
			answers: []rr{{"my.example.", dns.TypeNS, 3600}},
		},
		{
			name: "ANY returns all types", qname: "www.my.example.", qtype: dns.TypeANY, rcode: dns.RcodeSuccess,
			answers: []rr{{"www.my.example.", dns.TypeA, 600}, {"www.my.example.", dns.TypeA, 120}, {"www.my.example.", dns.TypeTXT, 30}},
		},
		{
			name: "wildcard synthesized with the query name", qname: "anything.my.example.", qtype: dns.TypeA, rcode: dns.RcodeSuccess,
			answers: []rr{{"anything.my.example.", dns.TypeA, 45}},
		},
		{
			name: "wildcard below a closest encloser", qname: "x.wild.my.example.", qtype: dns.TypeTXT, rcode: dns.RcodeSuccess,
			answers: []rr{{"x.wild.my.example.", dns.TypeTXT, 300}},
		},
		{
			name: "existing name hides the wildcard: NODATA", qname: "mail.my.example.", qtype: dns.TypeA, rcode: dns.RcodeSuccess,
			authority: []rr{{"my.example.", dns.TypeSOA, 60}},
		},
		{
			name: "wildcard of another type: NODATA", qname: "anything.my.example.", qtype: dns.TypeMX, rcode: dns.RcodeSuccess,
			authority: []rr{{"my.example.", dns.TypeSOA, 60}},
		},
		{
			name: "empty non-terminal: NODATA", qname: "b.my.example.", qtype: dns.TypeA, rcode: dns.RcodeSuccess,
			authority: []rr{{"my.example.", dns.TypeSOA, 60}},
		},
		{name: "outside every zone", qname: "other.test.", qtype: dns.TypeA, rcode: dns.RcodeRefused},
		{name: "parent of the zone", qname: "example.", qtype: dns.TypeA, rcode: dns.RcodeRefused},
		{
			name: "unparsable record is skipped, the wildcard answers", qname: "broken.my.example.", qtype: dns.TypeA, rcode: dns.RcodeSuccess,
			answers: []rr{{"broken.my.example.", dns.TypeA, 45}},
		},
	}
	for _, tt := range tests {
		for _, network := range []string{"udp", "tcp"} {
			t.Run(tt.name+"/"+network, func(t *testing.T) {
				t.Parallel()
				resp := query(t, network, addr, tt.qname, tt.qtype)
				if resp.Rcode != tt.rcode {
					t.Fatalf("rcode = %s, want %s", dns.RcodeToString[resp.Rcode], dns.RcodeToString[tt.rcode])
				}
				wantAA := tt.rcode != dns.RcodeRefused
				if resp.Authoritative != wantAA {
					t.Errorf("AA = %v, want %v", resp.Authoritative, wantAA)
				}
				if resp.Truncated {
					t.Error("TC set on a small answer")
				}
				check := func(section string, got []dns.RR, want []rr) {
					t.Helper()
					if len(got) != len(want) {
						t.Fatalf("%s has %d records, want %d: %v", section, len(got), len(want), got)
					}
					seen := map[rr]int{}
					for _, g := range got {
						h := g.Header()
						seen[rr{h.Name, h.Rrtype, h.Ttl}]++
					}
					for _, w := range want {
						if seen[w] == 0 {
							t.Errorf("%s lacks %v; got %v", section, w, got)
						}
					}
				}
				check("answer", resp.Answer, tt.answers)
				check("authority", resp.Ns, tt.authority)
			})
		}
	}
}

func TestNXDOMAIN(t *testing.T) {
	t.Parallel()
	addr := start(t, &fakeSource{zone: testZone()})
	// "*.wild" only covers names below "wild"; "other.test.my.example." has
	// a closest encloser (the apex) whose wildcard exists, so use a zone
	// without a root wildcard for a real NXDOMAIN.
	z := testZone()
	kept := z.Records[:0]
	for _, r := range z.Records {
		if r.GetSpec().GetRelativeName() != "*" {
			kept = append(kept, r)
		}
	}
	z.Records = kept
	noWild := start(t, &fakeSource{zone: z})

	for _, network := range []string{"udp", "tcp"} {
		t.Run(network, func(t *testing.T) {
			t.Parallel()
			resp := query(t, network, noWild, "missing.my.example.", dns.TypeA)
			if resp.Rcode != dns.RcodeNameError || !resp.Authoritative || len(resp.Answer) != 0 {
				t.Fatalf("response = rcode %s aa %v answers %d, want authoritative NXDOMAIN", dns.RcodeToString[resp.Rcode], resp.Authoritative, len(resp.Answer))
			}
			if len(resp.Ns) != 1 {
				t.Fatalf("authority = %v, want the SOA", resp.Ns)
			}
			soa, ok := resp.Ns[0].(*dns.SOA)
			if !ok || soa.Header().Name != zoneDomain || soa.Header().Ttl != 60 || soa.Minttl != 60 {
				t.Errorf("authority = %v, want SOA of my.example. with ttl min(3600, 60) = 60", resp.Ns[0])
			}
			// With the root wildcard in place the same name is answered.
			if wild := query(t, network, addr, "missing.my.example.", dns.TypeA); wild.Rcode != dns.RcodeSuccess || len(wild.Answer) != 1 {
				t.Errorf("with wildcard: rcode %s, %d answers, want 1 synthesized answer", dns.RcodeToString[wild.Rcode], len(wild.Answer))
			}
		})
	}
}

func TestNegativeTTLCappedBySOATTL(t *testing.T) {
	t.Parallel()
	z := testZone()
	z.Zone.Spec.SoaSpec.NegativeTtl = 100000 // larger than the SOA record's own TTL of 3600
	z.Records = z.Records[:2]
	addr := start(t, &fakeSource{zone: z})
	resp := query(t, "udp", addr, "nothing.my.example.", dns.TypeA)
	if resp.Rcode != dns.RcodeNameError || len(resp.Ns) != 1 || resp.Ns[0].Header().Ttl != 3600 {
		t.Errorf("response = rcode %s authority %v, want NXDOMAIN with SOA ttl 3600", dns.RcodeToString[resp.Rcode], resp.Ns)
	}
}

func TestBigAnswerTruncatesOverUDPOnly(t *testing.T) {
	t.Parallel()
	addr := start(t, &fakeSource{zone: testZone()})

	udp := query(t, "udp", addr, "big.my.example.", dns.TypeTXT)
	if !udp.Truncated {
		t.Errorf("UDP answer without EDNS: TC = false, want true (answers: %d)", len(udp.Answer))
	}
	if n := rawUDPSize(t, addr, "big.my.example.", dns.TypeTXT); n > dns.MinMsgSize {
		t.Errorf("UDP datagram is %d bytes, want at most %d", n, dns.MinMsgSize)
	}
	if len(udp.Answer) >= 20 {
		t.Errorf("UDP answer has all %d records, want fewer", len(udp.Answer))
	}

	// A client that advertises a larger buffer gets the whole answer.
	edns := queryEDNS(t, addr, "big.my.example.", dns.TypeTXT)
	if edns.Truncated || len(edns.Answer) != 20 {
		t.Errorf("UDP answer with EDNS 4096: TC = %v, %d answers, want complete", edns.Truncated, len(edns.Answer))
	}

	tcp := query(t, "tcp", addr, "big.my.example.", dns.TypeTXT)
	if tcp.Truncated || len(tcp.Answer) != 20 {
		t.Errorf("TCP answer: TC = %v, %d answers, want the full 20", tcp.Truncated, len(tcp.Answer))
	}
}

func TestMalformedRequests(t *testing.T) {
	t.Parallel()
	addr := start(t, &fakeSource{zone: testZone()})
	c := &dns.Client{Net: "udp", Timeout: 5 * time.Second}

	notify := new(dns.Msg)
	notify.SetQuestion("www.my.example.", dns.TypeA)
	notify.Opcode = dns.OpcodeNotify
	if resp, _, err := c.ExchangeContext(t.Context(), notify, addr); err != nil || resp.Rcode != dns.RcodeNotImplemented {
		t.Errorf("NOTIFY = %v, %v, want NOTIMP", resp, err)
	}

	two := new(dns.Msg)
	two.Question = []dns.Question{
		{Name: "www.my.example.", Qtype: dns.TypeA, Qclass: dns.ClassINET},
		{Name: "mail.my.example.", Qtype: dns.TypeMX, Qclass: dns.ClassINET},
	}
	two.Id = dns.Id()
	two.RecursionDesired = true
	if resp, _, err := c.ExchangeContext(t.Context(), two, addr); err != nil || resp.Rcode != dns.RcodeFormatError {
		t.Errorf("two questions = %v, %v, want FORMERR", resp, err)
	}

	chaos := new(dns.Msg)
	chaos.SetQuestion("www.my.example.", dns.TypeA)
	chaos.Question[0].Qclass = dns.ClassCHAOS
	if resp, _, err := c.ExchangeContext(t.Context(), chaos, addr); err != nil || resp.Rcode != dns.RcodeRefused {
		t.Errorf("CHAOS class = %v, %v, want REFUSED", resp, err)
	}
}

func TestSourceErrorIsServfail(t *testing.T) {
	t.Parallel()
	src := &fakeSource{zone: testZone(), err: errors.New("store down")}
	addr := start(t, src)
	for _, network := range []string{"udp", "tcp"} {
		if resp := query(t, network, addr, "www.my.example.", dns.TypeA); resp.Rcode != dns.RcodeServerFailure {
			t.Errorf("%s with a failing source: rcode = %s, want SERVFAIL", network, dns.RcodeToString[resp.Rcode])
		}
	}
}

func TestZoneChangesAreServed(t *testing.T) {
	t.Parallel()
	src := &fakeSource{zone: testZone()}
	addr := start(t, src)
	if resp := query(t, "udp", addr, "new.my.example.", dns.TypeMX); len(resp.Answer) != 0 {
		t.Fatalf("answers before the record exists: %v", resp.Answer)
	}
	src.mu.Lock()
	src.zone.Records = append(src.zone.Records, rec("new", dnspb.RecordSpec_A, 10, "10.9.9.9"))
	src.mu.Unlock()
	resp := query(t, "udp", addr, "new.my.example.", dns.TypeA)
	if len(resp.Answer) != 1 || resp.Answer[0].(*dns.A).A.String() != "10.9.9.9" {
		t.Errorf("answers after the record was added = %v, want 10.9.9.9", resp.Answer)
	}
}

func TestStop(t *testing.T) {
	t.Parallel()
	srv := responder.New("127.0.0.1:0", &fakeSource{zone: testZone()}, slog.New(slog.DiscardHandler))
	if srv.Addr() != nil {
		t.Errorf("Addr() = %v before Start, want nil", srv.Addr())
	}
	if err := srv.Stop(t.Context()); err != nil {
		t.Errorf("Stop before Start: %v", err)
	}
	if err := srv.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	addr := srv.Addr().String()
	if resp := query(t, "udp", addr, "www.my.example.", dns.TypeA); resp.Rcode != dns.RcodeSuccess {
		t.Fatalf("rcode before Stop = %s, want NOERROR", dns.RcodeToString[resp.Rcode])
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := srv.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	m := new(dns.Msg)
	m.SetQuestion("www.my.example.", dns.TypeA)
	c := &dns.Client{Net: "tcp", Timeout: time.Second}
	if _, _, err := c.ExchangeContext(t.Context(), m, addr); err == nil {
		t.Error("TCP query after Stop succeeded, want a connection error")
	}
}

// TestStopRightAfterStart stops a server before any query has given its
// serve goroutines a chance to run: Start must return only once both servers
// serve, or Stop finds them "not started" and waits for them forever.
func TestStopRightAfterStart(t *testing.T) {
	t.Parallel()
	for range 50 {
		srv := responder.New("127.0.0.1:0", &fakeSource{zone: testZone()}, slog.New(slog.DiscardHandler))
		if err := srv.Start(t.Context()); err != nil {
			t.Fatalf("Start: %v", err)
		}
		// Stop's own wait is unbounded once shutdown fails, so bound it here:
		// a regression fails fast instead of hanging the package.
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		stopped := make(chan error, 1)
		go func() { stopped <- srv.Stop(ctx) }()
		select {
		case err := <-stopped:
			cancel()
			if err != nil {
				t.Fatalf("Stop right after Start: %v", err)
			}
		case <-ctx.Done():
			cancel()
			t.Fatal("Stop right after Start did not return within 10s")
		}
	}
}

func TestStartRejectsBadAddress(t *testing.T) {
	t.Parallel()
	srv := responder.New("not-an-address", &fakeSource{zone: testZone()}, slog.New(slog.DiscardHandler))
	if err := srv.Start(t.Context()); err == nil {
		t.Error("Start(not-an-address) succeeded, want an error")
		_ = srv.Stop(t.Context())
	}
}
