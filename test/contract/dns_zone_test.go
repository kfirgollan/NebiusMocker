package contract

import (
	"strings"
	"testing"

	"github.com/miekg/dns"
	"google.golang.org/grpc/codes"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	dnspb "github.com/nebius/gosdk/proto/nebius/dns/v1"
	vpcpb "github.com/nebius/gosdk/proto/nebius/vpc/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
)

// soaOf returns the data of the zone's apex SOA record, listed through the API.
func (e *dnsEnv) soaOf(t *testing.T, zoneID string) *dnspb.Record {
	t.Helper()
	list, err := e.records.List(t.Context(), &dnspb.ListRecordsRequest{ParentId: zoneID})
	if err != nil {
		t.Fatalf("list records: %v", err)
	}
	for _, r := range list.GetItems() {
		if r.GetSpec().GetType() == dnspb.RecordSpec_SOA {
			return r
		}
	}
	t.Fatalf("zone %s has no SOA record", zoneID)
	return nil
}

func TestDNSZoneCRUD(t *testing.T) {
	t.Parallel()
	e := startDNS(t)
	ctx := t.Context()
	z := e.createZone(t, "main-zone", "my.example.")
	id := z.GetMetadata().GetId()

	if !strings.HasPrefix(id, "dnszone-") || z.GetMetadata().GetParentId() != e.inst.ProjectID {
		t.Errorf("metadata = %v", z.GetMetadata())
	}
	if z.GetSpec().GetDomainName() != "my.example." || z.GetSpec().GetVpc().GetPrimaryNetworkId() != e.netID {
		t.Errorf("spec = %v", z.GetSpec())
	}
	if got := z.GetStatus().GetRecordCount(); got != 2 {
		t.Errorf("record_count = %d, want 2", got)
	}
	soa := e.soaOf(t, id)
	if want := "ns.internal. hostmaster.nebius.internal. 1 3600 600 604800 900"; soa.GetSpec().GetData() != want {
		t.Errorf("SOA data = %q, want %q", soa.GetSpec().GetData(), want)
	}
	if soa.GetStatus().GetEffectiveFqdn() != "my.example." || soa.GetSpec().GetRelativeName() != "@" {
		t.Errorf("SOA = %v", soa)
	}

	byName, err := e.zones.GetByName(ctx, &commonpb.GetByNameRequest{ParentId: e.inst.ProjectID, Name: "main-zone"})
	if err != nil || byName.GetMetadata().GetId() != id {
		t.Fatalf("GetByName = %v, %v", byName, err)
	}
	_, err = e.zones.GetByName(ctx, &commonpb.GetByNameRequest{ParentId: e.inst.ProjectID, Name: "nope"})
	kmsWantCode(t, err, codes.NotFound)
	_, err = e.zones.Get(ctx, &dnspb.GetZoneRequest{Id: "dnszone-doesnotexist"})
	kmsWantCode(t, err, codes.NotFound)

	byNet, err := e.zones.ListByNetwork(ctx, &dnspb.ListZonesByNetworkRequest{NetworkId: e.netID})
	if err != nil || len(byNet.GetItems()) != 1 || byNet.GetItems()[0].GetMetadata().GetId() != id {
		t.Fatalf("ListByNetwork = %v, %v", byNet, err)
	}

	e.deleteZone(t, id)
	_, err = e.zones.Get(ctx, &dnspb.GetZoneRequest{Id: id})
	kmsWantCode(t, err, codes.NotFound)
	if got := e.query(t, "www.my.example.", dns.TypeA).Rcode; got != dns.RcodeRefused {
		t.Errorf("rcode after delete = %s, want REFUSED", dns.RcodeToString[got])
	}
}

