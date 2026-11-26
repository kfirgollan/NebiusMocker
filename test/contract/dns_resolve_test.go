package contract

import (
	"slices"
	"testing"

	"github.com/miekg/dns"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	dnspb "github.com/nebius/gosdk/proto/nebius/dns/v1"
)

// queryTCP asks the responder over TCP.
func (e *dnsEnv) queryTCP(t *testing.T, name string, qtype uint16) *dns.Msg {
	t.Helper()
	m := new(dns.Msg)
	m.SetQuestion(name, qtype)
	resp, _, err := (&dns.Client{Net: "tcp"}).ExchangeContext(t.Context(), m, e.mod.DNSAddr())
	if err != nil {
		t.Fatalf("tcp query %s %s: %v", name, dns.TypeToString[qtype], err)
	}
	return resp
}

// wantRcode fails the test unless the response has the rcode.
func wantRcode(t *testing.T, resp *dns.Msg, rcode int) {
	t.Helper()
	if resp.Rcode != rcode {
		t.Fatalf("rcode = %s, want %s (response %v)", dns.RcodeToString[resp.Rcode], dns.RcodeToString[rcode], resp)
	}
}

func TestDNSResolveRecordTypes(t *testing.T) {
	t.Parallel()
	e := startDNS(t)
	zid := e.createZone(t, "types", "my.example.").GetMetadata().GetId()
	e.createRecord(t, zid, "", "mx", dnspb.RecordSpec_MX, "10 mail.my.example.")
	e.createRecord(t, zid, "", "txt", dnspb.RecordSpec_TXT, `"v=spf1 -all" "second"`)
	e.createRecord(t, zid, "", "_sip._tcp", dnspb.RecordSpec_SRV, "10 5 5060 sip.my.example.")
	e.createRecord(t, zid, "", "@", dnspb.RecordSpec_CAA, `0 issue "letsencrypt.org"`)
	e.createRecord(t, zid, "", "svc", dnspb.RecordSpec_SVCB, "1 svc.my.example. alpn=h2")
	e.createRecord(t, zid, "", "web", dnspb.RecordSpec_HTTPS, "1 web.my.example. alpn=h2,h3")
	e.recordWithTTL(t, zid, "", "fast", dnspb.RecordSpec_A, "192.0.2.7", 120)
	e.createRecord(t, zid, "", "slow", dnspb.RecordSpec_A, "192.0.2.8")

	for _, tcp := range []bool{false, true} {
		ask := e.query
		if tcp {
			ask = e.queryTCP
		}
		check := func(name string, qtype uint16, ttl uint32, want string) {
			t.Helper()
			resp := ask(t, name, qtype)
			wantRcode(t, resp, dns.RcodeSuccess)
			if !resp.Authoritative || len(resp.Answer) != 1 {
				t.Fatalf("tcp=%v %s %s: answer = %v (aa=%v)", tcp, name, dns.TypeToString[qtype], resp.Answer, resp.Authoritative)
			}
			rr := resp.Answer[0]
			if rr.Header().Ttl != ttl || rr.Header().Name != name || rr.Header().Class != dns.ClassINET {
				t.Errorf("tcp=%v header = %v, want ttl %d", tcp, rr.Header(), ttl)
			}
			if got := rr.String()[len(rr.Header().String()):]; got != want {
				t.Errorf("tcp=%v %s rdata = %q, want %q", tcp, name, got, want)
			}
		}
		check("mx.my.example.", dns.TypeMX, 600, "10 mail.my.example.")
		check("txt.my.example.", dns.TypeTXT, 600, `"v=spf1 -all" "second"`)
		check("_sip._tcp.my.example.", dns.TypeSRV, 600, "10 5 5060 sip.my.example.")
		check("my.example.", dns.TypeCAA, 600, `0 issue "letsencrypt.org"`)
		check("svc.my.example.", dns.TypeSVCB, 600, "1 svc.my.example. alpn=\"h2\"")
		check("web.my.example.", dns.TypeHTTPS, 600, "1 web.my.example. alpn=\"h2,h3\"")
		check("fast.my.example.", dns.TypeA, 120, "192.0.2.7")
		check("slow.my.example.", dns.TypeA, 600, "192.0.2.8")
	}
}

