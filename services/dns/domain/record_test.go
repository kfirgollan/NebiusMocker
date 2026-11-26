package domain_test

import (
	"fmt"
	"strings"
	"testing"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	dnspb "github.com/nebius/gosdk/proto/nebius/dns/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/services/dns/domain"
)

func TestCreateRecordTypeAndData(t *testing.T) {
	t.Parallel()
	const (
		a     = dnspb.RecordSpec_A
		mx    = dnspb.RecordSpec_MX
		txt   = dnspb.RecordSpec_TXT
		srv   = dnspb.RecordSpec_SRV
		caa   = dnspb.RecordSpec_CAA
		svcb  = dnspb.RecordSpec_SVCB
		https = dnspb.RecordSpec_HTTPS
	)
	bad, ok := errs.KindBadRequest, errs.KindUnknown
	tests := []struct {
		name string
		typ  dnspb.RecordSpec_RecordType
		data string
		want errs.Kind
	}{
		{"A valid", a, "10.0.0.1", ok},
		{"A not an address", a, "not-an-ip", bad},
		{"A ipv6", a, "2001:db8::1", bad},
		{"A empty", a, "", bad},
		{"A blank", a, "   ", bad},
		{"A multi line", a, "10.0.0.1\n10.0.0.2", bad},
		{"A comment", a, "10.0.0.1 ; note", bad},
		{"MX valid", mx, "10 mail.my.example.", ok},
		{"MX relative target", mx, "10 mail", bad},
		{"MX root target", mx, "10 .", bad},
		{"MX no preference", mx, "mail.my.example.", bad},
		{"TXT quoted", txt, `"v=spf1 -all"`, ok},
		{"TXT several strings", txt, `"abc" "def"`, ok},
		{"TXT unquoted", txt, "v=spf1", bad},
		{"TXT empty string", txt, `""`, bad},
		{"TXT string of 255", txt, `"` + strings.Repeat("a", 255) + `"`, ok},
		{"TXT string of 256", txt, `"` + strings.Repeat("a", 256) + `"`, bad},
		{"TXT semicolon inside quotes", txt, `"a;b"`, ok},
		{"TXT semicolon outside quotes", txt, `"a" ; b`, bad},
		{"SRV valid", srv, "10 5 8080 svc.my.example.", ok},
		{"SRV relative target", srv, "10 5 8080 svc", bad},
		{"SRV root target", srv, "10 5 8080 .", bad},
		{"SRV missing fields", srv, "10 5", bad},
		{"CAA issue", caa, `0 issue "letsencrypt.org"`, ok},
		{"CAA issuewild", caa, `0 issuewild ";"`, ok},
		{"CAA iodef", caa, `0 iodef "mailto:sec@example.com"`, ok},
		{"CAA other tag", caa, `0 contactemail "a@b.c"`, bad},
		{"SVCB valid", svcb, "1 svc.my.example. alpn=h2", ok},
		{"SVCB relative target", svcb, "1 svc alpn=h2", bad},
		{"HTTPS valid", https, "1 svc.my.example. alpn=h2", ok},
		{"HTTPS root target", https, "0 .", bad},
		{"unspecified type", dnspb.RecordSpec_RECORD_TYPE_UNSPECIFIED, "10.0.0.1", bad},
		{"CNAME", dnspb.RecordSpec_CNAME, "x.my.example.", bad},
		{"AAAA", dnspb.RecordSpec_AAAA, "2001:db8::1", bad},
		{"PTR", dnspb.RecordSpec_PTR, "x.my.example.", bad},
		{"NS", dnspb.RecordSpec_NS, "ns.internal.", bad},
		{"SOA", dnspb.RecordSpec_SOA, "ns.internal. h.nebius.internal. 1 1 1 1 1", bad},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			zone := e.newZone(t, "my.example.")
			before := len(e.recordsOf(t, zone))
			_, err := e.createRecord(t.Context(), zone, "", &dnspb.RecordSpec{RelativeName: "www", Type: tt.typ, Data: tt.data})
			wantKind(t, fmt.Sprintf("CreateRecord(%s %q)", tt.typ, tt.data), err, tt.want)
			wantN := before
			if tt.want == errs.KindUnknown {
				wantN++
			}
			if n := len(e.recordsOf(t, zone)); n != wantN {
				t.Errorf("records = %d, want %d", n, wantN)
			}
		})
	}
}

