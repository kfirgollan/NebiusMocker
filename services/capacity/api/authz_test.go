package api_test

import (
	"testing"

	"google.golang.org/grpc"

	capacitypb "github.com/nebius/gosdk/proto/nebius/capacity/v1"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/services/capacity/api"
)

func newRules() *authz.Rules {
	r := authz.NewRules()
	api.RegisterAuthzRules(r)
	return r
}

func TestEveryRPCHasARule(t *testing.T) {
	t.Parallel()
	rules := newRules()
	descs := []grpc.ServiceDesc{
		capacitypb.CapacityAllowanceService_ServiceDesc,
		capacitypb.CapacityBlockGroupService_ServiceDesc,
		capacitypb.CapacityIntervalService_ServiceDesc,
		capacitypb.ResourceAdviceService_ServiceDesc,
	}
	for _, d := range descs {
		if len(d.Methods) == 0 {
			t.Fatalf("%s has no methods", d.ServiceName)
		}
		for _, m := range d.Methods {
			full := "/" + d.ServiceName + "/" + m.MethodName
			rule, ok := rules.Lookup(full)
			if !ok {
				t.Errorf("no authz rule for %s", full)
				continue
			}
			if rule.Permission == "" || rule.Target == nil {
				t.Errorf("rule for %s has an empty permission or no target", full)
			}
		}
	}
}

func TestPermissions(t *testing.T) {
	t.Parallel()
	rules := newRules()
	tests := []struct{ method, perm string }{
		{"/nebius.capacity.v1.CapacityAllowanceService/Get", "capacity.allowances.get"},
		{"/nebius.capacity.v1.CapacityAllowanceService/GetByParentAndCapacityBlockGroup", "capacity.allowances.get"},
		{"/nebius.capacity.v1.CapacityAllowanceService/List", "capacity.allowances.list"},
		{"/nebius.capacity.v1.CapacityAllowanceService/ListByCapacityBlockGroup", "capacity.allowances.list"},
		{"/nebius.capacity.v1.CapacityAllowanceService/Create", "capacity.allowances.create"},
		{"/nebius.capacity.v1.CapacityAllowanceService/Update", "capacity.allowances.update"},
		{"/nebius.capacity.v1.CapacityAllowanceService/Delete", "capacity.allowances.delete"},
		{"/nebius.capacity.v1.CapacityBlockGroupService/Get", "capacity.blockGroups.get"},
		{"/nebius.capacity.v1.CapacityBlockGroupService/GetByResourceAffinity", "capacity.blockGroups.get"},
		{"/nebius.capacity.v1.CapacityBlockGroupService/List", "capacity.blockGroups.list"},
		{"/nebius.capacity.v1.CapacityBlockGroupService/ListResources", "capacity.blockGroups.list"},
		{"/nebius.capacity.v1.CapacityIntervalService/Get", "capacity.intervals.get"},
		{"/nebius.capacity.v1.CapacityIntervalService/List", "capacity.intervals.list"},
		{"/nebius.capacity.v1.ResourceAdviceService/List", "capacity.resourceAdvice.list"},
	}
	for _, tt := range tests {
		t.Run(tt.method, func(t *testing.T) {
			t.Parallel()
			rule, ok := rules.Lookup(tt.method)
			if !ok {
				t.Fatal("no rule")
			}
			if got := string(rule.Permission); got != tt.perm {
				t.Errorf("permission = %q, want %q", got, tt.perm)
			}
		})
	}
}

