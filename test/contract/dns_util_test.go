package contract

import (
	"context"
	"testing"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	dnspb "github.com/nebius/gosdk/proto/nebius/dns/v1"
	vpcpb "github.com/nebius/gosdk/proto/nebius/vpc/v1"
)

// zoneSpec is a private zone on the network.
func zoneSpec(domain, netID string) *dnspb.ZoneSpec {
	return &dnspb.ZoneSpec{
		DomainName: domain,
		Scope:      &dnspb.ZoneSpec_Vpc{Vpc: &dnspb.VpcZoneScope{PrimaryNetworkId: netID}},
	}
}

// tryZone creates a zone and returns its ID or the call/operation error.
func (e *dnsEnv) tryZone(ctx context.Context, project, name string, spec *dnspb.ZoneSpec) (string, error) {
	op, err := e.zones.Create(ctx, &dnspb.CreateZoneRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: project, Name: name},
		Spec:     spec,
	})
	if err := kmsWait(ctx, op, err); err != nil {
		return "", err
	}
	return op.ResourceID(), nil
}

// tryRecord creates a record and returns its ID or the call/operation error.
func (e *dnsEnv) tryRecord(ctx context.Context, zoneID, name string, spec *dnspb.RecordSpec) (string, error) {
	op, err := e.records.Create(ctx, &dnspb.CreateRecordRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: zoneID, Name: name},
		Spec:     spec,
	})
	if err := kmsWait(ctx, op, err); err != nil {
		return "", err
	}
	return op.ResourceID(), nil
}

func recSpec(rel string, typ dnspb.RecordSpec_RecordType, data string) *dnspb.RecordSpec {
	return &dnspb.RecordSpec{RelativeName: rel, Type: typ, Data: data}
}

// recordWithTTL creates a record with an explicit TTL.
func (e *dnsEnv) recordWithTTL(t *testing.T, zoneID, name, rel string, typ dnspb.RecordSpec_RecordType, data string, ttl int64) *dnspb.Record {
	t.Helper()
	spec := recSpec(rel, typ, data)
	spec.Ttl = ttl
	id, err := e.tryRecord(t.Context(), zoneID, name, spec)
	if err != nil {
		t.Fatalf("create record %s %s: %v", rel, typ, err)
	}
	return e.getRecord(t, id)
}

func (e *dnsEnv) getRecord(t *testing.T, id string) *dnspb.Record {
	t.Helper()
	r, err := e.records.Get(t.Context(), &dnspb.GetRecordRequest{Id: id})
	if err != nil {
		t.Fatalf("get record %s: %v", id, err)
	}
	return r
}

func (e *dnsEnv) getZone(t *testing.T, id string) *dnspb.Zone {
	t.Helper()
	z, err := e.zones.Get(t.Context(), &dnspb.GetZoneRequest{Id: id})
	if err != nil {
		t.Fatalf("get zone %s: %v", id, err)
	}
	return z
}

func (e *dnsEnv) deleteRecord(t *testing.T, id string) {
	t.Helper()
	op, err := e.records.Delete(t.Context(), &dnspb.DeleteRecordRequest{Id: id})
	kmsMustWait(t, op, err)
}

func (e *dnsEnv) deleteZone(t *testing.T, id string) {
	t.Helper()
	op, err := e.zones.Delete(t.Context(), &dnspb.DeleteZoneRequest{Id: id})
	kmsMustWait(t, op, err)
}

// newNet creates a VPC network in the default project.
func (e *dnsEnv) newNet(t *testing.T, name string) string {
	t.Helper()
	op, err := e.nets.Create(t.Context(), &vpcpb.CreateNetworkRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: e.inst.ProjectID, Name: name},
		Spec:     &vpcpb.NetworkSpec{},
	})
	return mustOp(t, op, err)
}