func TestCreateRecordTypeErrorNamesType(t *testing.T) {
	t.Parallel()
	for typ, want := range map[dnspb.RecordSpec_RecordType]string{
		dnspb.RecordSpec_CNAME: "CNAME", dnspb.RecordSpec_AAAA: "AAAA", dnspb.RecordSpec_PTR: "PTR",
		dnspb.RecordSpec_NS: "NS", dnspb.RecordSpec_SOA: "SOA",
	} {
		e := newEnv(t)
		zone := e.newZone(t, "my.example.")
		_, err := e.createRecord(t.Context(), zone, "", &dnspb.RecordSpec{RelativeName: "www", Type: typ, Data: "x"})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("CreateRecord(%s) error = %v, want it to name %s", typ, err, want)
		}
	}
}

func TestCreateRecordParentAndMetadata(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	spec := &dnspb.RecordSpec{RelativeName: "www", Type: dnspb.RecordSpec_A, Data: "10.0.0.1"}
	_, err := e.svc.CreateRecord(t.Context(), domain.CreateRecordCmd{Metadata: &commonpb.ResourceMetadata{}, Spec: spec})
	wantKind(t, "CreateRecord without parent", err, errs.KindBadRequest)
	_, err = e.createRecord(t.Context(), "dnszone-nope", "", spec)
	wantKind(t, "CreateRecord in a missing zone", err, errs.KindNotFound)
	zone := e.newZone(t, "my.example.")
	_, err = e.svc.CreateRecord(t.Context(), domain.CreateRecordCmd{Metadata: &commonpb.ResourceMetadata{ParentId: zone, Id: "dnsrecord-x"}, Spec: spec})
	wantKind(t, "CreateRecord with metadata.id", err, errs.KindBadRequest)
}

func TestCreateRecordStatusAndNames(t *testing.T) {
	t.Parallel()
	tests := []struct {
		rel      string
		wantFQDN string
	}{
		{"www", "www.my.example."},
		{"@", "my.example."},
		{"*", "*.my.example."},
		{"a.b", "a.b.my.example."},
		{"*.sub", "*.sub.my.example."},
	}
	for _, tt := range tests {
		t.Run(tt.rel, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			zone := e.newZone(t, "my.example.")
			id := e.newRecord(t, zone, tt.rel, dnspb.RecordSpec_A, "10.0.0.1")
			r, err := e.svc.GetRecord(t.Context(), id, 0)
			if err != nil {
				t.Fatalf("GetRecord: %v", err)
			}
			st := r.GetStatus()
			if st.GetEffectiveFqdn() != tt.wantFQDN || st.GetZoneDomainName() != "my.example." {
				t.Errorf("status = %v, want fqdn %s in zone my.example.", st, tt.wantFQDN)
			}
			if got := domain.EffectiveFQDN(tt.rel, "my.example."); got != tt.wantFQDN {
				t.Errorf("EffectiveFQDN(%q) = %q, want %q", tt.rel, got, tt.wantFQDN)
			}
			if r.GetMetadata().GetResourceVersion() != 1 {
				t.Errorf("resource_version = %d, want 1", r.GetMetadata().GetResourceVersion())
			}
		})
	}
}

func TestCreateRecordFQDNTooLong(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	zone := e.newZone(t, "my.example.")
	rel := strings.Repeat(fmt.Sprintf("%063d.", 0), 5)
	_, err := e.createRecord(t.Context(), zone, "", &dnspb.RecordSpec{RelativeName: rel[:len(rel)-1], Type: dnspb.RecordSpec_A, Data: "10.0.0.1"})
	wantKind(t, "CreateRecord with a 330 character name", err, errs.KindBadRequest)
}

