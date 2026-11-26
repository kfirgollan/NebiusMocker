//go:build e2e

// Package dns runs the official nebius CLI against NebiusMocker through the
// DNS flows the Nebius docs show (https://docs.nebius.com/vpc/dns/manage-zones,
// https://docs.nebius.com/vpc/dns/manage-records), and verifies the records
// with dig against the mock's DNS responder, as the docs verify them from a VM.
package dns

import (
	"net"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	dnsmod "github.com/kfirgollan/NebiusMocker/services/dns"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/services/vpc"
	"github.com/kfirgollan/NebiusMocker/test/internal/nebiuscli"
)

type metadata struct {
	ID              string            `json:"id"`
	ParentID        string            `json:"parent_id"`
	Name            string            `json:"name"`
	ResourceVersion string            `json:"resource_version"`
	Labels          map[string]string `json:"labels"`
}

type zone struct {
	Metadata metadata `json:"metadata"`
	Spec     struct {
		DomainName string `json:"domain_name"`
		Vpc        struct {
			PrimaryNetworkID string `json:"primary_network_id"`
		} `json:"vpc"`
		SoaSpec struct {
			NegativeTTL string `json:"negative_ttl"`
		} `json:"soa_spec"`
	} `json:"spec"`
	Status struct {
		RecordCount string `json:"record_count"`
	} `json:"status"`
}

type record struct {
	Metadata metadata `json:"metadata"`
	Spec     struct {
		RelativeName       string `json:"relative_name"`
		Type               string `json:"type"`
		TTL                string `json:"ttl"`
		Data               string `json:"data"`
		DeletionProtection bool   `json:"deletion_protection"`
	} `json:"spec"`
	Status struct {
		ZoneDomainName string `json:"zone_domain_name"`
		EffectiveFQDN  string `json:"effective_fqdn"`
	} `json:"status"`
}

type env struct {
	cli     *nebiuscli.CLI
	dnsAddr string
	project string
}

func start(t *testing.T) *env {
	t.Helper()
	mod := dnsmod.New(dnsmod.WithListen("127.0.0.1:0"))
	inst := testkit.StartT(t, []mocker.Module{iam.New(), vpc.New(), mod},
		testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce), testkit.WithOperationDelay(100*time.Millisecond))
	cli := nebiuscli.New(t, nebiuscli.Profile{Addr: inst.Addr, Token: inst.Token, ParentID: inst.ProjectID, TenantID: inst.TenantID})
	return &env{cli: cli, dnsAddr: mod.DNSAddr(), project: inst.ProjectID}
}

func (e *env) run(t *testing.T, args ...string) {
	t.Helper()
	if _, err := e.cli.Run(t.Context(), args...); err != nil {
		t.Fatalf("%v", err)
	}
}

// fails runs a command that must fail with an error mentioning want.
func (e *env) fails(t *testing.T, want string, args ...string) {
	t.Helper()
	_, err := e.cli.Run(t.Context(), args...)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("nebius %s: err = %v, want one mentioning %q", strings.Join(args, " "), err, want)
	}
}

// dig queries the mock's DNS responder the way the docs verify a record
// ("dig www.my.example."), pointed at the responder's address.
func (e *env) dig(t *testing.T, name, typ string) []string {
	t.Helper()
	bin, err := exec.LookPath("dig")
	if err != nil {
		t.Skip("dig not found on PATH")
	}
	host, port, err := net.SplitHostPort(e.dnsAddr)
	if err != nil {
		t.Fatalf("dns address %q: %v", e.dnsAddr, err)
	}
	out, err := exec.CommandContext(t.Context(), bin, "@"+host, "-p", port, name, typ, "+short", "+norecurse").CombinedOutput()
	if err != nil {
		t.Fatalf("dig %s %s: %v\n%s", name, typ, err, out)
	}
	var lines []string
	for l := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		if l != "" {
			lines = append(lines, l)
		}
	}
	slices.Sort(lines)
	return lines
}

func (e *env) defaultNetwork(t *testing.T) string {
	t.Helper()
	var n struct {
		Metadata metadata `json:"metadata"`
	}
	e.cli.JSON(t, &n, "vpc", "network", "get-by-name", "--name", "default-network")
	return n.Metadata.ID
}

