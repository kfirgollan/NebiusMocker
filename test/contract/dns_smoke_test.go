package contract

import (
	"testing"

	"github.com/miekg/dns"

	dnspb "github.com/nebius/gosdk/proto/nebius/dns/v1"
)

// TestDNSSmoke creates a zone and an A record with the Go SDK and resolves
// the record through the DNS responder.
func TestDNSSmoke(t *testing.T) {
	t.Parallel()
	e := startDNS(t)
	z := e.createZone(t, "my-zone", "my.example.")
	if got := z.GetStatus().GetRecordCount(); got != 2 {
		t.Fatalf("record_count = %d, want 2 (SOA and NS)", got)
	}
	r := e.createRecord(t, z.GetMetadata().GetId(), "www-a", "www", dnspb.RecordSpec_A, "192.0.2.10")
	if got := r.GetStatus().GetEffectiveFqdn(); got != "www.my.example." {
		t.Fatalf("effective_fqdn = %q", got)
	}
	resp := e.query(t, "www.my.example.", dns.TypeA)
	if resp.Rcode != dns.RcodeSuccess || len(resp.Answer) != 1 {
		t.Fatalf("answer = %v", resp)
	}
	if a, ok := resp.Answer[0].(*dns.A); !ok || a.A.String() != "192.0.2.10" || a.Hdr.Ttl != 600 {
		t.Fatalf("answer = %v", resp.Answer[0])
	}
	nx := e.query(t, "nope.my.example.", dns.TypeA)
	if nx.Rcode != dns.RcodeNameError || len(nx.Ns) != 1 {
		t.Fatalf("nxdomain = %v", nx)
	}
}
