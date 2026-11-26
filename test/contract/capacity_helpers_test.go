package contract

import (
	"testing"
	"time"

	"github.com/nebius/gosdk"
	capacitypb "github.com/nebius/gosdk/proto/nebius/capacity/v1"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	capacitysvc "github.com/nebius/gosdk/services/nebius/capacity/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/capacity"
	"github.com/kfirgollan/NebiusMocker/services/iam"
)

// Platforms and fabrics of the seeded block groups (catalog.yaml).
const (
	capH100Platform = "gpu-h100-sxm"
	capH100Fabric   = "fabric-2"
	capH200Platform = "gpu-h200-sxm"
	capH200Fabric   = "fabric-7"
	capB200Platform = "gpu-b200-sxm"
	capB200Fabric   = "us-central1-b"
)

// capacityTestStart is the start of the manual clock capacity tests use: the
// catalog's day offsets are relative to its UTC day.
var capacityTestStart = time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)

// capacityEnv is a mocker with IAM and capacity.
type capacityEnv struct {
	inst *testkit.Instance
	capacityClients
}

// capacityClients are the four capacity services over one SDK.
type capacityClients struct {
	allowances capacitysvc.CapacityAllowanceService
	groups     capacitysvc.CapacityBlockGroupService
	intervals  capacitysvc.CapacityIntervalService
	advice     capacitysvc.ResourceAdviceService
}

func newCapacityClients(sdk *gosdk.SDK) capacityClients {
	return capacityClients{
		allowances: capacitysvc.NewCapacityAllowanceService(sdk),
		groups:     capacitysvc.NewCapacityBlockGroupService(sdk),
		intervals:  capacitysvc.NewCapacityIntervalService(sdk),
		advice:     capacitysvc.NewResourceAdviceService(sdk),
	}
}

func startCapacity(t *testing.T, opts ...testkit.Option) *capacityEnv {
	t.Helper()
	inst := testkit.StartT(t, []mocker.Module{iam.New(), capacity.New()}, opts...)
	return &capacityEnv{inst: inst, capacityClients: newCapacityClients(inst.SDK(t))}
}

// capacityAs returns the capacity clients authenticated with the token.
func (e *capacityEnv) capacityAs(t *testing.T, token string) capacityClients {
	t.Helper()
	return newCapacityClients(e.inst.SDKAs(t, token))
}

// groupByPlatform returns the tenant's block group of the platform.
func (e *capacityEnv) groupByPlatform(t *testing.T, platform string) *capacitypb.CapacityBlockGroup {
	t.Helper()
	list, err := e.groups.List(t.Context(), &capacitypb.ListCapacityBlockGroupsRequest{ParentId: e.inst.TenantID})
	if err != nil {
		t.Fatalf("list block groups: %v", err)
	}
	for _, g := range list.GetItems() {
		if g.GetStatus().GetResourceAffinity().GetComputeV1().GetPlatform() == platform {
			return g
		}
	}
	t.Fatalf("no block group for platform %s", platform)
	return nil
}

// createAllowance creates an allowance in the seeded project (limit nil means
// unlimited) and returns it.
func (e *capacityEnv) createAllowance(t *testing.T, groupID string, limit *uint64) *capacitypb.CapacityAllowance {
	t.Helper()
	op, err := e.allowances.Create(t.Context(), &capacitypb.CreateCapacityAllowanceRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: e.inst.ProjectID},
		Spec:     &capacitypb.CapacityAllowanceSpec{CapacityBlockGroupId: groupID, Limit: limit},
	})
	id := mustOp(t, op, err)
	a, err := e.allowances.Get(t.Context(), &capacitypb.GetCapacityAllowanceRequest{Id: id})
	if err != nil {
		t.Fatalf("get allowance: %v", err)
	}
	return a
}

func capLimit(v uint64) *uint64 { return &v }
