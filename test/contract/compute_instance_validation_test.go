package contract

import (
	"testing"

	"google.golang.org/grpc/codes"

	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"
	iamsvc "github.com/nebius/gosdk/services/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
)

func TestInstanceValidation(t *testing.T) {
	t.Parallel()
	c := startInstances(t, testkit.WithProjects(config.ProjectSeed{Name: "other"}))
	ctx := t.Context()
	otherProject := c.env.Inst.Projects[1].ID
	sdk := c.env.Inst.SDK(t)
	otherSA := createServiceAccount(t, iamsvc.NewServiceAccountService(sdk), otherProject, "foreign").GetMetadata().GetId()

	tests := []struct {
		name   string
		mutate func(*computepb.InstanceSpec)
		code   codes.Code
		field  string
	}{
		{"unknown platform", func(s *computepb.InstanceSpec) { s.Resources = preset("cpu-nope", "2vcpu-8gb") }, codes.InvalidArgument, "platform"},
		{"preset not in platform", func(s *computepb.InstanceSpec) { s.Resources = preset("cpu-e2", "1gpu-16vcpu-200gb") }, codes.InvalidArgument, "preset"},
		{"platform not in region", func(s *computepb.InstanceSpec) { s.Resources = preset("gpu-b200-sxm", "8gpu-160vcpu-1792gb") }, codes.InvalidArgument, "platform"},
		{"nvl_instance_group_id", func(s *computepb.InstanceSpec) { s.NvlInstanceGroupId = "computenvlinstancegroup-x" }, codes.InvalidArgument, "nvl_instance_group_id"},
		{"spot_pricing_policy", func(s *computepb.InstanceSpec) {
			s.PricingModel = &computepb.InstanceSpec_SpotPricingPolicy{SpotPricingPolicy: &computepb.SpotPricingPolicySpec{Id: "x"}}
		}, codes.InvalidArgument, "spot_pricing_policy"},
		{"reservation ids", func(s *computepb.InstanceSpec) {
			s.ReservationPolicy = &computepb.ReservationPolicy{Policy: computepb.ReservationPolicy_AUTO, ReservationIds: []string{"r1"}}
		}, codes.InvalidArgument, "reservation_ids"},
		{"recovery ALWAYS", func(s *computepb.InstanceSpec) { s.RecoveryPolicy = computepb.InstanceRecoveryPolicy_ALWAYS }, codes.InvalidArgument, "recovery_policy"},
		{"preemptible cpu", func(s *computepb.InstanceSpec) {
			s.Preemptible = &computepb.PreemptibleSpec{OnPreemption: computepb.PreemptibleSpec_STOP}
			s.RecoveryPolicy = computepb.InstanceRecoveryPolicy_FAIL
		}, codes.InvalidArgument, "preemptible"},
		{"no nics", func(s *computepb.InstanceSpec) { s.NetworkInterfaces = nil }, codes.InvalidArgument, "network_interfaces"},
		{"duplicate nic names", func(s *computepb.InstanceSpec) {
			s.NetworkInterfaces = append(s.NetworkInterfaces, nic("eth0", s.GetNetworkInterfaces()[0].GetSubnetId()))
		}, codes.InvalidArgument, "name"},
		{"missing ip_address", func(s *computepb.InstanceSpec) { s.NetworkInterfaces[0].IpAddress = nil }, codes.InvalidArgument, "ip_address"},
		{"missing subnet", func(s *computepb.InstanceSpec) { s.NetworkInterfaces[0].SubnetId = "vpcsubnet-e00doesnotexist" }, codes.NotFound, ""},
		{"missing service account", func(s *computepb.InstanceSpec) { s.ServiceAccountId = "serviceaccount-e00doesnotexist" }, codes.NotFound, ""},
		{"service account of another project", func(s *computepb.InstanceSpec) { s.ServiceAccountId = otherSA }, codes.InvalidArgument, "service_account_id"},
		{"missing boot disk", func(s *computepb.InstanceSpec) { s.BootDisk = nil }, codes.InvalidArgument, "boot_disk"},
		{"missing resources", func(s *computepb.InstanceSpec) { s.Resources = nil }, codes.InvalidArgument, "resources"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			spec := c.cpuSpec("v")
			tc.mutate(spec)
			_, err := c.create(ctx, "v-"+tc.name, spec)
			wantCode(t, tc.name, err, tc.code, tc.field)
		})
	}
	if list, err := c.instances.List(ctx, &computepb.ListInstancesRequest{ParentId: c.project}); err != nil || len(list.GetItems()) != 0 {
		t.Errorf("rejected creates left instances behind: %v %v", list, err)
	}
}
