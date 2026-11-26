package domain_test

import (
	"strings"
	"testing"
	"time"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	dnspb "github.com/nebius/gosdk/proto/nebius/dns/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/services/dns/domain"
)

func TestCreateZoneValidation(t *testing.T) {
	t.Parallel()
	label63 := strings.Repeat("a", 63)
	tests := []struct {
		name     string
		parent   string
		domain   string
		network  string
		wantKind errs.Kind
	}{
		{name: "valid", parent: projectID, domain: "my.example.", network: networkID},
		{name: "valid 63 char label", parent: projectID, domain: label63 + ".example.", network: networkID},
		{name: "parent in hierarchy", parent: otherProjectID, domain: "my.example.", network: foreignNetwork},
		{name: "no parent", parent: "", domain: "my.example.", network: networkID, wantKind: errs.KindBadRequest},
		{name: "parent not a project", parent: "tenant-e00mock000000000default", domain: "my.example.", network: networkID, wantKind: errs.KindBadRequest},
		{name: "unknown project", parent: "project-e00unknown", domain: "my.example.", network: networkID, wantKind: errs.KindNotFound},
		{name: "single label", parent: projectID, domain: "example.", network: networkID, wantKind: errs.KindBadRequest},
		{name: "no trailing dot", parent: projectID, domain: "my.example", network: networkID, wantKind: errs.KindBadRequest},
		{name: "empty label", parent: projectID, domain: "my..example.", network: networkID, wantKind: errs.KindBadRequest},
		{name: "label too long", parent: projectID, domain: label63 + "a.example.", network: networkID, wantKind: errs.KindBadRequest},
		{name: "reserved ns.internal", parent: projectID, domain: "ns.internal.", network: networkID, wantKind: errs.KindPermissionDenied},
		{name: "reserved compute.internal", parent: projectID, domain: "compute.internal.", network: networkID, wantKind: errs.KindPermissionDenied},
		{name: "reserved nebius.internal", parent: projectID, domain: "nebius.internal.", network: networkID, wantKind: errs.KindPermissionDenied},
		{name: "reserved console.nebius", parent: projectID, domain: "console.nebius.", network: networkID, wantKind: errs.KindPermissionDenied},
		{name: "reserved example.invalid", parent: projectID, domain: "example.invalid.", network: networkID, wantKind: errs.KindPermissionDenied},
		{name: "subdomain of reserved", parent: projectID, domain: "a.b.compute.internal.", network: networkID, wantKind: errs.KindPermissionDenied},
		{name: "other internal allowed", parent: projectID, domain: "corp.internal.", network: networkID},
		{name: "bare internal is single label", parent: projectID, domain: "internal.", network: networkID, wantKind: errs.KindBadRequest},
		{name: "no network", parent: projectID, domain: "my.example.", network: "", wantKind: errs.KindBadRequest},
		{name: "not a network id", parent: projectID, domain: "my.example.", network: "subnet-e00x", wantKind: errs.KindBadRequest},
		{name: "network missing", parent: projectID, domain: "my.example.", network: "vpcnetwork-e00missing", wantKind: errs.KindNotFound},
		{name: "network in another project", parent: projectID, domain: "my.example.", network: foreignNetwork, wantKind: errs.KindBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			op, err := e.svc.CreateZone(t.Context(), domain.CreateZoneCmd{
				Metadata: &commonpb.ResourceMetadata{ParentId: tt.parent}, Spec: zoneSpec(tt.domain, tt.network),
			})
			wantKind(t, "CreateZone", err, tt.wantKind)
			if tt.wantKind != errs.KindUnknown {
				if len(e.zones.items) != 0 || len(e.networks.bound) != 0 {
					t.Errorf("failed create left %d zones and %d bindings", len(e.zones.items), len(e.networks.bound))
				}
				return
			}
			if op.Err != nil {
				t.Fatalf("operation failed: %v", op.Err)
			}
		})
	}
}

