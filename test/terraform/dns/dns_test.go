//go:build e2e

// Package dns runs the official Terraform binary with the official
// nebius/nebius provider from the Terraform Registry against NebiusMocker's
// DNS service (zones, records and the authoritative responder). Nothing is
// patched or forked.
//
// Run with: make e2e   (needs terraform on PATH and registry access on first run)
package dns

import (
	"bytes"
	"fmt"
	"maps"
	"net"
	"strconv"
	"strings"
	"testing"
	"text/template"
	"time"

	mdns "github.com/miekg/dns"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	dnspb "github.com/nebius/gosdk/proto/nebius/dns/v1"
	dnssvc "github.com/nebius/gosdk/services/nebius/dns/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	dnsmod "github.com/kfirgollan/NebiusMocker/services/dns"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/services/vpc"
)

type env struct {
	inst    *testkit.Instance
	mod     *dnsmod.Module
	zones   dnssvc.ZoneService
	records dnssvc.RecordService
}

func startDNS(t *testing.T) *env {
	t.Helper()
	mod := dnsmod.New(dnsmod.WithListen("127.0.0.1:0"))
	// A non-zero delay makes operations asynchronous, so the provider's
	// OperationService polling path is exercised too.
	inst := testkit.StartT(t, []mocker.Module{iam.New(), vpc.New(), mod},
		mockerOptions(testkit.WithOperationDelay(200*time.Millisecond), testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce))...)
	sdk := inst.SDK(t)
	return &env{inst: inst, mod: mod, zones: dnssvc.NewZoneService(sdk), records: dnssvc.NewRecordService(sdk)}
}

// rec describes one nebius_dns_v1_record in the rendered config.
type rec struct {
	Res, Name, Rel, Type, Data, TTL, Labels string
	Protect                                 bool
}

type cfg struct {
	Proj, ZoneName, Domain, ZoneLabels, NegTTL string
	Records                                    []rec
	DataSources                                bool
}

var cfgTmpl = template.Must(template.New("cfg").Parse(`
resource "nebius_vpc_v1_network" "net" {
  parent_id = "{{.Proj}}"
  name      = "tf-dns-net"
}

resource "nebius_dns_v1_zone" "z" {
  parent_id   = "{{.Proj}}"
  name        = "{{.ZoneName}}"
  labels      = { {{.ZoneLabels}} }
  domain_name = "{{.Domain}}"
  vpc         = { primary_network_id = nebius_vpc_v1_network.net.id }
  soa_spec    = { negative_ttl = {{.NegTTL}} }
}
{{range .Records}}
resource "nebius_dns_v1_record" "{{.Res}}" {
  parent_id     = nebius_dns_v1_zone.z.id
  name          = "{{.Name}}"
  relative_name = "{{.Rel}}"
  type          = "{{.Type}}"
  data          = {{.Data}}
{{- if .TTL}}
  ttl           = {{.TTL}}
{{- end}}
{{- if .Labels}}
  labels        = { {{.Labels}} }
{{- end}}
{{- if .Protect}}
  deletion_protection = true
{{- end}}
}
{{end}}
{{- if .DataSources}}
data "nebius_dns_v1_zone" "by_id" {
  id = nebius_dns_v1_zone.z.id
}
data "nebius_dns_v1_zone" "by_name" {
  parent_id = "{{.Proj}}"
  name      = nebius_dns_v1_zone.z.name
}
data "nebius_dns_v1_record" "by_id" {
  id = nebius_dns_v1_record.www.id
}
data "nebius_dns_v1_record" "by_name" {
  parent_id = nebius_dns_v1_zone.z.id
  name      = nebius_dns_v1_record.www.name
}
{{end}}`))

func (c *cfg) render(t *testing.T) string {
	t.Helper()
	var b bytes.Buffer
	if err := cfgTmpl.Execute(&b, c); err != nil {
		t.Fatalf("render: %v", err)
	}
	return b.String()
}

// baseCfg is the lifecycle starting point: A with ttl, TXT with escaped
// quotes, MX, A without ttl, and a delete-protected A.
func baseCfg(proj string) cfg {
	return cfg{
		Proj: proj, ZoneName: "tf-zone", Domain: "my.example.", ZoneLabels: `env = "dev"`, NegTTL: "300",
		Records: []rec{
			{Res: "www", Name: "tf-www", Rel: "www", Type: "A", Data: `"10.0.0.1"`, TTL: "300", Labels: `tier = "web"`},
			{Res: "spf", Name: "tf-spf", Rel: "@", Type: "TXT", Data: `"\"v=spf1 -all\""`, TTL: "120"},
			{Res: "mx", Name: "tf-mx", Rel: "@", Type: "MX", Data: `"10 mail.my.example."`, TTL: "3600"},
			{Res: "nottl", Name: "tf-nottl", Rel: "api", Type: "A", Data: `"10.0.0.2"`},
			{Res: "prot", Name: "tf-prot", Rel: "db", Type: "A", Data: `"10.0.0.3"`, TTL: "60", Protect: true},
		},
	}
}

