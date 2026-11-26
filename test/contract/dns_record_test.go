package contract

import (
	"fmt"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	dnspb "github.com/nebius/gosdk/proto/nebius/dns/v1"
)

func TestDNSRecordCRUD(t *testing.T) {
	t.Parallel()
	e := startDNS(t)
	ctx := t.Context()
	zid := e.createZone(t, "recs", "my.example.").GetMetadata().GetId()

	r := e.createRecord(t, zid, "www-a", "www", dnspb.RecordSpec_A, "192.0.2.10")
	rid := r.GetMetadata().GetId()
	if !strings.HasPrefix(rid, "dnsrecord-") || r.GetMetadata().GetParentId() != zid {
		t.Errorf("metadata = %v", r.GetMetadata())
	}
	if r.GetStatus().GetEffectiveFqdn() != "www.my.example." || r.GetStatus().GetZoneDomainName() != "my.example." {
		t.Errorf("status = %v", r.GetStatus())
	}
	apex := e.createRecord(t, zid, "apex-a", "@", dnspb.RecordSpec_A, "192.0.2.11")
	if apex.GetStatus().GetEffectiveFqdn() != "my.example." {
		t.Errorf("apex fqdn = %q", apex.GetStatus().GetEffectiveFqdn())
	}
	wild := e.createRecord(t, zid, "wild-a", "*.app", dnspb.RecordSpec_A, "192.0.2.12")
	if wild.GetStatus().GetEffectiveFqdn() != "*.app.my.example." {
		t.Errorf("wildcard fqdn = %q", wild.GetStatus().GetEffectiveFqdn())
	}

	byName, err := e.records.GetByName(ctx, &commonpb.GetByNameRequest{ParentId: zid, Name: "www-a"})
	if err != nil || byName.GetMetadata().GetId() != rid {
		t.Fatalf("GetByName = %v, %v", byName, err)
	}
	_, err = e.records.GetByName(ctx, &commonpb.GetByNameRequest{ParentId: zid, Name: "nope"})
	kmsWantCode(t, err, codes.NotFound)
	_, err = e.records.Get(ctx, &dnspb.GetRecordRequest{Id: "dnsrecord-doesnotexist"})
	kmsWantCode(t, err, codes.NotFound)
	_, err = e.records.Get(ctx, &dnspb.GetRecordRequest{Id: rid, ResourceVersion: r.GetMetadata().GetResourceVersion() + 9})
	kmsWantCode(t, err, codes.FailedPrecondition)

	// Update data, name and labels.
	op, err := e.records.Update(ctx, &dnspb.UpdateRecordRequest{
		Metadata: &commonpb.ResourceMetadata{Id: rid, ParentId: zid, Name: "www-renamed", Labels: map[string]string{"a": "b"}},
		Spec:     recSpec("www", dnspb.RecordSpec_A, "192.0.2.99"),
	})
	kmsMustWait(t, op, err)
	got := e.getRecord(t, rid)
	if got.GetSpec().GetData() != "192.0.2.99" || got.GetMetadata().GetName() != "www-renamed" || got.GetMetadata().GetLabels()["a"] != "b" {
		t.Errorf("after update = %v", got)
	}
	if got.GetMetadata().GetResourceVersion() <= r.GetMetadata().GetResourceVersion() {
		t.Errorf("resource_version did not grow")
	}

	// An unchanged update keeps the version.
	op, err = e.records.Update(ctx, &dnspb.UpdateRecordRequest{
		Metadata: &commonpb.ResourceMetadata{Id: rid, ParentId: zid, Name: "www-renamed", Labels: map[string]string{"a": "b"}},
		Spec:     recSpec("www", dnspb.RecordSpec_A, "192.0.2.99"),
	})
	kmsMustWait(t, op, err)
	if again := e.getRecord(t, rid); again.GetMetadata().GetResourceVersion() != got.GetMetadata().GetResourceVersion() {
		t.Errorf("resource_version %d -> %d after an unchanged update", got.GetMetadata().GetResourceVersion(), again.GetMetadata().GetResourceVersion())
	}

	// A stale resource version is refused.
	op, err = e.records.Update(ctx, &dnspb.UpdateRecordRequest{
		Metadata: &commonpb.ResourceMetadata{Id: rid, ResourceVersion: got.GetMetadata().GetResourceVersion() + 7},
		Spec:     recSpec("www", dnspb.RecordSpec_A, "192.0.2.50"),
	})
	kmsWantCode(t, kmsWait(ctx, op, err), codes.FailedPrecondition)

	// The relative name and the type are immutable.
	op, err = e.records.Update(ctx, &dnspb.UpdateRecordRequest{
		Metadata: &commonpb.ResourceMetadata{Id: rid},
		Spec:     recSpec("other", dnspb.RecordSpec_A, "192.0.2.99"),
	})
	kmsWantCode(t, kmsWait(ctx, op, err), codes.InvalidArgument)
	op, err = e.records.Update(ctx, &dnspb.UpdateRecordRequest{
		Metadata: &commonpb.ResourceMetadata{Id: rid},
		Spec:     recSpec("www", dnspb.RecordSpec_TXT, `"x"`),
	})
	kmsWantCode(t, kmsWait(ctx, op, err), codes.InvalidArgument)

	// Updated data is validated and checked for duplicates.
	op, err = e.records.Update(ctx, &dnspb.UpdateRecordRequest{Metadata: &commonpb.ResourceMetadata{Id: rid}, Spec: recSpec("www", dnspb.RecordSpec_A, "not-an-ip")})
	kmsWantCode(t, kmsWait(ctx, op, err), codes.InvalidArgument)
	e.createRecord(t, zid, "www-b", "www", dnspb.RecordSpec_A, "192.0.2.77")
	op, err = e.records.Update(ctx, &dnspb.UpdateRecordRequest{Metadata: &commonpb.ResourceMetadata{Id: rid}, Spec: recSpec("www", dnspb.RecordSpec_A, "192.0.2.77")})
	kmsWantCode(t, kmsWait(ctx, op, err), codes.AlreadyExists)

	e.deleteRecord(t, rid)
	_, err = e.records.Get(ctx, &dnspb.GetRecordRequest{Id: rid})
	kmsWantCode(t, err, codes.NotFound)
	op, err = e.records.Delete(ctx, &dnspb.DeleteRecordRequest{Id: rid})
	kmsWantCode(t, kmsWait(ctx, op, err), codes.NotFound)
}