func TestCreateRecordDuplicates(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		first *dnspb.RecordSpec
		next  *dnspb.RecordSpec
		want  errs.Kind
	}{
		{
			name:  "exact duplicate",
			first: &dnspb.RecordSpec{RelativeName: "www", Type: dnspb.RecordSpec_A, Data: "10.0.0.1"},
			next:  &dnspb.RecordSpec{RelativeName: "www", Type: dnspb.RecordSpec_A, Data: "10.0.0.1"},
			want:  errs.KindAlreadyExists,
		},
		{
			name:  "duplicate differing only in ttl",
			first: &dnspb.RecordSpec{RelativeName: "www", Type: dnspb.RecordSpec_A, Data: "10.0.0.1", Ttl: 60},
			next:  &dnspb.RecordSpec{RelativeName: "www", Type: dnspb.RecordSpec_A, Data: "10.0.0.1", Ttl: 300},
			want:  errs.KindAlreadyExists,
		},
		{
			name:  "canonical MX whitespace",
			first: &dnspb.RecordSpec{RelativeName: "@", Type: dnspb.RecordSpec_MX, Data: "10  mail.my.example."},
			next:  &dnspb.RecordSpec{RelativeName: "@", Type: dnspb.RecordSpec_MX, Data: "10 mail.my.example."},
			want:  errs.KindAlreadyExists,
		},
		{
			name:  "different data",
			first: &dnspb.RecordSpec{RelativeName: "www", Type: dnspb.RecordSpec_A, Data: "10.0.0.1"},
			next:  &dnspb.RecordSpec{RelativeName: "www", Type: dnspb.RecordSpec_A, Data: "10.0.0.2"},
		},
		{
			name:  "different name",
			first: &dnspb.RecordSpec{RelativeName: "www", Type: dnspb.RecordSpec_A, Data: "10.0.0.1"},
			next:  &dnspb.RecordSpec{RelativeName: "api", Type: dnspb.RecordSpec_A, Data: "10.0.0.1"},
		},
		{
			name:  "different type same name",
			first: &dnspb.RecordSpec{RelativeName: "www", Type: dnspb.RecordSpec_TXT, Data: `"10.0.0.1"`},
			next:  &dnspb.RecordSpec{RelativeName: "www", Type: dnspb.RecordSpec_A, Data: "10.0.0.1"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			zone := e.newZone(t, "my.example.")
			done(t)(e.createRecord(t.Context(), zone, "", tt.first))
			_, err := e.createRecord(t.Context(), zone, "", tt.next)
			wantKind(t, "second CreateRecord", err, tt.want)
		})
	}
}

func TestCreateRecordLimits(t *testing.T) {
	t.Parallel()
	t.Run("per set", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t)
		zone := e.newZone(t, "my.example.")
		for i := range domain.MaxRecordsPerSet {
			e.newRecord(t, zone, "www", dnspb.RecordSpec_A, fmt.Sprintf("10.0.0.%d", i+1))
		}
		_, err := e.createRecord(t.Context(), zone, "", &dnspb.RecordSpec{RelativeName: "www", Type: dnspb.RecordSpec_A, Data: "10.0.1.1"})
		wantKind(t, "record past MaxRecordsPerSet", err, errs.KindBadState)
		// Another name or type is not affected by the set limit.
		e.newRecord(t, zone, "api", dnspb.RecordSpec_A, "10.0.1.1")
		e.newRecord(t, zone, "www", dnspb.RecordSpec_TXT, `"x"`)
	})
	t.Run("per zone", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t)
		zone := e.newZone(t, "my.example.")
		// The SOA and NS records count towards the zone limit.
		for i := len(e.recordsOf(t, zone)); i < domain.MaxRecordsPerZone; i++ {
			e.newRecord(t, zone, fmt.Sprintf("h%d", i), dnspb.RecordSpec_A, "10.0.0.1")
		}
		if n := len(e.recordsOf(t, zone)); n != domain.MaxRecordsPerZone {
			t.Fatalf("records = %d, want %d", n, domain.MaxRecordsPerZone)
		}
		_, err := e.createRecord(t.Context(), zone, "", &dnspb.RecordSpec{RelativeName: "extra", Type: dnspb.RecordSpec_A, Data: "10.0.0.1"})
		wantKind(t, "record past MaxRecordsPerZone", err, errs.KindBadState)
		// Another zone has its own budget.
		other := e.newZone(t, "other.example.")
		e.newRecord(t, other, "extra", dnspb.RecordSpec_A, "10.0.0.1")
	})
}

