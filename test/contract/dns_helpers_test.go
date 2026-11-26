package contract

import (
	"testing"

	"github.com/miekg/dns"

	"github.com/nebius/gosdk/operations"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	dnspb "github.com/nebius/gosdk/proto/nebius/dns/v1"
	vpcpb "github.com/nebius/gosdk/proto/nebius/vpc/v1"
	dnssvc "github.com/nebius/gosdk/services/nebius/dns/v1"
	vpcsvc "github.com/nebius/gosdk/services/nebius/vpc/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	dnsmod "github.com/kfirgollan/NebiusMocker/services/dns"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/services/vpc"
)

// dnsEnv is a mocker with IAM, VPC and DNS (its responder on a free port).
type dnsEnv struct {
	inst    *testkit.Instance
	mod     *dnsmod.Module
	zones   dnssvc.ZoneService
	records dnssvc.RecordService
	nets    vpcsvc.NetworkService
	// netID is the seeded project's default network.
	netID string
}

func startDNS(t *testing.T, opts ...testkit.Option) *dnsEnv {
	t.Helper()
	mod := dnsmod.New(dnsmod.WithListen("127.0.0.1:0"))
	inst := testkit.StartT(t, []mocker.Module{iam.New(), vpc.New(), mod}, opts...)
	sdk := inst.SDK(t)
	e := &dnsEnv{
		inst: inst, mod: mod,
		zones: dnssvc.NewZoneService(sdk), records: dnssvc.NewRecordService(sdk), nets: vpcsvc.NewNetworkService(sdk),
	}
	n, err := e.nets.GetByName(t.Context(), &vpcpb.GetNetworkByNameRequest{ParentId: inst.ProjectID, Name: "default-network"})
	if err != nil {
		t.Fatalf("default network: %v", err)
	}
	e.netID = n.GetMetadata().GetId()
	return e
}

func (e *dnsEnv) createZone(t *testing.T, name, domain string) *dnspb.Zone {
	t.Helper()
	op, err := e.zones.Create(t.Context(), &dnspb.CreateZoneRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: e.inst.ProjectID, Name: name},
		Spec: &dnspb.ZoneSpec{
			DomainName: domain,
			Scope:      &dnspb.ZoneSpec_Vpc{Vpc: &dnspb.VpcZoneScope{PrimaryNetworkId: e.netID}},
		},
	})
	id := mustOp(t, op, err)
	z, err := e.zones.Get(t.Context(), &dnspb.GetZoneRequest{Id: id})
	if err != nil {
		t.Fatalf("get zone: %v", err)
	}
	return z
}

func (e *dnsEnv) createRecord(t *testing.T, zoneID, name, rel string, typ dnspb.RecordSpec_RecordType, data string) *dnspb.Record {
	t.Helper()
	op, err := e.records.Create(t.Context(), &dnspb.CreateRecordRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: zoneID, Name: name},
		Spec:     &dnspb.RecordSpec{RelativeName: rel, Type: typ, Data: data},
	})
	id := mustOp(t, op, err)
	r, err := e.records.Get(t.Context(), &dnspb.GetRecordRequest{Id: id})
	if err != nil {
		t.Fatalf("get record: %v", err)
	}
	return r
}

// mustOp waits for an operation and returns its resource ID.
func mustOp(t *testing.T, op operations.Operation, err error) string {
	t.Helper()
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if _, err := op.Wait(t.Context()); err != nil {
		t.Fatalf("operation: %v", err)
	}
	return op.ResourceID()
}

// query asks the mock's DNS responder over UDP, the way dig does.
func (e *dnsEnv) query(t *testing.T, name string, qtype uint16) *dns.Msg {
	t.Helper()
	m := new(dns.Msg)
	m.SetQuestion(name, qtype)
	resp, _, err := new(dns.Client).ExchangeContext(t.Context(), m, e.mod.DNSAddr())
	if err != nil {
		t.Fatalf("dns query %s %s: %v", name, dns.TypeToString[qtype], err)
	}
	return resp
}