func TestDNSRecordList(t *testing.T) {
	t.Parallel()
	e := startDNS(t)
	ctx := t.Context()
	zid := e.createZone(t, "list", "list.example.").GetMetadata().GetId()
	for i := range 5 {
		e.createRecord(t, zid, fmt.Sprintf("a-%d", i), fmt.Sprintf("h%d", i), dnspb.RecordSpec_A, fmt.Sprintf("192.0.2.%d", i+1))
	}

	types := map[dnspb.RecordSpec_RecordType]int{}
	token, pages, total := "", 0, 0
	for {
		page, err := e.records.List(ctx, &dnspb.ListRecordsRequest{ParentId: zid, PageSize: 3, PageToken: token})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(page.GetItems()) > 3 {
			t.Fatalf("page has %d items", len(page.GetItems()))
		}
		for _, r := range page.GetItems() {
			types[r.GetSpec().GetType()]++
			total++
		}
		pages++
		if token = page.GetNextPageToken(); token == "" {
			break
		}
	}
	// 5 A records plus the service-managed SOA and NS.
	if total != 7 || pages != 3 || types[dnspb.RecordSpec_SOA] != 1 || types[dnspb.RecordSpec_NS] != 1 || types[dnspb.RecordSpec_A] != 5 {
		t.Errorf("total = %d, pages = %d, types = %v", total, pages, types)
	}
	if got := e.getZone(t, zid).GetStatus().GetRecordCount(); got != 7 {
		t.Errorf("zone record_count = %d, want 7", got)
	}
}