func TestDNSResolveRoundRobinSet(t *testing.T) {
	t.Parallel()
	e := startDNS(t)
	zid := e.createZone(t, "rr", "rr.example.").GetMetadata().GetId()
	want := []string{"192.0.2.1", "192.0.2.2", "192.0.2.3"}
	for _, ip := range want {
		e.createRecord(t, zid, "", "web", dnspb.RecordSpec_A, ip)
	}
	resp := e.query(t, "web.rr.example.", dns.TypeA)
	wantRcode(t, resp, dns.RcodeSuccess)
	var got []string
	for _, rr := range resp.Answer {
		got = append(got, rr.(*dns.A).A.String())
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("A set = %v, want %v", got, want)
	}
}

func TestDNSResolveApex(t *testing.T) {
	t.Parallel()
	e := startDNS(t)
	e.createZone(t, "apex", "apex.example.")

	soa := e.query(t, "apex.example.", dns.TypeSOA)
	wantRcode(t, soa, dns.RcodeSuccess)
	if len(soa.Answer) != 1 || soa.Answer[0].(*dns.SOA).Ns != "ns.internal." || soa.Answer[0].(*dns.SOA).Minttl != 900 {
		t.Errorf("SOA answer = %v", soa.Answer)
	}
	ns := e.query(t, "apex.example.", dns.TypeNS)
	wantRcode(t, ns, dns.RcodeSuccess)
	if len(ns.Answer) != 1 || ns.Answer[0].(*dns.NS).Ns != "ns.internal." || ns.Answer[0].Header().Ttl != 3600 {
		t.Errorf("NS answer = %v", ns.Answer)
	}
}

func TestDNSResolveNegativeAnswers(t *testing.T) {
	t.Parallel()
	e := startDNS(t)
	ctx := t.Context()
	zid := e.createZone(t, "neg", "neg.example.").GetMetadata().GetId()
	e.createRecord(t, zid, "", "host", dnspb.RecordSpec_A, "192.0.2.1")
	e.createRecord(t, zid, "", "a.b.c", dnspb.RecordSpec_A, "192.0.2.2")

	authorityTTL := func(resp *dns.Msg) uint32 {
		t.Helper()
		if len(resp.Ns) != 1 {
			t.Fatalf("authority = %v, want one SOA", resp.Ns)
		}
		soa, ok := resp.Ns[0].(*dns.SOA)
		if !ok {
			t.Fatalf("authority = %v, want an SOA", resp.Ns[0])
		}
		return soa.Hdr.Ttl
	}

	// NXDOMAIN: min(3600, negative TTL), default 900.
	nx := e.query(t, "missing.neg.example.", dns.TypeA)
	wantRcode(t, nx, dns.RcodeNameError)
	if !nx.Authoritative || len(nx.Answer) != 0 || authorityTTL(nx) != 900 {
		t.Errorf("NXDOMAIN = %v", nx)
	}
	// NODATA: the name exists, the type does not.
	nodata := e.query(t, "host.neg.example.", dns.TypeAAAA)
	wantRcode(t, nodata, dns.RcodeSuccess)
	if len(nodata.Answer) != 0 || authorityTTL(nodata) != 900 {
		t.Errorf("NODATA = %v", nodata)
	}
	// Empty non-terminals exist.
	ent := e.query(t, "b.c.neg.example.", dns.TypeA)
	wantRcode(t, ent, dns.RcodeSuccess)
	if len(ent.Answer) != 0 || len(ent.Ns) != 1 {
		t.Errorf("empty non-terminal = %v", ent)
	}
	if e.query(t, "c.neg.example.", dns.TypeA).Rcode != dns.RcodeSuccess {
		t.Errorf("c.neg.example. should exist as an empty non-terminal")
	}
	wantRcode(t, e.query(t, "x.c.neg.example.", dns.TypeA), dns.RcodeNameError)

	// A custom negative TTL is served, capped at the SOA TTL (3600).
	for _, tc := range []struct{ negative int64 }{{300}, {7200}} {
		spec := zoneSpec("neg.example.", e.netID)
		spec.SoaSpec = &dnspb.SoaSpec{NegativeTtl: tc.negative}
		op, err := e.zones.Update(ctx, &dnspb.UpdateZoneRequest{
			Metadata: &commonpb.ResourceMetadata{Id: zid, Name: "neg"}, Spec: spec,
		})
		kmsMustWait(t, op, err)
		want := uint32(min(tc.negative, 3600))
		nx = e.query(t, "missing.neg.example.", dns.TypeA)
		wantRcode(t, nx, dns.RcodeNameError)
		if got := authorityTTL(nx); got != want {
			t.Errorf("negative_ttl %d: NXDOMAIN authority TTL = %d, want %d", tc.negative, got, want)
		}
	}
}