func (c *cfg) rec(t *testing.T, res string) *rec {
	t.Helper()
	for i := range c.Records {
		if c.Records[i].Res == res {
			return &c.Records[i]
		}
	}
	t.Fatalf("scenario has no record %q", res)
	return nil
}

func (e *env) getZone(t *testing.T, id string) *dnspb.Zone {
	t.Helper()
	z, err := e.zones.Get(t.Context(), &dnspb.GetZoneRequest{Id: id})
	if err != nil {
		t.Fatalf("Get zone %s: %v", id, err)
	}
	return z
}

func (e *env) getRecord(t *testing.T, id string) *dnspb.Record {
	t.Helper()
	r, err := e.records.Get(t.Context(), &dnspb.GetRecordRequest{Id: id})
	if err != nil {
		t.Fatalf("Get record %s: %v", id, err)
	}
	return r
}

// checkState verifies, through the Go SDK, that the mock holds exactly what
// the config describes: zone fields, every record, and the record count
// (user records plus the managed SOA and NS).
func (e *env) checkState(t *testing.T, tf *tfRun, c cfg) {
	t.Helper()
	ids := tf.stateIDs(t)
	z := e.getZone(t, ids["z"])
	if z.GetMetadata().GetName() != c.ZoneName || z.GetMetadata().GetParentId() != c.Proj ||
		z.GetSpec().GetDomainName() != c.Domain || z.GetSpec().GetVpc().GetPrimaryNetworkId() != ids["net"] {
		t.Fatalf("mock zone = %v, want %+v", z, c)
	}
	if got := z.GetStatus().GetRecordCount(); got != int64(len(c.Records)+2) {
		t.Errorf("zone record_count = %d, want %d", got, len(c.Records)+2)
	}
	for _, want := range c.Records {
		r := e.getRecord(t, ids[want.Res])
		wantTTL := int64(0)
		if want.TTL != "" {
			if _, err := fmt.Sscan(want.TTL, &wantTTL); err != nil {
				t.Fatalf("bad ttl %q", want.TTL)
			}
		}
		wantData, err := strconv.Unquote(want.Data) // HCL and Go share these string escapes
		if err != nil {
			t.Fatalf("bad data %q: %v", want.Data, err)
		}
		fqdn := want.Rel + "." + c.Domain
		if want.Rel == "@" {
			fqdn = c.Domain
		}
		if r.GetMetadata().GetParentId() != ids["z"] || r.GetMetadata().GetName() != want.Name ||
			r.GetSpec().GetRelativeName() != want.Rel || r.GetSpec().GetType().String() != want.Type ||
			r.GetSpec().GetData() != wantData || r.GetSpec().GetTtl() != wantTTL ||
			r.GetSpec().GetDeletionProtection() != want.Protect ||
			r.GetStatus().GetEffectiveFqdn() != fqdn || r.GetStatus().GetZoneDomainName() != c.Domain {
			t.Fatalf("mock record %s = %v, want %+v (data %q fqdn %q)", want.Res, r, want, wantData, fqdn)
		}
	}
}

func (e *env) checkGone(t *testing.T, zoneID string, recordIDs ...string) {
	t.Helper()
	if _, err := e.zones.Get(t.Context(), &dnspb.GetZoneRequest{Id: zoneID}); status.Code(err) != codes.NotFound {
		t.Fatalf("Get zone %s: %v, want NotFound", zoneID, err)
	}
	for _, id := range recordIDs {
		if _, err := e.records.Get(t.Context(), &dnspb.GetRecordRequest{Id: id}); status.Code(err) != codes.NotFound {
			t.Fatalf("Get record %s: %v, want NotFound", id, err)
		}
	}
}

func (e *env) query(t *testing.T, name string, typ uint16) *mdns.Msg {
	t.Helper()
	addr := e.mod.DNSAddr()
	if _, _, err := net.SplitHostPort(addr); err != nil {
		t.Fatalf("DNSAddr = %q", addr)
	}
	m := new(mdns.Msg)
	m.SetQuestion(name, typ)
	resp, _, err := new(mdns.Client).ExchangeContext(t.Context(), m, addr)
	if err != nil {
		t.Fatalf("dns query %s: %v", name, err)
	}
	return resp
}

