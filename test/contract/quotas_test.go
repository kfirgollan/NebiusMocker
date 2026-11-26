package contract

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	quotaspb "github.com/nebius/gosdk/proto/nebius/quotas/v1"
	vpcpb "github.com/nebius/gosdk/proto/nebius/vpc/v1"
	"github.com/nebius/gosdk/serviceerror"
	quotassvc "github.com/nebius/gosdk/services/nebius/quotas/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/quota"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/services/quotas"
	"github.com/kfirgollan/NebiusMocker/services/vpc"
)

const quotaRegion = "eu-north1"

type quotaEnv struct {
	inst   *testkit.Instance
	quotas quotassvc.QuotaAllowanceService
	vpc    vpcClients
}

// startQuotas runs IAM, VPC and quotas under authz enforce; the dev token is
// a tenant admin.
func startQuotas(t *testing.T) quotaEnv {
	t.Helper()
	inst := testkit.StartT(t, []mocker.Module{iam.New(), vpc.New(), quotas.New()},
		testkit.WithAuthz(authz.ModeEnforce))
	return quotaEnv{
		inst:   inst,
		quotas: quotassvc.NewQuotaAllowanceService(inst.SDK(t)),
		vpc:    vpcClientsFor(t, inst, inst.Token),
	}
}

func (e quotaEnv) byName(t *testing.T, parent, name string) *quotaspb.QuotaAllowance {
	t.Helper()
	a, err := e.quotas.GetByName(t.Context(), &quotaspb.GetByNameRequest{ParentId: parent, Name: name, Region: quotaRegion})
	if err != nil {
		t.Fatalf("GetByName(%s, %s): %v", parent, name, err)
	}
	return a
}

// setLimit sets an allowance through Create (an upsert) and returns its ID.
func (e quotaEnv) setLimit(t *testing.T, parent, name string, limit uint64) string {
	t.Helper()
	spec := &quotaspb.QuotaAllowanceSpec{Limit: proto.Uint64(limit)}
	if parent == e.inst.TenantID {
		spec.Region = quotaRegion
	}
	return wait(t)(e.quotas.Create(t.Context(), &quotaspb.CreateQuotaAllowanceRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: parent, Name: name}, Spec: spec,
	}))
}

func (e quotaEnv) createNetwork(ctx context.Context, name string) error {
	return waitOK(ctx)(e.vpc.networks.Create(ctx, &vpcpb.CreateNetworkRequest{
		Metadata: md(e.inst.ProjectID, name), Spec: &vpcpb.NetworkSpec{},
	}))
}

// quotaFailure returns the QuotaFailure detail of err, as gosdk decodes it.
func quotaFailure(t *testing.T, err error) *serviceerror.QuotaFailure {
	t.Helper()
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("err = %v, want RESOURCE_EXHAUSTED", err)
	}
	se, ok := errors.AsType[*serviceerror.Error](serviceerror.FromError(err))
	if !ok {
		t.Fatalf("err %v carries no ServiceError", err)
	}
	for _, d := range se.Details {
		if qf, ok := d.(*serviceerror.QuotaFailure); ok {
			return qf
		}
	}
	t.Fatalf("err %v has no QuotaFailure detail: %v", err, se.Details)
	return nil
}

