package contract

import (
	"testing"

	"google.golang.org/grpc/codes"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	dnspb "github.com/nebius/gosdk/proto/nebius/dns/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"
	dnssvc "github.com/nebius/gosdk/services/nebius/dns/v1"
	iamsvc "github.com/nebius/gosdk/services/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
)

const (
	dnsAuditorToken     = "tok-dns-auditor"
	dnsAuditorPrincipal = "tenantuseraccount-e00dnsauditor"
)

func startDNSEnforcing(t *testing.T) *dnsEnv {
	t.Helper()
	return startDNS(t,
		testkit.WithAuthz(authz.ModeEnforce), testkit.WithAuthnRequired(),
		testkit.WithTokens(map[string]string{dnsAuditorToken: dnsAuditorPrincipal}),
	)
}

// dnsAs returns the DNS clients authenticated with the token.
func (e *dnsEnv) dnsAs(t *testing.T, token string) (dnssvc.ZoneService, dnssvc.RecordService) {
	t.Helper()
	sdk := e.inst.SDKAs(t, token)
	return dnssvc.NewZoneService(sdk), dnssvc.NewRecordService(sdk)
}

// grantTenantRole makes principal a member of a new group that holds role on
// the tenant, all as the admin.
func (e *dnsEnv) grantTenantRole(t *testing.T, principal, role string) {
	t.Helper()
	ctx := t.Context()
	sdk := e.inst.SDK(t)
	group := createGroup(t, iamsvc.NewGroupService(sdk), e.inst.TenantID, "dns-"+role, nil)
	op, err := iamsvc.NewGroupMembershipService(sdk).Create(ctx, &iampb.CreateGroupMembershipRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: group.GetMetadata().GetId()},
		Spec:     &iampb.GroupMembershipSpec{MemberId: principal},
	})
	kmsMustWait(t, op, err)
	op, err = iamsvc.NewAccessPermitService(sdk).Create(ctx, &iampb.CreateAccessPermitRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: group.GetMetadata().GetId()},
		Spec:     &iampb.AccessPermitSpec{ResourceId: e.inst.TenantID, Role: role},
	})
	kmsMustWait(t, op, err)
}

func TestDNSViewerReadsButCannotWrite(t *testing.T) {
	t.Parallel()
	e := startDNSEnforcing(t)
	ctx := t.Context()
	z := e.createZone(t, "guarded", "guarded.example.")
	zid := z.GetMetadata().GetId()
	r := e.createRecord(t, zid, "rec", "www", dnspb.RecordSpec_A, "192.0.2.1")
	rid := r.GetMetadata().GetId()
	zones, records := e.dnsAs(t, config.DefaultViewerToken)

	// Reads work.
	if _, err := zones.Get(ctx, &dnspb.GetZoneRequest{Id: zid}); err != nil {
		t.Errorf("zone Get: %v", err)
	}
	if _, err := zones.GetByName(ctx, &commonpb.GetByNameRequest{ParentId: e.inst.ProjectID, Name: "guarded"}); err != nil {
		t.Errorf("zone GetByName: %v", err)
	}
	if l, err := zones.List(ctx, &dnspb.ListZonesRequest{ParentId: e.inst.ProjectID}); err != nil || len(l.GetItems()) != 1 {
		t.Errorf("zone List = %v, %v", l, err)
	}
	if l, err := zones.ListByNetwork(ctx, &dnspb.ListZonesByNetworkRequest{NetworkId: e.netID}); err != nil || len(l.GetItems()) != 1 {
		t.Errorf("zone ListByNetwork = %v, %v", l, err)
	}
	if _, err := records.Get(ctx, &dnspb.GetRecordRequest{Id: rid}); err != nil {
		t.Errorf("record Get: %v", err)
	}
	if _, err := records.GetByName(ctx, &commonpb.GetByNameRequest{ParentId: zid, Name: "rec"}); err != nil {
		t.Errorf("record GetByName: %v", err)
	}
	if l, err := records.List(ctx, &dnspb.ListRecordsRequest{ParentId: zid}); err != nil || len(l.GetItems()) != 3 {
		t.Errorf("record List = %v, %v", l, err)
	}

	// Writes do not.
	op, err := zones.Create(ctx, &dnspb.CreateZoneRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: e.inst.ProjectID, Name: "by-viewer"},
		Spec:     zoneSpec("viewer.example.", e.netID),
	})
	kmsWantCode(t, kmsWait(ctx, op, err), codes.PermissionDenied)
	op, err = zones.Update(ctx, &dnspb.UpdateZoneRequest{
		Metadata: &commonpb.ResourceMetadata{Id: zid, Name: "renamed"}, Spec: zoneSpec("guarded.example.", e.netID),
	})
	kmsWantCode(t, kmsWait(ctx, op, err), codes.PermissionDenied)
	op, err = zones.Delete(ctx, &dnspb.DeleteZoneRequest{Id: zid})
	kmsWantCode(t, kmsWait(ctx, op, err), codes.PermissionDenied)
	op, err = records.Create(ctx, &dnspb.CreateRecordRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: zid}, Spec: recSpec("x", dnspb.RecordSpec_A, "192.0.2.2"),
	})
	kmsWantCode(t, kmsWait(ctx, op, err), codes.PermissionDenied)
	op, err = records.Update(ctx, &dnspb.UpdateRecordRequest{
		Metadata: &commonpb.ResourceMetadata{Id: rid}, Spec: recSpec("www", dnspb.RecordSpec_A, "192.0.2.3"),
	})
	kmsWantCode(t, kmsWait(ctx, op, err), codes.PermissionDenied)
	op, err = records.Delete(ctx, &dnspb.DeleteRecordRequest{Id: rid})
	kmsWantCode(t, kmsWait(ctx, op, err), codes.PermissionDenied)

	if got := e.getRecord(t, rid).GetSpec().GetData(); got != "192.0.2.1" {
		t.Errorf("record data = %q after denied writes", got)
	}
}