func TestCreateZoneSystemRecords(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		neg     int64
		wantSOA string
	}{
		{name: "default negative ttl", wantSOA: "ns.internal. hostmaster.nebius.internal. 1 3600 600 604800 900"},
		{name: "custom negative ttl", neg: 60, wantSOA: "ns.internal. hostmaster.nebius.internal. 1 3600 600 604800 60"},
		{name: "below minimum ignored", neg: 4, wantSOA: "ns.internal. hostmaster.nebius.internal. 1 3600 600 604800 900"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			id := done(t)(e.createZone(t.Context(), "", "my.example.", networkID, tt.neg))
			recs := e.recordsOf(t, id)
			if len(recs) != 2 {
				t.Fatalf("zone has %d records, want SOA and NS", len(recs))
			}
			byType := map[dnspb.RecordSpec_RecordType]*dnspb.Record{}
			for _, r := range recs {
				byType[r.GetSpec().GetType()] = r
			}
			soa, ns := byType[dnspb.RecordSpec_SOA], byType[dnspb.RecordSpec_NS]
			if soa == nil || ns == nil {
				t.Fatalf("records = %v, want one SOA and one NS", recs)
			}
			if got := soa.GetSpec().GetData(); got != tt.wantSOA {
				t.Errorf("SOA data = %q, want %q", got, tt.wantSOA)
			}
			if got := ns.GetSpec().GetData(); got != "ns.internal." {
				t.Errorf("NS data = %q, want ns.internal.", got)
			}
			for _, r := range recs {
				s := r.GetSpec()
				if s.GetRelativeName() != "@" || s.GetTtl() != 3600 || r.GetMetadata().GetName() != "" {
					t.Errorf("system record spec = %v name %q, want @ ttl 3600 nameless", s, r.GetMetadata().GetName())
				}
				if st := r.GetStatus(); st.GetEffectiveFqdn() != "my.example." || st.GetZoneDomainName() != "my.example." {
					t.Errorf("system record status = %v, want fqdn and zone my.example.", st)
				}
			}
			z, err := e.svc.GetZone(t.Context(), id, 0)
			if err != nil {
				t.Fatalf("GetZone: %v", err)
			}
			if z.GetStatus().GetRecordCount() != 2 {
				t.Errorf("record_count = %d, want 2", z.GetStatus().GetRecordCount())
			}
			if e.networks.binds != 1 || !e.networks.bound[networkID+"|"+id] {
				t.Errorf("binds = %d, bound = %v, want zone bound to its network once", e.networks.binds, e.networks.bound)
			}
		})
	}
}

func TestCreateZoneRejectsServerFields(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	_, err := e.svc.CreateZone(t.Context(), domain.CreateZoneCmd{
		Metadata: &commonpb.ResourceMetadata{ParentId: projectID, Id: "dnszone-x"}, Spec: zoneSpec("my.example.", networkID),
	})
	wantKind(t, "CreateZone with metadata.id", err, errs.KindBadRequest)
	_, err = e.svc.CreateZone(t.Context(), domain.CreateZoneCmd{
		Metadata: &commonpb.ResourceMetadata{ParentId: projectID, ResourceVersion: 3}, Spec: zoneSpec("my.example.", networkID),
	})
	wantKind(t, "CreateZone with resource_version", err, errs.KindBadRequest)
}

func TestCreateZoneDuplicates(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		second   string
		network  string
		wantKind errs.Kind
	}{
		{name: "same domain same network", second: "my.example.", network: networkID, wantKind: errs.KindAlreadyExists},
		{name: "same domain other network", second: "my.example.", network: network2ID},
		{name: "nested domain same network", second: "sub.my.example.", network: networkID},
		{name: "parent domain same network", second: "example.com.", network: networkID},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			e.newZone(t, "my.example.")
			_, err := e.createZone(t.Context(), "", tt.second, tt.network, 0)
			wantKind(t, "second CreateZone", err, tt.wantKind)
			if tt.wantKind != errs.KindUnknown {
				if n := len(e.zones.items); n != 1 {
					t.Errorf("zones = %d, want 1", n)
				}
			}
		})
	}
}

func TestCreateZoneFailureUnbinds(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	first := done(t)(e.createZone(t.Context(), "taken", "a.example.", networkID, 0))
	// A different domain in the same project with the same resource name: the
	// binding was made before the store refused the zone and must be released.
	_, err := e.createZone(t.Context(), "taken", "b.example.", networkID, 0)
	wantKind(t, "CreateZone with a taken name", err, errs.KindAlreadyExists)
	if e.networks.binds != 2 || e.networks.unbinds != 1 {
		t.Errorf("binds = %d, unbinds = %d, want 2 and 1", e.networks.binds, e.networks.unbinds)
	}
	if len(e.networks.bound) != 1 || !e.networks.bound[networkID+"|"+first] {
		t.Errorf("bound = %v, want only the first zone", e.networks.bound)
	}
}