func TestCreateRecordNameUniqueness(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	zone := e.newZone(t, "my.example.")
	spec := func(data string) *dnspb.RecordSpec {
		return &dnspb.RecordSpec{RelativeName: "www", Type: dnspb.RecordSpec_A, Data: data}
	}
	done(t)(e.createRecord(t.Context(), zone, "first", spec("10.0.0.1")))
	_, err := e.createRecord(t.Context(), zone, "first", spec("10.0.0.2"))
	wantKind(t, "CreateRecord with a taken name", err, errs.KindAlreadyExists)
	// Nameless records never collide, and neither do the system records.
	e.newRecord(t, zone, "www", dnspb.RecordSpec_A, "10.0.0.3")
	e.newRecord(t, zone, "www", dnspb.RecordSpec_A, "10.0.0.4")
	r, err := e.svc.GetRecordByName(t.Context(), zone, "first")
	if err != nil || r.GetSpec().GetData() != "10.0.0.1" {
		t.Errorf("GetRecordByName(first) = %v, %v", r, err)
	}
	if _, err := e.svc.GetRecordByName(t.Context(), zone, "nope"); errs.KindOf(err) != errs.KindNotFound {
		t.Errorf("GetRecordByName(missing) kind = %v, want NotFound", errs.KindOf(err))
	}
}

func TestUpdateRecord(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		md       *commonpb.ResourceMetadata
		spec     *dnspb.RecordSpec
		wantKind errs.Kind
		wantVer  int64
		wantData string
		wantTTL  int64
	}{
		{name: "change data", md: &commonpb.ResourceMetadata{}, spec: &dnspb.RecordSpec{Data: "10.0.0.9", Ttl: 60}, wantVer: 2, wantData: "10.0.0.9", wantTTL: 60},
		{name: "change ttl only", md: &commonpb.ResourceMetadata{}, spec: &dnspb.RecordSpec{Data: "10.0.0.1", Ttl: 99}, wantVer: 2, wantData: "10.0.0.1", wantTTL: 99},
		{name: "no change", md: &commonpb.ResourceMetadata{}, spec: &dnspb.RecordSpec{Data: "10.0.0.1", Ttl: 60}, wantVer: 1, wantData: "10.0.0.1", wantTTL: 60},
		{
			name: "unchanged immutables accepted", md: &commonpb.ResourceMetadata{},
			spec:    &dnspb.RecordSpec{RelativeName: "www", Type: dnspb.RecordSpec_A, Data: "10.0.0.5", Ttl: 60},
			wantVer: 2, wantData: "10.0.0.5", wantTTL: 60,
		},
		{name: "relative name change", md: &commonpb.ResourceMetadata{}, spec: &dnspb.RecordSpec{RelativeName: "api", Data: "10.0.0.1"}, wantKind: errs.KindBadRequest},
		{name: "type change", md: &commonpb.ResourceMetadata{}, spec: &dnspb.RecordSpec{Type: dnspb.RecordSpec_TXT, Data: `"x"`}, wantKind: errs.KindBadRequest},
		{name: "invalid data", md: &commonpb.ResourceMetadata{}, spec: &dnspb.RecordSpec{Data: "nope"}, wantKind: errs.KindBadRequest},
		{name: "duplicate of another record", md: &commonpb.ResourceMetadata{}, spec: &dnspb.RecordSpec{Data: "10.0.0.2", Ttl: 60}, wantKind: errs.KindAlreadyExists},
		{name: "stale version", md: &commonpb.ResourceMetadata{ResourceVersion: 4}, spec: &dnspb.RecordSpec{Data: "10.0.0.9"}, wantKind: errs.KindBadState},
		{name: "parent change", md: &commonpb.ResourceMetadata{ParentId: "dnszone-other"}, spec: &dnspb.RecordSpec{Data: "10.0.0.9"}, wantKind: errs.KindBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			zone := e.newZone(t, "my.example.")
			id := done(t)(e.createRecord(t.Context(), zone, "", &dnspb.RecordSpec{RelativeName: "www", Type: dnspb.RecordSpec_A, Data: "10.0.0.1", Ttl: 60}))
			e.newRecord(t, zone, "www", dnspb.RecordSpec_A, "10.0.0.2")
			tt.md.Id = id
			_, err := e.svc.UpdateRecord(t.Context(), domain.UpdateRecordCmd{Metadata: tt.md, Spec: tt.spec})
			wantKind(t, "UpdateRecord", err, tt.wantKind)
			if tt.wantKind != errs.KindUnknown {
				return
			}
			r, err := e.svc.GetRecord(t.Context(), id, 0)
			if err != nil {
				t.Fatalf("GetRecord: %v", err)
			}
			if r.GetMetadata().GetResourceVersion() != tt.wantVer || r.GetSpec().GetData() != tt.wantData || r.GetSpec().GetTtl() != tt.wantTTL {
				t.Errorf("record = version %d data %q ttl %d, want %d %q %d",
					r.GetMetadata().GetResourceVersion(), r.GetSpec().GetData(), r.GetSpec().GetTtl(), tt.wantVer, tt.wantData, tt.wantTTL)
			}
			if r.GetSpec().GetRelativeName() != "www" || r.GetSpec().GetType() != dnspb.RecordSpec_A {
				t.Errorf("immutable spec changed: %v", r.GetSpec())
			}
		})
	}
}