// TestZoneAndRecordFlows follows the docs: create a zone, add, verify, update
// and delete records (with deletion protection), update and delete the zone.
func TestZoneAndRecordFlows(t *testing.T) {
	e := start(t)
	netID := e.defaultNetwork(t)

	// nebius dns zone create --domain-name … --parent-id … --name … --labels … --soa-spec-negative-ttl … --vpc-primary-network-id …
	var z zone
	e.cli.JSON(t, &z, "dns", "zone", "create", "--domain-name", "my.example.", "--parent-id", e.project, "--name", "my-zone",
		"--labels", "team=net,env=dev", "--soa-spec-negative-ttl", "300", "--vpc-primary-network-id", netID)
	zoneID := z.Metadata.ID
	if !strings.HasPrefix(zoneID, "dnszone-") || z.Spec.DomainName != "my.example." || z.Spec.Vpc.PrimaryNetworkID != netID ||
		z.Spec.SoaSpec.NegativeTTL != "300" || z.Status.RecordCount != "2" {
		t.Fatalf("zone create = %+v", z)
	}

	// nebius dns zone list
	var zones struct {
		Items []zone `json:"items"`
	}
	e.cli.JSON(t, &zones, "dns", "zone", "list")
	if len(zones.Items) != 1 || zones.Items[0].Metadata.ID != zoneID {
		t.Fatalf("zone list = %+v", zones)
	}

	// nebius dns record create --parent-id … --name … --relative-name … --type … --data … --ttl …
	var www record
	e.cli.JSON(t, &www, "dns", "record", "create", "--parent-id", zoneID, "--name", "www-a-record",
		"--relative-name", "www", "--type", "a", "--data", "192.0.2.10", "--ttl", "600")
	if www.Spec.Type != "A" || www.Status.EffectiveFQDN != "www.my.example." || www.Status.ZoneDomainName != "my.example." {
		t.Fatalf("record create = %+v", www)
	}
	// Several values for one name: several records with the same name and type.
	e.run(t, "dns", "record", "create", "--parent-id", zoneID, "--relative-name", "www", "--type", "a", "--data", "192.0.2.11")
	e.fails(t, "AlreadyExists", "dns", "record", "create", "--parent-id", zoneID, "--relative-name", "www", "--type", "a", "--data", "192.0.2.11")
	e.fails(t, "CNAME", "dns", "record", "create", "--parent-id", zoneID, "--relative-name", "alias", "--type", "cname", "--data", "www.my.example.")

	// The protected SPF record from the docs.
	var spf record
	e.cli.JSON(t, &spf, "dns", "record", "create", "--parent-id", zoneID, "--name", "spf-record", "--relative-name", "@",
		"--type", "txt", "--data", `"v=spf1 -all"`, "--ttl", "3600", "--deletion-protection")
	if !spf.Spec.DeletionProtection {
		t.Fatalf("spf record = %+v, want deletion protection", spf)
	}
	e.run(t, "dns", "record", "create", "--parent-id", zoneID, "--relative-name", "@", "--type", "mx", "--data", "10 mail.my.example.")

	// Verify with dig.
	if got := e.dig(t, "www.my.example.", "A"); !slices.Equal(got, []string{"192.0.2.10", "192.0.2.11"}) {
		t.Fatalf("dig www A = %v", got)
	}
	if got := e.dig(t, "my.example.", "TXT"); !slices.Equal(got, []string{`"v=spf1 -all"`}) {
		t.Fatalf("dig TXT = %v", got)
	}
	if got := e.dig(t, "my.example.", "MX"); !slices.Equal(got, []string{"10 mail.my.example."}) {
		t.Fatalf("dig MX = %v", got)
	}

	// nebius dns record list --parent-id … (and --all): the user records plus SOA and NS.
	var recs struct {
		Items []record `json:"items"`
	}
	e.cli.JSON(t, &recs, "dns", "record", "list", "--parent-id", zoneID)
	if len(recs.Items) != 6 {
		t.Fatalf("record list = %d records, want 6", len(recs.Items))
	}
	var all struct {
		Items []record `json:"items"`
	}
	e.cli.JSON(t, &all, "dns", "record", "list", "--parent-id", zoneID, "--all")
	if len(all.Items) != 6 {
		t.Fatalf("record list --all = %d records, want 6", len(all.Items))
	}

	// nebius dns record get-by-name --parent-id … --name …
	var byName record
	e.cli.JSON(t, &byName, "dns", "record", "get-by-name", "--parent-id", zoneID, "--name", "www-a-record")
	if byName.Metadata.ID != www.Metadata.ID {
		t.Fatalf("get-by-name = %+v, want %s", byName, www.Metadata.ID)
	}

	// nebius dns record update <id> --data … --ttl …
	e.run(t, "dns", "record", "update", www.Metadata.ID, "--data", "192.0.2.20", "--ttl", "300")
	var updated record
	e.cli.JSON(t, &updated, "dns", "record", "get", www.Metadata.ID)
	if updated.Spec.Data != "192.0.2.20" || updated.Spec.TTL != "300" || updated.Metadata.Name != "www-a-record" {
		t.Fatalf("record after update = %+v", updated)
	}
	if got := e.dig(t, "www.my.example.", "A"); !slices.Equal(got, []string{"192.0.2.11", "192.0.2.20"}) {
		t.Fatalf("dig www A after update = %v", got)
	}

	// A protected record: delete fails with FailedPrecondition, --force deletes it.
	e.fails(t, "FailedPrecondition", "dns", "record", "delete", spf.Metadata.ID)
	e.run(t, "dns", "record", "delete", spf.Metadata.ID, "--force")
	if got := e.dig(t, "my.example.", "TXT"); len(got) != 0 {
		t.Fatalf("dig TXT after delete = %v, want nothing", got)
	}

	// nebius dns zone update <id> --name … --soa-spec-negative-ttl … --resource-version … --labels-add / --labels-remove
	var cur zone
	e.cli.JSON(t, &cur, "dns", "zone", "get", zoneID)
	e.run(t, "dns", "zone", "update", zoneID, "--name", "renamed-zone", "--soa-spec-negative-ttl", "60",
		"--resource-version", cur.Metadata.ResourceVersion, "--labels-add", "owner=me", "--labels-remove", "env")
	cur = zone{} // json.Unmarshal merges into an existing labels map
	e.cli.JSON(t, &cur, "dns", "zone", "get", zoneID)
	if cur.Metadata.Name != "renamed-zone" || cur.Spec.SoaSpec.NegativeTTL != "60" ||
		cur.Metadata.Labels["owner"] != "me" || cur.Metadata.Labels["team"] != "net" || cur.Metadata.Labels["env"] != "" {
		t.Fatalf("zone after update = %+v", cur)
	}
	if soa := e.dig(t, "my.example.", "SOA"); len(soa) != 1 || !strings.HasSuffix(soa[0], " 60") {
		t.Fatalf("dig SOA = %v, want the negative TTL 60 as its last field", soa)
	}
	// A stale resource version fails.
	e.fails(t, "FailedPrecondition", "dns", "zone", "update", zoneID, "--name", "x", "--resource-version", "1")

	// nebius dns zone delete --id …: refused while user records remain.
	e.fails(t, "FailedPrecondition", "dns", "zone", "delete", "--id", zoneID)
	recs.Items = nil
	e.cli.JSON(t, &recs, "dns", "record", "list", "--parent-id", zoneID)
	for _, r := range recs.Items {
		if r.Spec.Type != "SOA" && r.Spec.Type != "NS" {
			e.run(t, "dns", "record", "delete", r.Metadata.ID)
		}
	}
	cur = zone{}
	e.cli.JSON(t, &cur, "dns", "zone", "get", zoneID)
	if cur.Status.RecordCount != "2" {
		t.Fatalf("record_count = %s, want 2", cur.Status.RecordCount)
	}
	e.run(t, "dns", "zone", "delete", "--id", zoneID, "--resource-version", cur.Metadata.ResourceVersion)
	zones.Items = nil // an empty list has no "items" key
	e.cli.JSON(t, &zones, "dns", "zone", "list")
	if len(zones.Items) != 0 {
		t.Fatalf("zone list after delete = %+v", zones)
	}
}