func TestDNSZoneList(t *testing.T) {
	t.Parallel()
	e := startDNS(t)
	ctx := t.Context()
	want := map[string]bool{}
	for _, d := range []string{"a.example.", "b.example.", "c.example."} {
		want[e.createZone(t, "zone-"+strings.TrimSuffix(d, ".example."), d).GetMetadata().GetId()] = true
	}

	got := map[string]bool{}
	token, pages := "", 0
	for {
		page, err := e.zones.List(ctx, &dnspb.ListZonesRequest{ParentId: e.inst.ProjectID, PageSize: 2, PageToken: token})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(page.GetItems()) > 2 {
			t.Fatalf("page has %d items, want <= 2", len(page.GetItems()))
		}
		for _, z := range page.GetItems() {
			got[z.GetMetadata().GetId()] = true
		}
		pages++
		if token = page.GetNextPageToken(); token == "" {
			break
		}
	}
	if pages != 2 || len(got) != 3 {
		t.Errorf("pages = %d, zones = %d, want 2 and 3", pages, len(got))
	}
	for id := range want {
		if !got[id] {
			t.Errorf("zone %s missing from the listing", id)
		}
	}

	// ListByNetwork pages too.
	page, err := e.zones.ListByNetwork(ctx, &dnspb.ListZonesByNetworkRequest{NetworkId: e.netID, PageSize: 2})
	if err != nil || len(page.GetItems()) != 2 || page.GetNextPageToken() == "" {
		t.Fatalf("ListByNetwork page 1 = %v, %v", page, err)
	}
	page, err = e.zones.ListByNetwork(ctx, &dnspb.ListZonesByNetworkRequest{NetworkId: e.netID, PageSize: 2, PageToken: page.GetNextPageToken()})
	if err != nil || len(page.GetItems()) != 1 || page.GetNextPageToken() != "" {
		t.Fatalf("ListByNetwork page 2 = %v, %v", page, err)
	}
}

func TestDNSZoneUpdate(t *testing.T) {
	t.Parallel()
	e := startDNS(t)
	ctx := t.Context()
	z := e.createZone(t, "upd", "upd.example.")
	id := z.GetMetadata().GetId()

	update := func(md *commonpb.ResourceMetadata, spec *dnspb.ZoneSpec) error {
		op, err := e.zones.Update(ctx, &dnspb.UpdateZoneRequest{Metadata: md, Spec: spec})
		return kmsWait(ctx, op, err)
	}
	md := func() *commonpb.ResourceMetadata {
		return &commonpb.ResourceMetadata{Id: id, ParentId: e.inst.ProjectID, Name: "upd"}
	}

	// An unchanged update keeps the resource version.
	if err := update(md(), zoneSpec("upd.example.", e.netID)); err != nil {
		t.Fatalf("unchanged update: %v", err)
	}
	same := e.getZone(t, id)
	if same.GetMetadata().GetResourceVersion() != z.GetMetadata().GetResourceVersion() {
		t.Errorf("resource_version %d -> %d after an unchanged update", z.GetMetadata().GetResourceVersion(), same.GetMetadata().GetResourceVersion())
	}

	// Name and labels.
	m := md()
	m.Name, m.Labels = "renamed", map[string]string{"env": "dev"}
	if err := update(m, zoneSpec("upd.example.", e.netID)); err != nil {
		t.Fatalf("rename: %v", err)
	}
	got := e.getZone(t, id)
	if got.GetMetadata().GetName() != "renamed" || got.GetMetadata().GetLabels()["env"] != "dev" {
		t.Errorf("metadata = %v", got.GetMetadata())
	}
	if got.GetMetadata().GetResourceVersion() <= same.GetMetadata().GetResourceVersion() {
		t.Errorf("resource_version did not grow: %d", got.GetMetadata().GetResourceVersion())
	}

	// The negative TTL rewrites the SOA record and the NXDOMAIN answers.
	spec := zoneSpec("upd.example.", e.netID)
	spec.SoaSpec = &dnspb.SoaSpec{NegativeTtl: 300}
	m = md()
	m.Name = "renamed"
	if err := update(m, spec); err != nil {
		t.Fatalf("soa update: %v", err)
	}
	if got := e.soaOf(t, id).GetSpec().GetData(); !strings.HasSuffix(got, " 604800 300") {
		t.Errorf("SOA data = %q, want negative TTL 300", got)
	}
	nx := e.query(t, "nope.upd.example.", dns.TypeA)
	if len(nx.Ns) != 1 || nx.Ns[0].Header().Ttl != 300 {
		t.Errorf("NXDOMAIN authority = %v, want TTL 300", nx.Ns)
	}

	// X-ResetMask clears soa_spec back to the default.
	m = md()
	m.Name = "renamed"
	op, err := e.zones.Update(resetCtx(ctx, t, "spec.soa_spec"), &dnspb.UpdateZoneRequest{Metadata: m, Spec: zoneSpec("upd.example.", e.netID)})
	if err := kmsWait(ctx, op, err); err != nil {
		t.Fatalf("reset soa_spec: %v", err)
	}
	if got := e.soaOf(t, id).GetSpec().GetData(); !strings.HasSuffix(got, " 604800 900") {
		t.Errorf("SOA data after reset = %q, want negative TTL 900", got)
	}

	// Immutable fields.
	err = update(md(), zoneSpec("other.example.", e.netID))
	kmsWantCode(t, err, codes.InvalidArgument)
	other := e.newNet(t, "other-net")
	err = update(md(), zoneSpec("upd.example.", other))
	kmsWantCode(t, err, codes.InvalidArgument)

	// Stale resource version.
	m = md()
	m.ResourceVersion = got.GetMetadata().GetResourceVersion() + 50
	err = update(m, zoneSpec("upd.example.", e.netID))
	kmsWantCode(t, err, codes.FailedPrecondition)
}