func TestUpdateRecordDeletionProtectionAndName(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	zone := e.newZone(t, "my.example.")
	id := e.newRecord(t, zone, "www", dnspb.RecordSpec_A, "10.0.0.1")
	done(t)(e.svc.UpdateRecord(t.Context(), domain.UpdateRecordCmd{
		Metadata: &commonpb.ResourceMetadata{Id: id, Name: "web", Labels: map[string]string{"k": "v"}},
		Spec:     &dnspb.RecordSpec{Data: "10.0.0.1", DeletionProtection: true},
	}))
	r, err := e.svc.GetRecord(t.Context(), id, 0)
	if err != nil {
		t.Fatalf("GetRecord: %v", err)
	}
	if !r.GetSpec().GetDeletionProtection() || r.GetMetadata().GetName() != "web" || r.GetMetadata().GetLabels()["k"] != "v" {
		t.Errorf("record after update = %v, want protection, name web and label k=v", r)
	}
	_, err = e.svc.UpdateRecord(t.Context(), domain.UpdateRecordCmd{Metadata: &commonpb.ResourceMetadata{}, Spec: &dnspb.RecordSpec{}})
	wantKind(t, "UpdateRecord without id", err, errs.KindBadRequest)
	_, err = e.svc.UpdateRecord(t.Context(), domain.UpdateRecordCmd{Metadata: &commonpb.ResourceMetadata{Id: "dnsrecord-nope"}, Spec: &dnspb.RecordSpec{}})
	wantKind(t, "UpdateRecord(missing)", err, errs.KindNotFound)
}