// TestNetworkBoundToZone shows that a network with a zone cannot be deleted
// until the zone is.
func TestNetworkBoundToZone(t *testing.T) {
	e := start(t)
	netID := e.defaultNetwork(t)
	var z zone
	e.cli.JSON(t, &z, "dns", "zone", "create", "--domain-name", "svc.corp.internal.", "--vpc-primary-network-id", netID)
	// Reserved names are refused.
	e.fails(t, "PermissionDenied", "dns", "zone", "create", "--domain-name", "x.compute.internal.", "--vpc-primary-network-id", netID)
	// Zones listed by network.
	var byNet struct {
		Items []zone `json:"items"`
	}
	e.cli.JSON(t, &byNet, "dns", "zone", "list-by-network", "--network-id", netID)
	if len(byNet.Items) != 1 || byNet.Items[0].Metadata.ID != z.Metadata.ID {
		t.Fatalf("list-by-network = %+v", byNet)
	}
	e.fails(t, z.Metadata.ID, "vpc", "network", "delete", netID) // also in use by its default subnet
	e.run(t, "dns", "zone", "delete", "--id", z.Metadata.ID)
	if n := strconv.Itoa(len(e.dig(t, "svc.corp.internal.", "SOA"))); n != "0" {
		t.Fatalf("dig after zone delete returned %s answers, want none", n)
	}
}