func TestDNSZoneResourceVersionMismatch(t *testing.T) {
	t.Parallel()
	e := startDNS(t)
	ctx := t.Context()
	z := e.createZone(t, "ver", "ver.example.")
	id, rv := z.GetMetadata().GetId(), z.GetMetadata().GetResourceVersion()

	_, err := e.zones.Get(ctx, &dnspb.GetZoneRequest{Id: id, ResourceVersion: rv + 10})
	kmsWantCode(t, err, codes.FailedPrecondition)
	if _, err := e.zones.Get(ctx, &dnspb.GetZoneRequest{Id: id, ResourceVersion: rv}); err != nil {
		t.Errorf("Get at the current version: %v", err)
	}
	op, err := e.zones.Delete(ctx, &dnspb.DeleteZoneRequest{Id: id, ResourceVersion: rv + 10})
	kmsWantCode(t, kmsWait(ctx, op, err), codes.FailedPrecondition)
	op, err = e.zones.Delete(ctx, &dnspb.DeleteZoneRequest{Id: id, ResourceVersion: rv})
	kmsMustWait(t, op, err)
}

func TestDNSZoneCreateErrors(t *testing.T) {
	t.Parallel()
	e := startDNS(t, testkit.WithProjects(config.ProjectSeed{Name: "other"}))
	ctx := t.Context()
	project := e.inst.ProjectID
	otherProject := e.inst.Projects[1].ID

	// A zone with domain "taken.example." and the name "taken" exists.
	e.createZone(t, "taken", "taken.example.")

	cases := []struct {
		name    string
		project string
		zname   string
		spec    *dnspb.ZoneSpec
		want    codes.Code
	}{
		{"no vpc scope", project, "z1", &dnspb.ZoneSpec{DomainName: "novpc.example."}, codes.InvalidArgument},
		{"unknown network", project, "z2", zoneSpec("unk.example.", "vpcnetwork-e00doesnotexist"), codes.NotFound},
		{"network of another project", otherProject, "z3", zoneSpec("cross.example.", e.netID), codes.InvalidArgument},
		{"single label", project, "z4", zoneSpec("example.", e.netID), codes.InvalidArgument},
		{"no trailing dot", project, "z5", zoneSpec("nodot.example", e.netID), codes.InvalidArgument},
		{"reserved ns.internal", project, "z6", zoneSpec("ns.internal.", e.netID), codes.PermissionDenied},
		{"reserved subdomain", project, "z7", zoneSpec("a.compute.internal.", e.netID), codes.PermissionDenied},
		{"reserved console.nebius", project, "z8", zoneSpec("console.nebius.", e.netID), codes.PermissionDenied},
		{"reserved example.invalid", project, "z9", zoneSpec("example.invalid.", e.netID), codes.PermissionDenied},
		{"duplicate domain in the network", project, "z10", zoneSpec("taken.example.", e.netID), codes.AlreadyExists},
		{"duplicate name", project, "taken", zoneSpec("fresh.example.", e.netID), codes.AlreadyExists},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := e.tryZone(ctx, tc.project, tc.zname, tc.spec)
			kmsWantCode(t, err, tc.want)
		})
	}

	for _, ok := range []string{"nested.taken.example.", "corp.internal.", "x.y.z.example."} {
		if _, err := e.tryZone(ctx, project, "", zoneSpec(ok, e.netID)); err != nil {
			t.Errorf("zone %s: %v, want success", ok, err)
		}
	}
}