func TestDeleteRecord(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		protect    bool
		force      bool
		version    int64
		wantKind   errs.Kind
		wantExists bool
	}{
		{name: "plain delete"},
		{name: "protected without force", protect: true, wantKind: errs.KindBadState, wantExists: true},
		{name: "protected with force", protect: true, force: true},
		{name: "unprotected with force", force: true},
		{name: "stale version", version: 9, wantKind: errs.KindBadState, wantExists: true},
		{name: "matching version", version: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			zone := e.newZone(t, "my.example.")
			id := done(t)(e.createRecord(t.Context(), zone, "", &dnspb.RecordSpec{
				RelativeName: "www", Type: dnspb.RecordSpec_A, Data: "10.0.0.1", DeletionProtection: tt.protect,
			}))
			_, err := e.svc.DeleteRecord(t.Context(), domain.DeleteRecordCmd{ID: id, ResourceVersion: tt.version, Force: tt.force})
			wantKind(t, "DeleteRecord", err, tt.wantKind)
			_, getErr := e.svc.GetRecord(t.Context(), id, 0)
			if exists := getErr == nil; exists != tt.wantExists {
				t.Errorf("record exists = %v, want %v", exists, tt.wantExists)
			}
		})
	}
	t.Run("missing", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t)
		_, err := e.svc.DeleteRecord(t.Context(), domain.DeleteRecordCmd{ID: "dnsrecord-nope"})
		wantKind(t, "DeleteRecord(missing)", err, errs.KindNotFound)
	})
}

func TestSystemRecordsAreReadOnly(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	zone := e.newZone(t, "my.example.")
	for _, r := range e.recordsOf(t, zone) {
		id, typ := r.GetMetadata().GetId(), r.GetSpec().GetType()
		_, err := e.svc.UpdateRecord(t.Context(), domain.UpdateRecordCmd{
			Metadata: &commonpb.ResourceMetadata{Id: id}, Spec: &dnspb.RecordSpec{Data: "changed.", Ttl: 5},
		})
		wantKind(t, fmt.Sprintf("UpdateRecord(%s)", typ), err, errs.KindBadState)
		_, err = e.svc.DeleteRecord(t.Context(), domain.DeleteRecordCmd{ID: id, Force: true})
		wantKind(t, fmt.Sprintf("DeleteRecord(%s)", typ), err, errs.KindBadState)
		if !domain.IsSystemType(typ) {
			t.Errorf("IsSystemType(%s) = false, want true", typ)
		}
	}
	if domain.IsSystemType(dnspb.RecordSpec_A) {
		t.Error("IsSystemType(A) = true, want false")
	}
	if n := len(e.recordsOf(t, zone)); n != 2 {
		t.Errorf("records = %d, want the 2 system records untouched", n)
	}
}

func TestListRecords(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	zone := e.newZone(t, "my.example.")
	e.newRecord(t, zone, "www", dnspb.RecordSpec_A, "10.0.0.1")
	page, err := e.svc.ListRecords(t.Context(), zone, 0, "")
	if err != nil || len(page.Items) != 3 {
		t.Fatalf("ListRecords = %v, %v, want SOA, NS and the A record", page, err)
	}
	if _, err := e.svc.ListRecords(t.Context(), "dnszone-nope", 0, ""); errs.KindOf(err) != errs.KindNotFound {
		t.Errorf("ListRecords(missing zone) kind = %v, want NotFound", errs.KindOf(err))
	}
	if _, err := e.svc.ListRecords(t.Context(), zone, 5000, ""); errs.KindOf(err) != errs.KindBadRequest {
		t.Errorf("ListRecords(page_size 5000) kind = %v, want BadRequest", errs.KindOf(err))
	}
	if _, err := e.svc.GetRecord(t.Context(), "dnsrecord-nope", 0); errs.KindOf(err) != errs.KindNotFound {
		t.Errorf("GetRecord(missing) kind = %v, want NotFound", errs.KindOf(err))
	}
	id := page.Items[2].GetMetadata().GetId()
	if _, err := e.svc.GetRecord(t.Context(), id, 8); errs.KindOf(err) != errs.KindBadState {
		t.Errorf("GetRecord(wrong version) kind = %v, want BadState", errs.KindOf(err))
	}
}

func TestEffectiveTTL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   int64
		want uint32
	}{
		{0, 600}, {-5, 600}, {1, 1}, {300, 300}, {1 << 31, 1<<31 - 1}, {1 << 40, 1<<31 - 1},
	}
	for _, tt := range tests {
		if got := domain.EffectiveTTL(tt.in); got != tt.want {
			t.Errorf("EffectiveTTL(%d) = %d, want %d", tt.in, got, tt.want)
		}
	}
}