func TestTargets(t *testing.T) {
	t.Parallel()
	rules := newRules()
	tests := []struct {
		name    string
		method  string
		req     any
		want    string
		wantErr bool
	}{
		{
			"allowance create on the project", "/nebius.capacity.v1.CapacityAllowanceService/Create",
			&capacitypb.CreateCapacityAllowanceRequest{Metadata: &commonpb.ResourceMetadata{ParentId: "project-1"}}, "project-1", false,
		},
		{
			"allowance create without parent", "/nebius.capacity.v1.CapacityAllowanceService/Create",
			&capacitypb.CreateCapacityAllowanceRequest{}, "", true,
		},
		{
			"allowance update on its ID", "/nebius.capacity.v1.CapacityAllowanceService/Update",
			&capacitypb.UpdateCapacityAllowanceRequest{Metadata: &commonpb.ResourceMetadata{Id: "capacityallowance-1"}}, "capacityallowance-1", false,
		},
		{
			"allowance get on its ID", "/nebius.capacity.v1.CapacityAllowanceService/Get",
			&capacitypb.GetCapacityAllowanceRequest{Id: "capacityallowance-1"}, "capacityallowance-1", false,
		},
		{
			"allowance list on the project", "/nebius.capacity.v1.CapacityAllowanceService/List",
			&capacitypb.ListCapacityAllowancesRequest{ParentId: "project-1"}, "project-1", false,
		},
		{
			"get by parent and group on the project", "/nebius.capacity.v1.CapacityAllowanceService/GetByParentAndCapacityBlockGroup",
			&capacitypb.GetCapacityAllowanceByParentAndCapacityBlockGroupRequest{ParentId: "project-1", CapacityBlockGroupId: "capacityblockgroup-1"}, "project-1", false,
		},
		{
			"list by group targets the group", "/nebius.capacity.v1.CapacityAllowanceService/ListByCapacityBlockGroup",
			&capacitypb.ListCapacityAllowancesByCapacityBlockGroupRequest{CapacityBlockGroupId: "capacityblockgroup-1"}, "capacityblockgroup-1", false,
		},
		{
			"list by group without group", "/nebius.capacity.v1.CapacityAllowanceService/ListByCapacityBlockGroup",
			&capacitypb.ListCapacityAllowancesByCapacityBlockGroupRequest{}, "", true,
		},
		{
			"block group get on its ID", "/nebius.capacity.v1.CapacityBlockGroupService/Get",
			&capacitypb.GetCapacityBlockGroupRequest{Id: "capacityblockgroup-1"}, "capacityblockgroup-1", false,
		},
		{
			"block group by affinity on the tenant", "/nebius.capacity.v1.CapacityBlockGroupService/GetByResourceAffinity",
			&capacitypb.GetCapacityBlockGroupByResourceAffinityRequest{ParentId: "tenant-1"}, "tenant-1", false,
		},
		{
			"block group list on the tenant", "/nebius.capacity.v1.CapacityBlockGroupService/List",
			&capacitypb.ListCapacityBlockGroupsRequest{ParentId: "tenant-1"}, "tenant-1", false,
		},
		{
			"block group list without tenant", "/nebius.capacity.v1.CapacityBlockGroupService/List",
			&capacitypb.ListCapacityBlockGroupsRequest{}, "", true,
		},
		{
			"resources on the group", "/nebius.capacity.v1.CapacityBlockGroupService/ListResources",
			&capacitypb.ListCapacityBlockGroupResourcesRequest{Id: "capacityblockgroup-1"}, "capacityblockgroup-1", false,
		},
		{
			"interval get on its ID", "/nebius.capacity.v1.CapacityIntervalService/Get",
			&capacitypb.GetCapacityIntervalRequest{Id: "capacityinterval-1"}, "capacityinterval-1", false,
		},
		{
			"interval list on the block group", "/nebius.capacity.v1.CapacityIntervalService/List",
			&capacitypb.ListCapacityIntervalsRequest{ParentId: "capacityblockgroup-1"}, "capacityblockgroup-1", false,
		},
		{
			"advice list on the tenant", "/nebius.capacity.v1.ResourceAdviceService/List",
			&capacitypb.ListResourceAdviceRequest{ParentId: "tenant-1"}, "tenant-1", false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rule, ok := rules.Lookup(tt.method)
			if !ok {
				t.Fatal("no rule")
			}
			got, err := rule.Target(t.Context(), tt.req)
			if tt.wantErr {
				if !errs.IsKind(err, errs.KindBadRequest) {
					t.Fatalf("Target error = %v, want bad_request", err)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("Target = %q, %v; want %q, nil", got, err, tt.want)
			}
		})
	}
}