func TestQuotaAllowanceLifecycle(t *testing.T) {
	t.Parallel()
	e := startQuotas(t)
	ctx := t.Context()

	// List: every catalog quota, in the project's region, with defaults.
	var all []*quotaspb.QuotaAllowance
	token := ""
	for {
		page, err := e.quotas.List(ctx, &quotaspb.ListQuotaAllowancesRequest{ParentId: e.inst.ProjectID, PageSize: 7, PageToken: token})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		all = append(all, page.GetItems()...)
		if token = page.GetNextPageToken(); token == "" {
			break
		}
	}
	catalog, err := quota.DefaultCatalog(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != len(catalog.Entries()) {
		t.Fatalf("List returned %d allowances, want %d", len(all), len(catalog.Entries()))
	}
	var net *quotaspb.QuotaAllowance
	for _, a := range all {
		if a.GetSpec().GetRegion() != quotaRegion || a.GetStatus().GetState() != quotaspb.QuotaAllowanceStatus_STATE_ACTIVE {
			t.Fatalf("listed allowance = %v", a)
		}
		if a.GetMetadata().GetName() == quota.VPCNetworkCount {
			net = a
		}
	}
	if net == nil || net.GetSpec().GetLimit() != 1000 || net.GetStatus().GetService() != "vpc" ||
		net.GetStatus().GetUnit() != quota.UnitCount || net.GetStatus().GetDescription() != "Networks" {
		t.Fatalf("vpc.network.count = %v", net)
	}
	id := net.GetMetadata().GetId()

	// GetByName and Get agree; usage is live.
	base := e.byName(t, e.inst.ProjectID, quota.VPCNetworkCount).GetStatus().GetUsage()
	if err := e.createNetwork(ctx, "q-net"); err != nil {
		t.Fatal(err)
	}
	got, err := e.quotas.Get(ctx, &quotaspb.GetQuotaAllowanceRequest{Id: id})
	if err != nil || got.GetStatus().GetUsage() != base+1 ||
		got.GetStatus().GetUsageState() != quotaspb.QuotaAllowanceStatus_USAGE_STATE_USED {
		t.Fatalf("Get = %v, %v; want usage %d", got, err, base+1)
	}

	// Create upserts the existing allowance (same ID); region defaults to the project's.
	if gotID := e.setLimit(t, e.inst.ProjectID, quota.VPCNetworkCount, 20); gotID != id {
		t.Fatalf("Create returned %s, want the existing allowance %s", gotID, id)
	}
	got = e.byName(t, e.inst.ProjectID, quota.VPCNetworkCount)
	if got.GetSpec().GetLimit() != 20 || got.GetMetadata().GetResourceVersion() != 2 ||
		got.GetStatus().GetUsagePercentage() != fmt.Sprintf("%.2f", float64(base+1)/20) {
		t.Fatalf("after Create = %v", got)
	}

	// Update changes the limit; region is immutable; resource_version guards.
	err = waitOK(ctx)(e.quotas.Update(ctx, &quotaspb.UpdateQuotaAllowanceRequest{
		Metadata: &commonpb.ResourceMetadata{Id: id, Labels: map[string]string{"team": "net"}},
		Spec:     &quotaspb.QuotaAllowanceSpec{Limit: proto.Uint64(30)},
	}))
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, _ = e.quotas.Get(ctx, &quotaspb.GetQuotaAllowanceRequest{Id: id})
	if got.GetSpec().GetLimit() != 30 || got.GetMetadata().GetLabels()["team"] != "net" {
		t.Fatalf("after Update = %v", got)
	}
	_, err = e.quotas.Update(ctx, &quotaspb.UpdateQuotaAllowanceRequest{
		Metadata: &commonpb.ResourceMetadata{Id: id}, Spec: &quotaspb.QuotaAllowanceSpec{Limit: proto.Uint64(1), Region: "eu-west1"},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("Update region: %v, want InvalidArgument", err)
	}
	_, err = e.quotas.Update(ctx, &quotaspb.UpdateQuotaAllowanceRequest{
		Metadata: &commonpb.ResourceMetadata{Id: id, ResourceVersion: 1}, Spec: &quotaspb.QuotaAllowanceSpec{Limit: proto.Uint64(1)},
	})
	if status.Code(err) != codes.Aborted {
		t.Errorf("stale Update: %v, want Aborted", err)
	}

	// Delete resets to the default and the allowance remains.
	if err := waitOK(ctx)(e.quotas.Delete(ctx, &quotaspb.DeleteQuotaAllowanceRequest{Id: id})); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	got, err = e.quotas.Get(ctx, &quotaspb.GetQuotaAllowanceRequest{Id: id})
	if err != nil || got.GetSpec().GetLimit() != 1000 {
		t.Fatalf("after Delete = %v, %v", got, err)
	}

	// Tenant allowances exist in every region; a tenant must name one.
	if a := e.byName(t, e.inst.TenantID, quota.VPCNetworkCount); a.GetStatus().GetUsage() < base+1 {
		t.Errorf("tenant usage %d < project usage %d", a.GetStatus().GetUsage(), base+1)
	}
	for _, bad := range []*quotaspb.GetByNameRequest{
		{ParentId: e.inst.ProjectID, Name: "no.such.quota", Region: quotaRegion},
		{ParentId: e.inst.ProjectID, Name: quota.VPCNetworkCount, Region: "eu-west1"},
	} {
		if _, err := e.quotas.GetByName(ctx, bad); status.Code(err) != codes.InvalidArgument {
			t.Errorf("GetByName(%v): %v, want InvalidArgument", bad, err)
		}
	}
	_, err = e.quotas.Create(ctx, &quotaspb.CreateQuotaAllowanceRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: e.inst.TenantID, Name: quota.VPCNetworkCount},
		Spec:     &quotaspb.QuotaAllowanceSpec{Limit: proto.Uint64(5)},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("tenant Create without region: %v, want InvalidArgument", err)
	}
	if _, err := e.quotas.Get(ctx, &quotaspb.GetQuotaAllowanceRequest{Id: "quotaallowance-e00nosuchallowance00"}); status.Code(err) != codes.NotFound {
		t.Errorf("Get unknown: %v, want NotFound", err)
	}
}

func TestQuotaEnforcementAndRelease(t *testing.T) {
	t.Parallel()
	e := startQuotas(t)
	ctx := t.Context()
	base := e.byName(t, e.inst.ProjectID, quota.VPCNetworkCount).GetStatus().GetUsage()
	e.setLimit(t, e.inst.ProjectID, quota.VPCNetworkCount, base+1)

	if err := e.createNetwork(ctx, "fits"); err != nil {
		t.Fatalf("create within limit: %v", err)
	}
	err := e.createNetwork(ctx, "over")
	qf := quotaFailure(t, err)
	v := qf.Violations()
	if len(v) != 1 || v[0].GetQuota() != quota.VPCNetworkCount || v[0].GetLimit() != strconv.FormatUint(base+1, 10) ||
		v[0].GetRequested() != "1" || v[0].GetRegion() != quotaRegion {
		t.Fatalf("violations = %v", v)
	}
	if _, err := e.vpc.networks.GetByName(ctx, &vpcpb.GetNetworkByNameRequest{ParentId: e.inst.ProjectID, Name: "over"}); status.Code(err) != codes.NotFound {
		t.Fatalf("refused network exists: %v", err)
	}
	if u := e.byName(t, e.inst.ProjectID, quota.VPCNetworkCount).GetStatus().GetUsage(); u != base+1 {
		t.Fatalf("usage = %d, want %d", u, base+1)
	}

	// Deleting a network releases its share.
	n, err := e.vpc.networks.GetByName(ctx, &vpcpb.GetNetworkByNameRequest{ParentId: e.inst.ProjectID, Name: "fits"})
	if err != nil {
		t.Fatal(err)
	}
	if err := waitOK(ctx)(e.vpc.networks.Delete(ctx, &vpcpb.DeleteNetworkRequest{Id: n.GetMetadata().GetId()})); err != nil {
		t.Fatalf("delete network: %v", err)
	}
	if err := e.createNetwork(ctx, "after-release"); err != nil {
		t.Fatalf("create after release: %v", err)
	}

	// Raising the allowance admits more; the tenant allowance applies too.
	e.setLimit(t, e.inst.ProjectID, quota.VPCNetworkCount, base+10)
	e.setLimit(t, e.inst.TenantID, quota.VPCNetworkCount, e.byName(t, e.inst.TenantID, quota.VPCNetworkCount).GetStatus().GetUsage())
	qf = quotaFailure(t, e.createNetwork(ctx, "tenant-over"))
	if v := qf.Violations(); len(v) != 1 || v[0].GetQuota() != quota.VPCNetworkCount {
		t.Fatalf("tenant violations = %v", v)
	}
}

func TestQuotaParallelCreatesAreRaceSafe(t *testing.T) {
	t.Parallel()
	e := startQuotas(t)
	ctx := t.Context()
	base := e.byName(t, e.inst.ProjectID, quota.VPCNetworkCount).GetStatus().GetUsage()
	const room, attempts = 3, 12
	e.setLimit(t, e.inst.ProjectID, quota.VPCNetworkCount, base+room)

	var ok, exhausted atomic.Int32
	var wg sync.WaitGroup
	for i := range attempts {
		wg.Go(func() {
			err := e.createNetwork(ctx, fmt.Sprintf("par-%d", i))
			switch {
			case err == nil:
				ok.Add(1)
			case status.Code(err) == codes.ResourceExhausted:
				exhausted.Add(1)
			default:
				t.Errorf("create par-%d: %v", i, err)
			}
		})
	}
	wg.Wait()
	if ok.Load() != room || exhausted.Load() != attempts-room {
		t.Fatalf("%d created, %d refused; want %d and %d", ok.Load(), exhausted.Load(), room, attempts-room)
	}
	if u := e.byName(t, e.inst.ProjectID, quota.VPCNetworkCount).GetStatus().GetUsage(); u != base+room {
		t.Fatalf("usage = %d, want %d", u, base+room)
	}
}

func TestQuotaAllowanceAuthz(t *testing.T) {
	t.Parallel()
	e := startQuotas(t)
	ctx := t.Context()
	id := e.byName(t, e.inst.ProjectID, quota.VPCNetworkCount).GetMetadata().GetId()
	create := &quotaspb.CreateQuotaAllowanceRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: e.inst.ProjectID, Name: quota.VPCNetworkCount},
		Spec:     &quotaspb.QuotaAllowanceSpec{Limit: proto.Uint64(7)},
	}
	for _, tok := range []string{config.DefaultViewerToken, config.DefaultEditorToken} {
		c := quotassvc.NewQuotaAllowanceService(e.inst.SDKAs(t, tok))
		if _, err := c.Get(ctx, &quotaspb.GetQuotaAllowanceRequest{Id: id}); err != nil {
			t.Errorf("%s Get: %v", tok, err)
		}
		if _, err := c.List(ctx, &quotaspb.ListQuotaAllowancesRequest{ParentId: e.inst.ProjectID}); err != nil {
			t.Errorf("%s List: %v", tok, err)
		}
		if _, err := c.GetByName(ctx, &quotaspb.GetByNameRequest{ParentId: e.inst.ProjectID, Name: quota.VPCNetworkCount, Region: quotaRegion}); err != nil {
			t.Errorf("%s GetByName: %v", tok, err)
		}
		// Only admins change quotas (docs.nebius.com/overview/quotas).
		if _, err := c.Create(ctx, create); status.Code(err) != codes.PermissionDenied {
			t.Errorf("%s Create: %v, want PermissionDenied", tok, err)
		}
		if _, err := c.Update(ctx, &quotaspb.UpdateQuotaAllowanceRequest{
			Metadata: &commonpb.ResourceMetadata{Id: id}, Spec: &quotaspb.QuotaAllowanceSpec{Limit: proto.Uint64(7)},
		}); status.Code(err) != codes.PermissionDenied {
			t.Errorf("%s Update: %v, want PermissionDenied", tok, err)
		}
		if _, err := c.Delete(ctx, &quotaspb.DeleteQuotaAllowanceRequest{Id: id}); status.Code(err) != codes.PermissionDenied {
			t.Errorf("%s Delete: %v, want PermissionDenied", tok, err)
		}
	}
	if got := e.setLimit(t, e.inst.ProjectID, quota.VPCNetworkCount, 7); got != id {
		t.Fatalf("admin Create = %s, want %s", got, id)
	}
}