func TestDNSRecordCreateValidation(t *testing.T) {
	t.Parallel()
	e := startDNS(t)
	zid := e.createZone(t, "val", "my.example.").GetMetadata().GetId()

	valid := []struct {
		name string
		typ  dnspb.RecordSpec_RecordType
		data string
	}{
		{"a", dnspb.RecordSpec_A, "192.0.2.1"},
		{"mx", dnspb.RecordSpec_MX, "10 mail.my.example."},
		{"txt", dnspb.RecordSpec_TXT, `"v=spf1 -all"`},
		{"txt-multi", dnspb.RecordSpec_TXT, `"part one" "part two"`},
		{"srv", dnspb.RecordSpec_SRV, "10 5 5060 sip.my.example."},
		{"caa", dnspb.RecordSpec_CAA, `0 issue "letsencrypt.org"`},
		{"caa-iodef", dnspb.RecordSpec_CAA, `0 iodef "mailto:sec@my.example"`},
		{"svcb", dnspb.RecordSpec_SVCB, "1 svc.my.example. alpn=h2"},
		{"https", dnspb.RecordSpec_HTTPS, "1 web.my.example. alpn=h2,h3"},
	}
	for _, tc := range valid {
		t.Run("valid/"+tc.name, func(t *testing.T) {
			t.Parallel()
			id, err := e.tryRecord(t.Context(), zid, "", recSpec("v-"+tc.name, tc.typ, tc.data))
			if err != nil {
				t.Fatalf("create %s %q: %v", tc.typ, tc.data, err)
			}
			if got := e.getRecord(t, id).GetSpec(); got.GetType() != tc.typ || got.GetData() != tc.data {
				t.Errorf("round trip = %v, want %s %q", got, tc.typ, tc.data)
			}
		})
	}

	invalid := []struct {
		name    string
		typ     dnspb.RecordSpec_RecordType
		data    string
		message string // text the error must contain
	}{
		{"cname", dnspb.RecordSpec_CNAME, "target.my.example.", "CNAME"},
		{"aaaa", dnspb.RecordSpec_AAAA, "2001:db8::1", "AAAA"},
		{"ptr", dnspb.RecordSpec_PTR, "host.my.example.", "PTR"},
		{"ns", dnspb.RecordSpec_NS, "ns1.my.example.", "NS"},
		{"soa", dnspb.RecordSpec_SOA, "ns.internal. h.internal. 1 1 1 1 1", "SOA"},
		{"unspecified type", dnspb.RecordSpec_RECORD_TYPE_UNSPECIFIED, "192.0.2.1", ""},
		{"bad a", dnspb.RecordSpec_A, "999.1.1.1", ""},
		{"a with an ipv6", dnspb.RecordSpec_A, "2001:db8::1", ""},
		{"empty data", dnspb.RecordSpec_A, "", ""},
		{"mx unqualified target", dnspb.RecordSpec_MX, "10 mail", "fully qualified"},
		{"mx without a priority", dnspb.RecordSpec_MX, "mail.my.example.", ""},
		{"srv unqualified target", dnspb.RecordSpec_SRV, "10 5 5060 sip", "fully qualified"},
		{"svcb dot target", dnspb.RecordSpec_SVCB, "1 .", "'.'"},
		{"https dot target", dnspb.RecordSpec_HTTPS, "1 . alpn=h2", "'.'"},
		{"txt unquoted", dnspb.RecordSpec_TXT, "v=spf1 -all", "quoted"},
		{"txt string of 256 chars", dnspb.RecordSpec_TXT, `"` + strings.Repeat("a", 256) + `"`, ""},
		{"txt empty string", dnspb.RecordSpec_TXT, `""`, ""},
		{"caa bad tag", dnspb.RecordSpec_CAA, `0 contact "x@my.example"`, "issue, issuewild or iodef"},
		{"comment", dnspb.RecordSpec_A, "192.0.2.1 ; note", "';'"},
		{"multi line", dnspb.RecordSpec_TXT, "\"a\"\n\"b\"", "single line"},
	}
	for _, tc := range invalid {
		t.Run("invalid/"+tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := e.tryRecord(t.Context(), zid, "", recSpec("bad", tc.typ, tc.data))
			kmsWantCode(t, err, codes.InvalidArgument)
			if !strings.Contains(err.Error(), tc.message) {
				t.Errorf("error %q does not mention %q", err, tc.message)
			}
		})
	}

	t.Run("invalid/unknown zone", func(t *testing.T) {
		t.Parallel()
		_, err := e.tryRecord(t.Context(), "dnszone-doesnotexist", "", recSpec("x", dnspb.RecordSpec_A, "192.0.2.1"))
		kmsWantCode(t, err, codes.NotFound)
	})
	t.Run("invalid/long name", func(t *testing.T) {
		t.Parallel()
		_, err := e.tryRecord(t.Context(), zid, "", recSpec(strings.Repeat("a", 64), dnspb.RecordSpec_A, "192.0.2.1"))
		kmsWantCode(t, err, codes.InvalidArgument)
	})
}