func TestDNSEditorWrites(t *testing.T) {
	t.Parallel()
	e := startDNSEnforcing(t)
	ctx := t.Context()
	zones, records := e.dnsAs(t, config.DefaultEditorToken)

	op, err := zones.Create(ctx, &dnspb.CreateZoneRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: e.inst.ProjectID, Name: "by-editor"},
		Spec:     zoneSpec("editor.example.", e.netID),
	})
	kmsMustWait(t, op, err)
	zid := op.ResourceID()

	op, err = zones.Update(ctx, &dnspb.UpdateZoneRequest{
		Metadata: &commonpb.ResourceMetadata{Id: zid, Name: "by-editor-2"}, Spec: zoneSpec("editor.example.", e.netID),
	})
	kmsMustWait(t, op, err)

	op, err = records.Create(ctx, &dnspb.CreateRecordRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: zid}, Spec: recSpec("www", dnspb.RecordSpec_A, "192.0.2.1"),
	})
	kmsMustWait(t, op, err)
	rid := op.ResourceID()
	op, err = records.Update(ctx, &dnspb.UpdateRecordRequest{
		Metadata: &commonpb.ResourceMetadata{Id: rid}, Spec: recSpec("www", dnspb.RecordSpec_A, "192.0.2.2"),
	})
	kmsMustWait(t, op, err)
	op, err = records.Delete(ctx, &dnspb.DeleteRecordRequest{Id: rid})
	kmsMustWait(t, op, err)
	op, err = zones.Delete(ctx, &dnspb.DeleteZoneRequest{Id: zid})
	kmsMustWait(t, op, err)
}

func TestDNSAuditorReadsZonesNotRecords(t *testing.T) {
	t.Parallel()
	e := startDNSEnforcing(t)
	ctx := t.Context()
	z := e.createZone(t, "audited", "audited.example.")
	zid := z.GetMetadata().GetId()
	rid := e.createRecord(t, zid, "rec", "www", dnspb.RecordSpec_A, "192.0.2.1").GetMetadata().GetId()
	zones, records := e.dnsAs(t, dnsAuditorToken)

	// Without a grant the principal sees nothing.
	_, err := zones.List(ctx, &dnspb.ListZonesRequest{ParentId: e.inst.ProjectID})
	kmsWantCode(t, err, codes.PermissionDenied)

	e.grantTenantRole(t, dnsAuditorPrincipal, authz.RoleAuditor)

	if _, err := zones.Get(ctx, &dnspb.GetZoneRequest{Id: zid}); err != nil {
		t.Errorf("auditor zone Get: %v", err)
	}
	if l, err := zones.List(ctx, &dnspb.ListZonesRequest{ParentId: e.inst.ProjectID}); err != nil || len(l.GetItems()) != 1 {
		t.Errorf("auditor zone List = %v, %v", l, err)
	}
	// Records are not readable: a denied Get hides the record (NotFound), a
	// denied List is PermissionDenied.
	_, err = records.Get(ctx, &dnspb.GetRecordRequest{Id: rid})
	kmsWantCode(t, err, codes.NotFound)
	_, err = records.List(ctx, &dnspb.ListRecordsRequest{ParentId: zid})
	kmsWantCode(t, err, codes.PermissionDenied)

	// The auditor changes nothing.
	op, err := zones.Delete(ctx, &dnspb.DeleteZoneRequest{Id: zid})
	kmsWantCode(t, kmsWait(ctx, op, err), codes.PermissionDenied)
}

func TestDNSOutsiderSeesNothing(t *testing.T) {
	t.Parallel()
	e := startDNSEnforcing(t)
	ctx := t.Context()
	zid := e.createZone(t, "private", "private.example.").GetMetadata().GetId()
	zones, records := e.dnsAs(t, dnsAuditorToken) // no grant

	_, err := zones.Get(ctx, &dnspb.GetZoneRequest{Id: zid})
	kmsWantCode(t, err, codes.NotFound)
	_, err = zones.ListByNetwork(ctx, &dnspb.ListZonesByNetworkRequest{NetworkId: e.netID})
	kmsWantCode(t, err, codes.PermissionDenied)
	_, err = records.List(ctx, &dnspb.ListRecordsRequest{ParentId: zid})
	kmsWantCode(t, err, codes.PermissionDenied)
}