func TestZoneAndRecordLifecycle(t *testing.T) {
	e := startDNS(t)
	tf := newTerraform(t, e.inst)
	c := baseCfg(e.inst.ProjectID)

	// Create.
	tf.writeConfig(t, c.render(t))
	tf.init(t)
	tf.apply(t)
	tf.expectNoChanges(t)
	e.checkState(t, tf, c)
	first := maps.Clone(tf.stateIDs(t))

	// The responder serves what Terraform created.
	resp := e.query(t, "www.my.example.", mdns.TypeA)
	if !resp.Authoritative || len(resp.Answer) != 1 || resp.Answer[0].(*mdns.A).A.String() != "10.0.0.1" || resp.Answer[0].Header().Ttl != 300 {
		t.Fatalf("A answer = %v", resp)
	}
	resp = e.query(t, "my.example.", mdns.TypeTXT)
	if len(resp.Answer) != 1 || strings.Join(resp.Answer[0].(*mdns.TXT).Txt, "") != "v=spf1 -all" {
		t.Fatalf("TXT answer = %v", resp)
	}
	resp = e.query(t, "api.my.example.", mdns.TypeA)
	if len(resp.Answer) != 1 || resp.Answer[0].Header().Ttl != 600 {
		t.Fatalf("ttl-less A answer = %v, want default ttl 600", resp)
	}
	if resp = e.query(t, "nope.my.example.", mdns.TypeA); resp.Rcode != mdns.RcodeNameError {
		t.Fatalf("missing name rcode = %d, want NXDOMAIN", resp.Rcode)
	}

	// In place update: zone name/labels/negative_ttl; record data/ttl/name/
	// labels; deletion_protection true -> false.
	c.ZoneName, c.ZoneLabels, c.NegTTL = "tf-zone-2", `env = "prod", team = "net"`, "600"
	w := c.rec(t, "www")
	w.Data, w.TTL, w.Name, w.Labels = `"10.0.0.9"`, "900", "tf-www-2", `tier = "edge"`
	c.rec(t, "nottl").TTL = "45"
	c.rec(t, "prot").Protect = false
	tf.writeConfig(t, c.render(t))
	tf.expectPlan(t, map[string]string{
		"net": "no-op", "z": "update", "www": "update", "nottl": "update", "prot": "update",
		"spf": "no-op", "mx": "no-op",
	})
	tf.apply(t)
	for k, v := range tf.stateIDs(t) {
		if v != first[k] {
			t.Fatalf("%s recreated on update: %s -> %s", k, first[k], v)
		}
	}
	tf.expectNoChanges(t)
	e.checkState(t, tf, c)
	if resp = e.query(t, "www.my.example.", mdns.TypeA); len(resp.Answer) != 1 || resp.Answer[0].(*mdns.A).A.String() != "10.0.0.9" {
		t.Fatalf("A answer after update = %v", resp)
	}

	// Replace: zone domain_name is RequiresReplace; its records follow.
	c.Domain = "other.example."
	c.rec(t, "mx").Data = `"10 mail.other.example."`
	tf.writeConfig(t, c.render(t))
	tf.apply(t)
	tf.expectNoChanges(t)
	e.checkState(t, tf, c)
	second := maps.Clone(tf.stateIDs(t))
	if second["z"] == first["z"] || second["net"] != first["net"] {
		t.Fatalf("zone ids %s -> %s (want new), network %s -> %s (want same)", first["z"], second["z"], first["net"], second["net"])
	}
	e.checkGone(t, first["z"], first["www"], first["prot"])

	// Replace one record on relative_name and another on type.
	c.rec(t, "www").Rel = "web"
	n := c.rec(t, "nottl")
	n.Type, n.Data = "TXT", `"\"hello\""`
	tf.writeConfig(t, c.render(t))
	tf.apply(t)
	tf.expectNoChanges(t)
	e.checkState(t, tf, c)
	third := maps.Clone(tf.stateIDs(t))
	if third["www"] == second["www"] || third["nottl"] == second["nottl"] || third["z"] != second["z"] || third["mx"] != second["mx"] {
		t.Fatalf("ids after record replace: before %v after %v", second, third)
	}

	// Destroy: nothing is left behind.
	tf.destroy(t)
	e.checkGone(t, third["z"], third["www"], third["spf"], third["mx"], third["nottl"], third["prot"])
	if len(tf.stateIDs(t)) != 0 {
		t.Fatalf("state not empty after destroy: %v", tf.stateIDs(t))
	}
}