func TestDNSZoneDeleteNeedsEmptyZone(t *testing.T) {
	t.Parallel()
	e := startDNS(t)
	ctx := t.Context()
	z := e.createZone(t, "full", "full.example.")
	zid := z.GetMetadata().GetId()
	rec := e.createRecord(t, zid, "r", "www", dnspb.RecordSpec_A, "192.0.2.1")

	op, err := e.zones.Delete(ctx, &dnspb.DeleteZoneRequest{Id: zid})
	kmsWantCode(t, kmsWait(ctx, op, err), codes.FailedPrecondition)
	if _, err := e.zones.Get(ctx, &dnspb.GetZoneRequest{Id: zid}); err != nil {
		t.Fatalf("zone gone after a refused delete: %v", err)
	}

	e.deleteRecord(t, rec.GetMetadata().GetId())
	e.deleteZone(t, zid)
	list, err := e.records.List(ctx, &dnspb.ListRecordsRequest{ParentId: zid})
	if err == nil && len(list.GetItems()) != 0 {
		t.Errorf("records left after zone delete: %v", list.GetItems())
	}
}

func TestDNSZoneBlocksNetworkDelete(t *testing.T) {
	t.Parallel()
	e := startDNS(t)
	ctx := t.Context()
	netID := e.newNet(t, "dns-net")

	zid, err := e.tryZone(ctx, e.inst.ProjectID, "bound", zoneSpec("bound.example.", netID))
	if err != nil {
		t.Fatalf("create zone: %v", err)
	}
	op, err := e.nets.Delete(ctx, &vpcpb.DeleteNetworkRequest{Id: netID})
	err = kmsWait(ctx, op, err)
	kmsWantCode(t, err, codes.FailedPrecondition)
	if !strings.Contains(err.Error(), zid) {
		t.Errorf("error %q does not name the zone %s", err, zid)
	}

	e.deleteZone(t, zid)
	op, err = e.nets.Delete(ctx, &vpcpb.DeleteNetworkRequest{Id: netID})
	kmsMustWait(t, op, err)
}

func TestDNSSameDomainInAnotherNetwork(t *testing.T) {
	t.Parallel()
	e := startDNS(t)
	ctx := t.Context()
	e.createZone(t, "one", "shared.example.")
	other := e.newNet(t, "second-net")
	if _, err := e.tryZone(ctx, e.inst.ProjectID, "two", zoneSpec("shared.example.", other)); err != nil {
		t.Fatalf("same domain in another network: %v", err)
	}
	byNet, err := e.zones.ListByNetwork(ctx, &dnspb.ListZonesByNetworkRequest{NetworkId: other})
	if err != nil || len(byNet.GetItems()) != 1 || byNet.GetItems()[0].GetMetadata().GetName() != "two" {
		t.Errorf("ListByNetwork = %v, %v", byNet, err)
	}
}