func TestDNSResolveCustomNegativeTTLAtCreate(t *testing.T) {
	t.Parallel()
	e := startDNS(t)
	spec := zoneSpec("neg300.example.", e.netID)
	spec.SoaSpec = &dnspb.SoaSpec{NegativeTtl: 300}
	if _, err := e.tryZone(t.Context(), e.inst.ProjectID, "neg300", spec); err != nil {
		t.Fatalf("create zone: %v", err)
	}
	nx := e.query(t, "nope.neg300.example.", dns.TypeA)
	wantRcode(t, nx, dns.RcodeNameError)
	if len(nx.Ns) != 1 || nx.Ns[0].Header().Ttl != 300 {
		t.Errorf("authority = %v, want TTL 300", nx.Ns)
	}
}

func TestDNSResolveWildcards(t *testing.T) {
	t.Parallel()
	e := startDNS(t)
	zid := e.createZone(t, "wild", "wild.example.").GetMetadata().GetId()
	e.createRecord(t, zid, "", "*.app", dnspb.RecordSpec_A, "192.0.2.1")
	e.createRecord(t, zid, "", "special.app", dnspb.RecordSpec_TXT, `"only text"`)
	e.createRecord(t, zid, "", "*", dnspb.RecordSpec_A, "192.0.2.9")

	// The wildcard answers with the query name as owner.
	resp := e.query(t, "anything.app.wild.example.", dns.TypeA)
	wantRcode(t, resp, dns.RcodeSuccess)
	if len(resp.Answer) != 1 || resp.Answer[0].Header().Name != "anything.app.wild.example." || resp.Answer[0].(*dns.A).A.String() != "192.0.2.1" {
		t.Errorf("wildcard answer = %v", resp.Answer)
	}
	// It also covers deeper names below the encloser.
	resp = e.query(t, "a.b.app.wild.example.", dns.TypeA)
	wantRcode(t, resp, dns.RcodeSuccess)
	if len(resp.Answer) != 1 || resp.Answer[0].Header().Name != "a.b.app.wild.example." {
		t.Errorf("deep wildcard answer = %v", resp.Answer)
	}
	// An existing name is not overridden: special.app has no A record.
	resp = e.query(t, "special.app.wild.example.", dns.TypeA)
	wantRcode(t, resp, dns.RcodeSuccess)
	if len(resp.Answer) != 0 {
		t.Errorf("existing name answered by the wildcard: %v", resp.Answer)
	}
	resp = e.query(t, "special.app.wild.example.", dns.TypeTXT)
	if len(resp.Answer) != 1 {
		t.Errorf("TXT answer = %v", resp.Answer)
	}
	// The zone-level wildcard.
	resp = e.query(t, "other.wild.example.", dns.TypeA)
	wantRcode(t, resp, dns.RcodeSuccess)
	if len(resp.Answer) != 1 || resp.Answer[0].(*dns.A).A.String() != "192.0.2.9" {
		t.Errorf("zone wildcard answer = %v", resp.Answer)
	}
	// No wildcard for a type it does not have: NODATA.
	resp = e.query(t, "other.wild.example.", dns.TypeMX)
	wantRcode(t, resp, dns.RcodeSuccess)
	if len(resp.Answer) != 0 {
		t.Errorf("MX answer = %v", resp.Answer)
	}
}