func TestDataSources(t *testing.T) {
	e := startDNS(t)
	tf := newTerraform(t, e.inst)
	c := baseCfg(e.inst.ProjectID)
	c.Records = c.Records[:2] // www + spf
	c.DataSources = true
	tf.writeConfig(t, c.render(t))
	tf.init(t)
	tf.apply(t)
	tf.expectNoChanges(t)
	e.checkState(t, tf, c)

	// record_count is computed while records are still being created, so the
	// state written by apply is a snapshot; a refresh re-reads zones and
	// data sources once everything exists.
	if err := tf.tf.Refresh(t.Context()); err != nil {
		t.Fatalf("terraform refresh: %v", err)
	}
	attrs := tf.stateAttrs(t)
	str := func(m map[string]any, path ...string) string {
		var cur any = m
		for _, p := range path {
			mm, _ := cur.(map[string]any)
			cur = mm[p]
		}
		return fmt.Sprint(cur)
	}
	zone := attrs["nebius_dns_v1_zone.z"]
	for _, d := range []string{"data.nebius_dns_v1_zone.by_id", "data.nebius_dns_v1_zone.by_name"} {
		got := attrs[d]
		if got == nil {
			t.Fatalf("%s missing from state", d)
		}
		for _, p := range [][]string{
			{"id"},
			{"domain_name"},
			{"name"},
			{"parent_id"},
			{"vpc", "primary_network_id"},
			{"status", "record_count"},
			{"soa_spec", "negative_ttl"},
			{"labels", "env"},
		} {
			if str(got, p...) != str(zone, p...) || str(got, p...) == "<nil>" {
				t.Errorf("%s %v = %q, resource %q", d, p, str(got, p...), str(zone, p...))
			}
		}
	}
	if str(zone, "status", "record_count") != "4" {
		t.Errorf("record_count = %s, want 4 (2 records + SOA + NS)", str(zone, "status", "record_count"))
	}
	rec := attrs["nebius_dns_v1_record.www"]
	for _, d := range []string{"data.nebius_dns_v1_record.by_id", "data.nebius_dns_v1_record.by_name"} {
		got := attrs[d]
		if got == nil {
			t.Fatalf("%s missing from state", d)
		}
		for _, p := range [][]string{
			{"id"},
			{"data"},
			{"type"},
			{"relative_name"},
			{"ttl"},
			{"name"},
			{"parent_id"},
			{"status", "effective_fqdn"},
			{"status", "zone_domain_name"},
			{"labels", "tier"},
		} {
			if str(got, p...) != str(rec, p...) || str(got, p...) == "<nil>" {
				t.Errorf("%s %v = %q, resource %q", d, p, str(got, p...), str(rec, p...))
			}
		}
	}
	if str(rec, "status", "effective_fqdn") != "www.my.example." || str(rec, "status", "zone_domain_name") != "my.example." {
		t.Errorf("record status = %v", rec["status"])
	}
}

func TestDeletionProtection(t *testing.T) {
	e := startDNS(t)
	tf := newTerraform(t, e.inst)
	c := baseCfg(e.inst.ProjectID)
	c.Records = c.Records[4:] // prot only
	tf.writeConfig(t, c.render(t))
	tf.init(t)
	tf.apply(t)
	tf.expectNoChanges(t)
	e.checkState(t, tf, c)
	ids := maps.Clone(tf.stateIDs(t))

	before := tf.out.Len()
	err := tf.tf.Destroy(t.Context())
	if err == nil {
		t.Fatalf("terraform destroy succeeded with a delete-protected record")
	}
	out := strings.ToLower(tf.out.String()[before:] + err.Error())
	if !strings.Contains(out, "failedprecondition") && !strings.Contains(out, "failed precondition") && !strings.Contains(out, "failed_precondition") {
		t.Fatalf("destroy failed without FailedPrecondition:\n%s", out)
	}
	// The record survived, and so did the zone.
	e.getRecord(t, ids["prot"])
	e.getZone(t, ids["z"])

	// Switch protection off; now destroy succeeds.
	c.rec(t, "prot").Protect = false
	tf.writeConfig(t, c.render(t))
	tf.apply(t)
	tf.expectNoChanges(t)
	e.checkState(t, tf, c)
	tf.destroy(t)
	e.checkGone(t, ids["z"], ids["prot"])
}