func TestGetListZones(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	a := done(t)(e.createZone(t.Context(), "alpha", "a.example.", networkID, 0))
	b := done(t)(e.createZone(t.Context(), "beta", "b.example.", network2ID, 0))
	e.newRecord(t, a, "www", dnspb.RecordSpec_A, "10.0.0.1")

	z, err := e.svc.GetZone(t.Context(), a, 0)
	if err != nil || z.GetStatus().GetRecordCount() != 3 {
		t.Fatalf("GetZone(a) = %v, %v, want record_count 3", z, err)
	}
	if _, err := e.svc.GetZone(t.Context(), "dnszone-nope", 0); errs.KindOf(err) != errs.KindNotFound {
		t.Errorf("GetZone(missing) kind = %v, want NotFound", errs.KindOf(err))
	}
	if _, err := e.svc.GetZone(t.Context(), a, 99); errs.KindOf(err) != errs.KindBadState {
		t.Errorf("GetZone(wrong version) kind = %v, want BadState", errs.KindOf(err))
	}
	if _, err := e.svc.GetZone(t.Context(), a, 1); err != nil {
		t.Errorf("GetZone(matching version): %v", err)
	}
	byName, err := e.svc.GetZoneByName(t.Context(), projectID, "beta")
	if err != nil || byName.GetMetadata().GetId() != b {
		t.Errorf("GetZoneByName(beta) = %v, %v, want %s", byName, err, b)
	}
	if _, err := e.svc.GetZoneByName(t.Context(), projectID, "gamma"); errs.KindOf(err) != errs.KindNotFound {
		t.Errorf("GetZoneByName(missing) kind = %v, want NotFound", errs.KindOf(err))
	}
	page, err := e.svc.ListZones(t.Context(), projectID, 0, "")
	if err != nil || len(page.Items) != 2 {
		t.Fatalf("ListZones = %v, %v, want 2 zones", page, err)
	}
	if _, err := e.svc.ListZones(t.Context(), projectID, 1001, ""); errs.KindOf(err) != errs.KindBadRequest {
		t.Errorf("ListZones(page_size 1001) kind = %v, want BadRequest", errs.KindOf(err))
	}
	if _, err := e.svc.ListZones(t.Context(), projectID, -1, ""); errs.KindOf(err) != errs.KindBadRequest {
		t.Errorf("ListZones(page_size -1) kind = %v, want BadRequest", errs.KindOf(err))
	}
}

func TestListZonesByNetworkPaginates(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.newZone(t, "a.example.")
	e.newZone(t, "b.example.")
	e.newZone(t, "c.example.")
	done(t)(e.createZone(t.Context(), "", "d.example.", network2ID, 0))

	var got []string
	token := ""
	for range 5 {
		page, err := e.svc.ListZonesByNetwork(t.Context(), networkID, 2, token)
		if err != nil {
			t.Fatalf("ListZonesByNetwork: %v", err)
		}
		for _, z := range page.Items {
			got = append(got, z.GetSpec().GetDomainName())
		}
		if token = page.NextPageToken; token == "" {
			break
		}
	}
	if want := "a.example. b.example. c.example."; strings.Join(got, " ") != want {
		t.Errorf("zones of network = %v, want %s", got, want)
	}
	if _, err := e.svc.ListZonesByNetwork(t.Context(), networkID, 2, "!!"); errs.KindOf(err) != errs.KindBadRequest {
		t.Errorf("ListZonesByNetwork(bad token) kind = %v, want BadRequest", errs.KindOf(err))
	}
}