func TestDNSRecordDuplicatesAndLimits(t *testing.T) {
	t.Parallel()
	e := startDNS(t)
	ctx := t.Context()
	zid := e.createZone(t, "lim", "lim.example.").GetMetadata().GetId()

	e.createRecord(t, zid, "first", "dup", dnspb.RecordSpec_A, "192.0.2.1")
	_, err := e.tryRecord(ctx, zid, "", recSpec("dup", dnspb.RecordSpec_A, "192.0.2.1"))
	kmsWantCode(t, err, codes.AlreadyExists)
	// A different type or name with the same data is fine.
	e.createRecord(t, zid, "", "dup2", dnspb.RecordSpec_A, "192.0.2.1")
	// Same name twice in the zone.
	_, err = e.tryRecord(ctx, zid, "first", recSpec("other", dnspb.RecordSpec_A, "192.0.2.2"))
	kmsWantCode(t, err, codes.AlreadyExists)

	// A name+type set holds 20 records.
	for i := 2; i <= 20; i++ {
		if _, err := e.tryRecord(ctx, zid, "", recSpec("dup", dnspb.RecordSpec_A, fmt.Sprintf("192.0.2.%d", i+10))); err != nil {
			t.Fatalf("record %d of the set: %v", i, err)
		}
	}
	_, err = e.tryRecord(ctx, zid, "", recSpec("dup", dnspb.RecordSpec_A, "192.0.2.200"))
	kmsWantCode(t, err, codes.FailedPrecondition)
	// Another type at the same name has its own set.
	if _, err := e.tryRecord(ctx, zid, "", recSpec("dup", dnspb.RecordSpec_TXT, `"ok"`)); err != nil {
		t.Errorf("TXT at the full A name: %v", err)
	}
}

func TestDNSRecordSystemRecordsAreProtected(t *testing.T) {
	t.Parallel()
	e := startDNS(t)
	ctx := t.Context()
	zid := e.createZone(t, "sys", "sys.example.").GetMetadata().GetId()
	list, err := e.records.List(ctx, &dnspb.ListRecordsRequest{ParentId: zid})
	if err != nil || len(list.GetItems()) != 2 {
		t.Fatalf("List = %v, %v", list, err)
	}
	for _, r := range list.GetItems() {
		typ := r.GetSpec().GetType()
		if typ != dnspb.RecordSpec_SOA && typ != dnspb.RecordSpec_NS {
			t.Fatalf("unexpected record %v", r)
		}
		if r.GetSpec().GetRelativeName() != "@" || r.GetSpec().GetTtl() != 3600 {
			t.Errorf("%s spec = %v", typ, r.GetSpec())
		}
		op, err := e.records.Update(ctx, &dnspb.UpdateRecordRequest{
			Metadata: &commonpb.ResourceMetadata{Id: r.GetMetadata().GetId()},
			Spec:     recSpec("@", typ, r.GetSpec().GetData()),
		})
		kmsWantCode(t, kmsWait(ctx, op, err), codes.FailedPrecondition)
		op, err = e.records.Delete(ctx, &dnspb.DeleteRecordRequest{Id: r.GetMetadata().GetId(), Force: true})
		kmsWantCode(t, kmsWait(ctx, op, err), codes.FailedPrecondition)
	}
}