func TestDNSResolveLongestZoneWins(t *testing.T) {
	t.Parallel()
	e := startDNS(t)
	parent := e.createZone(t, "parent", "my.example.").GetMetadata().GetId()
	e.createRecord(t, parent, "", "x.sub", dnspb.RecordSpec_A, "192.0.2.1")
	e.createRecord(t, parent, "", "y.sub", dnspb.RecordSpec_A, "192.0.2.2")
	e.createRecord(t, parent, "", "top", dnspb.RecordSpec_A, "192.0.2.3")
	child := e.createZone(t, "child", "sub.my.example.").GetMetadata().GetId()
	e.createRecord(t, child, "", "x", dnspb.RecordSpec_A, "198.51.100.1")

	resp := e.query(t, "x.sub.my.example.", dns.TypeA)
	wantRcode(t, resp, dns.RcodeSuccess)
	if len(resp.Answer) != 1 || resp.Answer[0].(*dns.A).A.String() != "198.51.100.1" {
		t.Errorf("x.sub answer = %v, want the child zone's record", resp.Answer)
	}
	// The child zone owns the whole subtree, so the parent's y.sub is hidden.
	wantRcode(t, e.query(t, "y.sub.my.example.", dns.TypeA), dns.RcodeNameError)
	// The child's apex SOA comes from the child zone.
	soa := e.query(t, "sub.my.example.", dns.TypeSOA)
	wantRcode(t, soa, dns.RcodeSuccess)
	if len(soa.Answer) != 1 || soa.Answer[0].Header().Name != "sub.my.example." {
		t.Errorf("child SOA = %v", soa.Answer)
	}
	// The parent still answers outside the child.
	resp = e.query(t, "top.my.example.", dns.TypeA)
	if len(resp.Answer) != 1 || resp.Answer[0].(*dns.A).A.String() != "192.0.2.3" {
		t.Errorf("top answer = %v", resp.Answer)
	}
}

func TestDNSResolveRefusedOutsideZones(t *testing.T) {
	t.Parallel()
	e := startDNS(t)
	wantRcode(t, e.query(t, "www.example.com.", dns.TypeA), dns.RcodeRefused) // no zones at all
	e.createZone(t, "one", "my.example.")
	wantRcode(t, e.query(t, "www.example.com.", dns.TypeA), dns.RcodeRefused)
	wantRcode(t, e.query(t, "example.", dns.TypeSOA), dns.RcodeRefused)
	wantRcode(t, e.query(t, "notmy.example.", dns.TypeA), dns.RcodeRefused)
	wantRcode(t, e.queryTCP(t, "www.example.com.", dns.TypeA), dns.RcodeRefused)
}

func TestDNSResolveFollowsChanges(t *testing.T) {
	t.Parallel()
	e := startDNS(t)
	ctx := t.Context()
	zid := e.createZone(t, "live", "live.example.").GetMetadata().GetId()
	answers := func(qtype uint16) []dns.RR {
		t.Helper()
		resp := e.query(t, "www.live.example.", qtype)
		return resp.Answer
	}
	if len(answers(dns.TypeA)) != 0 {
		t.Fatal("answer before the record exists")
	}

	r := e.createRecord(t, zid, "", "www", dnspb.RecordSpec_A, "192.0.2.1")
	if a := answers(dns.TypeA); len(a) != 1 || a[0].(*dns.A).A.String() != "192.0.2.1" {
		t.Fatalf("after create = %v", a)
	}

	spec := recSpec("www", dnspb.RecordSpec_A, "192.0.2.2")
	spec.Ttl = 42
	op, err := e.records.Update(ctx, &dnspb.UpdateRecordRequest{Metadata: &commonpb.ResourceMetadata{Id: r.GetMetadata().GetId()}, Spec: spec})
	kmsMustWait(t, op, err)
	if a := answers(dns.TypeA); len(a) != 1 || a[0].(*dns.A).A.String() != "192.0.2.2" || a[0].Header().Ttl != 42 {
		t.Fatalf("after update = %v", a)
	}

	e.deleteRecord(t, r.GetMetadata().GetId())
	if a := answers(dns.TypeA); len(a) != 0 {
		t.Fatalf("after delete = %v", a)
	}
	wantRcode(t, e.query(t, "www.live.example.", dns.TypeA), dns.RcodeNameError)

	// Deleting the zone ends the answers altogether.
	e.deleteZone(t, zid)
	wantRcode(t, e.query(t, "www.live.example.", dns.TypeA), dns.RcodeRefused)
	wantRcode(t, e.query(t, "live.example.", dns.TypeSOA), dns.RcodeRefused)
}