func TestUpdateZone(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		md       *commonpb.ResourceMetadata
		spec     *dnspb.ZoneSpec
		wantKind errs.Kind
		wantVer  int64
		wantName string
		wantNeg  string // SOA minimum field expected afterwards
	}{
		{name: "rename", md: &commonpb.ResourceMetadata{Name: "renamed"}, spec: &dnspb.ZoneSpec{}, wantVer: 2, wantName: "renamed", wantNeg: "900"},
		{name: "negative ttl rewrites SOA", md: &commonpb.ResourceMetadata{}, spec: &dnspb.ZoneSpec{SoaSpec: &dnspb.SoaSpec{NegativeTtl: 120}}, wantVer: 2, wantNeg: "120"},
		{name: "no change keeps version", md: &commonpb.ResourceMetadata{}, spec: &dnspb.ZoneSpec{}, wantVer: 1, wantNeg: "900"},
		{
			name: "unchanged immutables accepted", md: &commonpb.ResourceMetadata{},
			spec:    zoneSpec("my.example.", networkID),
			wantVer: 1, wantNeg: "900",
		},
		{name: "domain change", md: &commonpb.ResourceMetadata{}, spec: &dnspb.ZoneSpec{DomainName: "other.example."}, wantKind: errs.KindBadRequest},
		{name: "network change", md: &commonpb.ResourceMetadata{}, spec: zoneSpec("my.example.", network2ID), wantKind: errs.KindBadRequest},
		{name: "parent change", md: &commonpb.ResourceMetadata{ParentId: otherProjectID}, spec: &dnspb.ZoneSpec{}, wantKind: errs.KindBadRequest},
		{name: "stale version", md: &commonpb.ResourceMetadata{ResourceVersion: 7}, spec: &dnspb.ZoneSpec{}, wantKind: errs.KindBadState},
		{name: "matching version", md: &commonpb.ResourceMetadata{ResourceVersion: 1, Name: "v"}, spec: &dnspb.ZoneSpec{}, wantVer: 2, wantName: "v", wantNeg: "900"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			id := e.newZone(t, "my.example.")
			e.clk.Advance(time.Second)
			tt.md.Id = id
			_, err := e.svc.UpdateZone(t.Context(), domain.UpdateZoneCmd{Metadata: tt.md, Spec: tt.spec})
			wantKind(t, "UpdateZone", err, tt.wantKind)
			if tt.wantKind != errs.KindUnknown {
				return
			}
			z, err := e.svc.GetZone(t.Context(), id, 0)
			if err != nil {
				t.Fatalf("GetZone: %v", err)
			}
			md := z.GetMetadata()
			if md.GetResourceVersion() != tt.wantVer || md.GetName() != tt.wantName {
				t.Errorf("version = %d, name = %q, want %d and %q", md.GetResourceVersion(), md.GetName(), tt.wantVer, tt.wantName)
			}
			if tt.wantVer == 1 && !md.GetUpdatedAt().AsTime().Equal(t0) {
				t.Errorf("updated_at = %v after a no-op update, want unchanged %v", md.GetUpdatedAt().AsTime(), t0)
			}
			if z.GetSpec().GetDomainName() != "my.example." || z.GetSpec().GetVpc().GetPrimaryNetworkId() != networkID {
				t.Errorf("immutable spec changed: %v", z.GetSpec())
			}
			for _, r := range e.recordsOf(t, id) {
				if r.GetSpec().GetType() == dnspb.RecordSpec_SOA && !strings.HasSuffix(r.GetSpec().GetData(), " "+tt.wantNeg) {
					t.Errorf("SOA data = %q, want minimum %s", r.GetSpec().GetData(), tt.wantNeg)
				}
			}
		})
	}
}

func TestUpdateZoneErrors(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	_, err := e.svc.UpdateZone(t.Context(), domain.UpdateZoneCmd{Metadata: &commonpb.ResourceMetadata{}, Spec: &dnspb.ZoneSpec{}})
	wantKind(t, "UpdateZone without id", err, errs.KindBadRequest)
	_, err = e.svc.UpdateZone(t.Context(), domain.UpdateZoneCmd{Metadata: &commonpb.ResourceMetadata{Id: "dnszone-nope"}, Spec: &dnspb.ZoneSpec{}})
	wantKind(t, "UpdateZone(missing)", err, errs.KindNotFound)

	done(t)(e.createZone(t.Context(), "one", "a.example.", networkID, 0))
	two := done(t)(e.createZone(t.Context(), "two", "b.example.", networkID, 0))
	_, err = e.svc.UpdateZone(t.Context(), domain.UpdateZoneCmd{Metadata: &commonpb.ResourceMetadata{Id: two, Name: "one"}, Spec: &dnspb.ZoneSpec{}})
	wantKind(t, "UpdateZone to a taken name", err, errs.KindAlreadyExists)
}