func TestDNSRecordDeletionProtection(t *testing.T) {
	t.Parallel()
	e := startDNS(t)
	ctx := t.Context()
	zid := e.createZone(t, "prot", "prot.example.").GetMetadata().GetId()
	spec := recSpec("keep", dnspb.RecordSpec_A, "192.0.2.5")
	spec.DeletionProtection = true
	id, err := e.tryRecord(ctx, zid, "", spec)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !e.getRecord(t, id).GetSpec().GetDeletionProtection() {
		t.Fatal("deletion_protection not stored")
	}

	op, err := e.records.Delete(ctx, &dnspb.DeleteRecordRequest{Id: id})
	kmsWantCode(t, kmsWait(ctx, op, err), codes.FailedPrecondition)
	if _, err := e.records.Get(ctx, &dnspb.GetRecordRequest{Id: id}); err != nil {
		t.Fatalf("record gone after a refused delete: %v", err)
	}
	op, err = e.records.Delete(ctx, &dnspb.DeleteRecordRequest{Id: id, Force: true})
	kmsMustWait(t, op, err)
	_, err = e.records.Get(ctx, &dnspb.GetRecordRequest{Id: id})
	kmsWantCode(t, err, codes.NotFound)

	// Turning protection off through the reset mask makes a plain delete work.
	id, err = e.tryRecord(ctx, zid, "", spec)
	if err != nil {
		t.Fatalf("create again: %v", err)
	}
	op, err = e.records.Update(resetCtx(ctx, t, "spec.deletion_protection"), &dnspb.UpdateRecordRequest{
		Metadata: &commonpb.ResourceMetadata{Id: id},
		Spec:     recSpec("keep", dnspb.RecordSpec_A, "192.0.2.5"),
	})
	kmsMustWait(t, op, err)
	e.deleteRecord(t, id)
}

func TestDNSRecordTTL(t *testing.T) {
	t.Parallel()
	e := startDNS(t)
	ctx := t.Context()
	zid := e.createZone(t, "ttl", "ttl.example.").GetMetadata().GetId()

	r := e.createRecord(t, zid, "", "a", dnspb.RecordSpec_A, "192.0.2.1")
	if got := r.GetSpec().GetTtl(); got != 0 {
		t.Errorf("ttl = %d, want 0 as sent", got)
	}

	spec := recSpec("a", dnspb.RecordSpec_A, "192.0.2.1")
	spec.Ttl = 300
	op, err := e.records.Update(ctx, &dnspb.UpdateRecordRequest{Metadata: &commonpb.ResourceMetadata{Id: r.GetMetadata().GetId()}, Spec: spec})
	kmsMustWait(t, op, err)
	if got := e.getRecord(t, r.GetMetadata().GetId()).GetSpec().GetTtl(); got != 300 {
		t.Errorf("ttl = %d, want 300", got)
	}
	spec.Ttl = 0
	op, err = e.records.Update(resetCtx(ctx, t, "spec.ttl"), &dnspb.UpdateRecordRequest{Metadata: &commonpb.ResourceMetadata{Id: r.GetMetadata().GetId()}, Spec: spec})
	kmsMustWait(t, op, err)
	if got := e.getRecord(t, r.GetMetadata().GetId()).GetSpec().GetTtl(); got != 0 {
		t.Errorf("ttl after reset = %d, want 0", got)
	}
}

func TestDNSZoneRecordCountFollowsRecords(t *testing.T) {
	t.Parallel()
	e := startDNS(t)
	zid := e.createZone(t, "count", "count.example.").GetMetadata().GetId()
	count := func() int64 { return e.getZone(t, zid).GetStatus().GetRecordCount() }
	if count() != 2 {
		t.Fatalf("record_count = %d, want 2", count())
	}
	a := e.createRecord(t, zid, "", "a", dnspb.RecordSpec_A, "192.0.2.1")
	b := e.createRecord(t, zid, "", "b", dnspb.RecordSpec_A, "192.0.2.2")
	if count() != 4 {
		t.Errorf("record_count = %d, want 4", count())
	}
	e.deleteRecord(t, a.GetMetadata().GetId())
	if count() != 3 {
		t.Errorf("record_count = %d, want 3", count())
	}
	e.deleteRecord(t, b.GetMetadata().GetId())
	if count() != 2 {
		t.Errorf("record_count = %d, want 2", count())
	}
}