func TestDeleteZone(t *testing.T) {
	t.Parallel()
	t.Run("empty zone removes system records and binding", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t)
		id := e.newZone(t, "my.example.")
		done(t)(e.svc.DeleteZone(t.Context(), id, 0, nil))
		if len(e.zones.items) != 0 || len(e.records.items) != 0 {
			t.Errorf("zones = %d, records = %d after delete, want none", len(e.zones.items), len(e.records.items))
		}
		if len(e.networks.bound) != 0 || e.networks.unbinds != 1 {
			t.Errorf("bound = %v, unbinds = %d, want released once", e.networks.bound, e.networks.unbinds)
		}
	})
	t.Run("zone with records is refused", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t)
		id := e.newZone(t, "my.example.")
		rec := e.newRecord(t, id, "www", dnspb.RecordSpec_A, "10.0.0.1")
		_, err := e.svc.DeleteZone(t.Context(), id, 0, nil)
		wantKind(t, "DeleteZone with records", err, errs.KindBadState)
		if len(e.zones.items) != 1 || len(e.networks.bound) != 1 {
			t.Errorf("refused delete changed state: zones = %d, bound = %v", len(e.zones.items), e.networks.bound)
		}
		done(t)(e.svc.DeleteRecord(t.Context(), domain.DeleteRecordCmd{ID: rec}))
		done(t)(e.svc.DeleteZone(t.Context(), id, 0, nil))
	})
	t.Run("version mismatch", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t)
		id := e.newZone(t, "my.example.")
		_, err := e.svc.DeleteZone(t.Context(), id, 5, nil)
		wantKind(t, "DeleteZone(wrong version)", err, errs.KindBadState)
	})
	t.Run("missing", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t)
		_, err := e.svc.DeleteZone(t.Context(), "dnszone-nope", 0, nil)
		wantKind(t, "DeleteZone(missing)", err, errs.KindNotFound)
	})
}

func TestEffectiveNegativeTTL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		zone *dnspb.Zone
		want uint32
	}{
		{name: "no spec", zone: &dnspb.Zone{}, want: 900},
		{name: "unset", zone: &dnspb.Zone{Spec: &dnspb.ZoneSpec{}}, want: 900},
		{name: "below minimum", zone: &dnspb.Zone{Spec: &dnspb.ZoneSpec{SoaSpec: &dnspb.SoaSpec{NegativeTtl: 4}}}, want: 900},
		{name: "negative", zone: &dnspb.Zone{Spec: &dnspb.ZoneSpec{SoaSpec: &dnspb.SoaSpec{NegativeTtl: -1}}}, want: 900},
		{name: "minimum", zone: &dnspb.Zone{Spec: &dnspb.ZoneSpec{SoaSpec: &dnspb.SoaSpec{NegativeTtl: 5}}}, want: 5},
		{name: "custom", zone: &dnspb.Zone{Spec: &dnspb.ZoneSpec{SoaSpec: &dnspb.SoaSpec{NegativeTtl: 3600}}}, want: 3600},
		{name: "clamped to 2^31-1", zone: &dnspb.Zone{Spec: &dnspb.ZoneSpec{SoaSpec: &dnspb.SoaSpec{NegativeTtl: 1 << 40}}}, want: 1<<31 - 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := domain.EffectiveNegativeTTL(tt.zone); got != tt.want {
				t.Errorf("EffectiveNegativeTTL = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestFindZone(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	outer := e.newZone(t, "a.example.")
	inner := e.newZone(t, "b.a.example.")
	e.newRecord(t, inner, "www", dnspb.RecordSpec_A, "10.0.0.1")
	done(t)(e.createZone(t.Context(), "", "a.example.", network2ID, 0)) // same domain, newer

	tests := []struct {
		name   string
		query  string
		wantID string // "" = no zone
		recs   int
	}{
		{name: "apex", query: "a.example.", wantID: outer, recs: 2},
		{name: "child in outer zone", query: "x.a.example.", wantID: outer, recs: 2},
		{name: "longest match wins", query: "www.b.a.example.", wantID: inner, recs: 3},
		{name: "deep name under inner", query: "x.y.b.a.example.", wantID: inner, recs: 3},
		{name: "sibling label is not a suffix match", query: "xa.example.", wantID: ""},
		{name: "parent of the zone", query: "example.", wantID: ""},
		{name: "unrelated", query: "other.test.", wantID: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			zd, ok, err := e.svc.FindZone(t.Context(), tt.query)
			if err != nil {
				t.Fatalf("FindZone(%q): %v", tt.query, err)
			}
			if (tt.wantID != "") != ok {
				t.Fatalf("FindZone(%q) ok = %v, want %v", tt.query, ok, tt.wantID != "")
			}
			if !ok {
				return
			}
			if got := zd.Zone.GetMetadata().GetId(); got != tt.wantID {
				t.Errorf("FindZone(%q) zone = %s, want %s (oldest wins a tie)", tt.query, got, tt.wantID)
			}
			if len(zd.Records) != tt.recs {
				t.Errorf("FindZone(%q) records = %d, want %d", tt.query, len(zd.Records), tt.recs)
			}
		})
	}
}
