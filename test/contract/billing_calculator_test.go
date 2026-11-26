package contract

import (
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"

	billingpb "github.com/nebius/gosdk/proto/nebius/billing/v1"
	billingv1alpha1pb "github.com/nebius/gosdk/proto/nebius/billing/v1alpha1"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"
	dnspb "github.com/nebius/gosdk/proto/nebius/dns/v1"
	billingv1 "github.com/nebius/gosdk/services/nebius/billing/v1"
	computesvc "github.com/nebius/gosdk/services/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
)

func billingInstanceReq(project, platform, preset string) *computepb.CreateInstanceRequest {
	// The request validation of compute applies to the spec: a realistic
	// instance has a network interface and a boot disk. The disk is a
	// separate resource, priced on its own.
	spec := &computepb.InstanceSpec{
		Resources: &computepb.ResourcesSpec{Platform: platform, Size: &computepb.ResourcesSpec_Preset{Preset: preset}},
		NetworkInterfaces: []*computepb.NetworkInterfaceSpec{{
			Name: "eth0", SubnetId: "vpcsubnet-e00mock000000000test", IpAddress: &computepb.IPAddress{},
		}},
		BootDisk: &computepb.AttachedDiskSpec{
			AttachMode: computepb.AttachedDiskSpec_READ_WRITE,
			Type:       &computepb.AttachedDiskSpec_ExistingDisk{ExistingDisk: &computepb.ExistingDisk{Id: "computedisk-e00mock000000000test"}},
		},
	}
	return &computepb.CreateInstanceRequest{Metadata: &commonpb.ResourceMetadata{ParentId: project, Name: "vm"}, Spec: spec}
}

// billingSpotInstanceReq is billingInstanceReq asking for a preemptible VM.
func billingSpotInstanceReq(project, platform, preset string) *computepb.CreateInstanceRequest {
	r := billingInstanceReq(project, platform, preset)
	r.Spec.Preemptible = &computepb.PreemptibleSpec{OnPreemption: computepb.PreemptibleSpec_STOP}
	return r
}

func billingDiskReq(project string, typ computepb.DiskSpec_DiskType, gib int64) *computepb.CreateDiskRequest {
	return &computepb.CreateDiskRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: project, Name: "d"},
		Spec:     &computepb.DiskSpec{Type: typ, Size: &computepb.DiskSpec_SizeGibibytes{SizeGibibytes: gib}},
	}
}

func billingFSReq(project string, typ computepb.FilesystemSpec_FilesystemType, gib int64) *computepb.CreateFilesystemRequest {
	return &computepb.CreateFilesystemRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: project, Name: "fs"},
		Spec:     &computepb.FilesystemSpec{Type: typ, Size: &computepb.FilesystemSpec_SizeGibibytes{SizeGibibytes: gib}},
	}
}

func alphaGeneral(t *testing.T, c *billingv1alpha1pb.ResourceGroupCost) (cost, rounded string) {
	t.Helper()
	g := c.GetGeneral().GetTotal()
	if g == nil {
		t.Fatalf("cost has no general total: %v", c)
	}
	return g.GetCost(), g.GetCostRounded()
}

func TestBillingCalculatorAlphaEstimate(t *testing.T) {
	t.Parallel()
	env := startBillingCalc(t)
	project := env.inst.ProjectID
	tests := []struct {
		name            string
		spec            *billingv1alpha1pb.ResourceSpec
		hourly, monthly string
		rounded         string
	}{
		{"h100 8 gpu", &billingv1alpha1pb.ResourceSpec{ResourceSpec: &billingv1alpha1pb.ResourceSpec_ComputeInstanceSpec{
			ComputeInstanceSpec: billingInstanceReq(project, "gpu-h100-sxm", "8gpu-128vcpu-1600gb"),
		}}, "23.6", "16992", "23.60"},
		{"h100 8 gpu preemptible", &billingv1alpha1pb.ResourceSpec{ResourceSpec: &billingv1alpha1pb.ResourceSpec_ComputeInstanceSpec{
			ComputeInstanceSpec: billingSpotInstanceReq(project, "gpu-h100-sxm", "8gpu-128vcpu-1600gb"),
		}}, "10", "7200", "10.00"},
		{"cpu-e2 4vcpu-16gb", &billingv1alpha1pb.ResourceSpec{ResourceSpec: &billingv1alpha1pb.ResourceSpec_ComputeInstanceSpec{
			ComputeInstanceSpec: billingInstanceReq(project, "cpu-e2", "4vcpu-16gb"),
		}}, "0.0832", "59.904", "0.09"},
		{"update instance with explicit project", &billingv1alpha1pb.ResourceSpec{ResourceSpec: &billingv1alpha1pb.ResourceSpec_ComputeInstanceUpdateSpec{
			ComputeInstanceUpdateSpec: &computepb.UpdateInstanceRequest{
				Metadata: &commonpb.ResourceMetadata{ParentId: project, Id: "computeinstance-e00x"},
				Spec:     billingInstanceReq(project, "cpu-e2", "2vcpu-8gb").GetSpec(),
			},
		}}, "0.0416", "29.952", "0.05"},
		{"disk ssd 100 GiB", &billingv1alpha1pb.ResourceSpec{ResourceSpec: &billingv1alpha1pb.ResourceSpec_ComputeDiskSpec{
			ComputeDiskSpec: billingDiskReq(project, computepb.DiskSpec_NETWORK_SSD, 100),
		}}, "0.00986", "7.0992", "0.01"},
		{"disk update spec", &billingv1alpha1pb.ResourceSpec{ResourceSpec: &billingv1alpha1pb.ResourceSpec_ComputeDiskUpdateSpec{
			ComputeDiskUpdateSpec: &computepb.UpdateDiskRequest{
				Metadata: &commonpb.ResourceMetadata{ParentId: project, Id: "computedisk-e00x"},
				Spec:     billingDiskReq(project, computepb.DiskSpec_NETWORK_HDD, 1000).GetSpec(),
			},
		}}, "0.0417", "30.024", "0.05"},
		{"filesystem ssd 1000 GiB", &billingv1alpha1pb.ResourceSpec{ResourceSpec: &billingv1alpha1pb.ResourceSpec_ComputeFilesystemSpec{
			ComputeFilesystemSpec: billingFSReq(project, computepb.FilesystemSpec_NETWORK_SSD, 1000),
		}}, "0.1111", "79.992", "0.12"},
		{"filesystem update spec", &billingv1alpha1pb.ResourceSpec{ResourceSpec: &billingv1alpha1pb.ResourceSpec_ComputeFilesystemUpdateSpec{
			ComputeFilesystemUpdateSpec: &computepb.UpdateFilesystemRequest{
				Metadata: &commonpb.ResourceMetadata{ParentId: project, Id: "computefilesystem-e00x"},
				Spec:     billingFSReq(project, computepb.FilesystemSpec_NETWORK_HDD, 100).GetSpec(),
			},
		}}, "0.00556", "4.0032", "0.01"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			resp, err := env.alpha.Estimate(t.Context(), &billingv1alpha1pb.EstimateRequest{ResourceSpec: tc.spec})
			if err != nil {
				t.Fatal(err)
			}
			hourly, rounded := alphaGeneral(t, resp.GetHourlyCost())
			monthly, _ := alphaGeneral(t, resp.GetMonthlyCost())
			if hourly != tc.hourly || monthly != tc.monthly || rounded != tc.rounded {
				t.Fatalf("hourly %s (%s), monthly %s; want %s (%s), %s", hourly, rounded, monthly, tc.hourly, tc.rounded, tc.monthly)
			}
		})
	}
}

func TestBillingCalculatorAlphaBatchSumsAndOffers(t *testing.T) {
	t.Parallel()
	env := startBillingCalc(t)
	project := env.inst.ProjectID
	specs := []*billingv1alpha1pb.ResourceSpec{
		{ResourceSpec: &billingv1alpha1pb.ResourceSpec_ComputeInstanceSpec{ComputeInstanceSpec: billingInstanceReq(project, "gpu-h100-sxm", "8gpu-128vcpu-1600gb")}},
		{ResourceSpec: &billingv1alpha1pb.ResourceSpec_ComputeDiskSpec{ComputeDiskSpec: billingDiskReq(project, computepb.DiskSpec_NETWORK_SSD, 1000)}},
	}
	resp, err := env.alpha.EstimateBatch(t.Context(), &billingv1alpha1pb.EstimateBatchRequest{
		ResourceSpecs: specs, OfferTypes: []billingv1alpha1pb.OfferType{billingv1alpha1pb.OfferType_OFFER_TYPE_CONTRACT_PRICE},
	})
	if err != nil {
		t.Fatal(err)
	}
	hourly, _ := alphaGeneral(t, resp.GetHourlyCost())
	monthly, _ := alphaGeneral(t, resp.GetMonthlyCost())
	if hourly != "23.6986" || monthly != "17062.992" {
		t.Fatalf("hourly %s, monthly %s; want 23.6986, 17062.992 (contract price changes nothing)", hourly, monthly)
	}
}

// The compute request validation requires metadata.parent_id on update
// specs, so the calculator never has to derive the project from metadata.id.
// TestBillingCalculatorAlphaUpdateSpecProject: an update spec without
// metadata.parent_id is priced in the project of the resource it updates
// (metadata.id); a missing resource is NOT_FOUND.
func TestBillingCalculatorAlphaUpdateSpecProject(t *testing.T) {
	t.Parallel()
	env := startBillingCalc(t)
	disks := computesvc.NewDiskService(env.inst.SDK(t))
	op, err := disks.Create(t.Context(), &computepb.CreateDiskRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: env.inst.ProjectID, Name: "estimate-disk"},
		Spec:     &computepb.DiskSpec{Type: computepb.DiskSpec_NETWORK_SSD, Size: &computepb.DiskSpec_SizeGibibytes{SizeGibibytes: 10}},
	})
	diskID := mustOp(t, op, err)

	estimate := func(id string) (*billingv1alpha1pb.EstimateResponse, error) {
		return env.alpha.Estimate(t.Context(), &billingv1alpha1pb.EstimateRequest{ResourceSpec: &billingv1alpha1pb.ResourceSpec{
			ResourceSpec: &billingv1alpha1pb.ResourceSpec_ComputeDiskUpdateSpec{ComputeDiskUpdateSpec: &computepb.UpdateDiskRequest{
				Metadata: &commonpb.ResourceMetadata{Id: id},
				Spec:     &computepb.DiskSpec{Type: computepb.DiskSpec_NETWORK_SSD, Size: &computepb.DiskSpec_SizeGibibytes{SizeGibibytes: 20}},
			}},
		}})
	}
	if _, err := estimate("computedisk-e00doesnotexist"); status.Code(err) != codes.NotFound {
		t.Fatalf("estimate update of a missing disk: code = %v, want NotFound (err: %v)", status.Code(err), err)
	}
	resp, err := estimate(diskID)
	if err != nil {
		t.Fatal(err)
	}
	if hourly, _ := alphaGeneral(t, resp.GetHourlyCost()); hourly != "0.001972" {
		t.Fatalf("hourly = %s, want 0.001972 (20 GiB SSD)", hourly)
	}
}

func TestBillingCalculatorAlphaErrors(t *testing.T) {
	t.Parallel()
	env := startBillingCalc(t)
	project := env.inst.ProjectID
	inst := func(r *computepb.CreateInstanceRequest) *billingv1alpha1pb.EstimateRequest {
		return &billingv1alpha1pb.EstimateRequest{ResourceSpec: &billingv1alpha1pb.ResourceSpec{ResourceSpec: &billingv1alpha1pb.ResourceSpec_ComputeInstanceSpec{ComputeInstanceSpec: r}}}
	}
	disk := func(r *computepb.CreateDiskRequest) *billingv1alpha1pb.EstimateRequest {
		return &billingv1alpha1pb.EstimateRequest{ResourceSpec: &billingv1alpha1pb.ResourceSpec{ResourceSpec: &billingv1alpha1pb.ResourceSpec_ComputeDiskSpec{ComputeDiskSpec: r}}}
	}
	imageDisk := billingDiskReq(project, computepb.DiskSpec_NETWORK_SSD, 1)
	imageDisk.Spec.Size = nil
	imageDisk.Spec.Source = &computepb.DiskSpec_SourceImageFamily{SourceImageFamily: &computepb.SourceImageFamily{ImageFamily: "ubuntu22.04-driverless"}}
	noPreset := billingInstanceReq(project, "cpu-e2", "4vcpu-16gb")
	noPreset.Spec.Resources.Size = nil

	tests := []struct {
		name  string
		req   *billingv1alpha1pb.EstimateRequest
		code  codes.Code
		field string
	}{
		{"oneof unset", &billingv1alpha1pb.EstimateRequest{ResourceSpec: &billingv1alpha1pb.ResourceSpec{}}, codes.InvalidArgument, "resource_spec"},
		{"resource_spec missing", &billingv1alpha1pb.EstimateRequest{}, codes.InvalidArgument, ""},
		{"unknown platform", inst(billingInstanceReq(project, "cpu-nope", "4vcpu-16gb")), codes.InvalidArgument, "resource_spec.compute_instance_spec.spec.resources.platform"},
		{"unknown preset", inst(billingInstanceReq(project, "cpu-e2", "99vcpu")), codes.InvalidArgument, "resource_spec.compute_instance_spec.spec.resources.preset"},
		{"preset missing", inst(noPreset), codes.InvalidArgument, ""},
		{"preemptible without a market", inst(billingSpotInstanceReq(project, "cpu-e2", "4vcpu-16gb")), codes.InvalidArgument, "resource_spec.compute_instance_spec.spec.preemptible"},
		{"unknown project", inst(billingInstanceReq("project-e00mock000000000ghost", "cpu-e2", "4vcpu-16gb")), codes.NotFound, ""},
		{"parent is not a project", inst(billingInstanceReq("tenant-e00mock0000000000default", "cpu-e2", "4vcpu-16gb")), codes.InvalidArgument, "resource_spec.compute_instance_spec.metadata.parent_id"},
		{"disk sized by its image", disk(imageDisk), codes.InvalidArgument, ""},
		{"offer type unspecified", &billingv1alpha1pb.EstimateRequest{
			ResourceSpec: inst(billingInstanceReq(project, "cpu-e2", "4vcpu-16gb")).GetResourceSpec(),
			OfferTypes:   []billingv1alpha1pb.OfferType{billingv1alpha1pb.OfferType_OFFER_TYPE_UNSPECIFIED},
		}, codes.InvalidArgument, "offer_types[0]"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := env.alpha.Estimate(t.Context(), tc.req)
			assertCode(t, err, tc.code)
			if tc.field != "" {
				billingCalcWantInvalid(t, err, tc.field)
			}
		})
	}
	t.Run("disk sized by its image names spec.size", func(t *testing.T) {
		_, err := env.alpha.Estimate(t.Context(), disk(imageDisk))
		billingCalcWantInvalid(t, err, "resource_spec.compute_disk_spec.spec.size")
	})
	t.Run("empty batch", func(t *testing.T) {
		_, err := env.alpha.EstimateBatch(t.Context(), &billingv1alpha1pb.EstimateBatchRequest{})
		assertCode(t, err, codes.InvalidArgument)
	})
	t.Run("batch names the failing index", func(t *testing.T) {
		_, err := env.alpha.EstimateBatch(t.Context(), &billingv1alpha1pb.EstimateBatchRequest{ResourceSpecs: []*billingv1alpha1pb.ResourceSpec{
			inst(billingInstanceReq(project, "cpu-e2", "4vcpu-16gb")).GetResourceSpec(),
			inst(billingInstanceReq(project, "cpu-nope", "4vcpu-16gb")).GetResourceSpec(),
		}})
		billingCalcWantInvalid(t, err, "resource_specs[1].compute_instance_spec.spec.resources.platform")
	})
}

func billingAny(t *testing.T, m proto.Message) *billingpb.ResourceSpec {
	t.Helper()
	a, err := anypb.New(m)
	if err != nil {
		t.Fatal(err)
	}
	return &billingpb.ResourceSpec{Spec: a}
}

type billingUnitCost struct{ cost, rounded string }

func TestBillingCalculatorV1EstimateBatch(t *testing.T) {
	t.Parallel()
	env := startBillingCalc(t)
	project := env.inst.ProjectID
	vmMeta := &commonpb.ResourceMetadata{ParentId: project, Name: "vm", Labels: map[string]string{"team": "ml"}}
	vm := billingInstanceReq(project, "gpu-h100-sxm", "8gpu-128vcpu-1600gb")
	vm.Metadata = vmMeta
	disk := billingDiskReq(project, computepb.DiskSpec_NETWORK_SSD, 1000)

	resp, err := env.v1.EstimateBatch(t.Context(), &billingpb.EstimateBatchRequest{
		ResourceSpecs: []*billingpb.ResourceSpec{billingAny(t, vm), billingAny(t, disk)},
		OfferTypes:    []billingpb.OfferType{billingpb.OfferType_OFFER_TYPE_CONTRACT_PRICE},
		Currency:      "USD",
		FilterAggregationUnit: &billingpb.FilterAggregationUnit{FilterAggregationUnitValues: []billingpb.FilterAggregationUnit_FilterAggregationUnitValue{
			billingpb.FilterAggregationUnit_FILTER_AGGREGATION_UNIT_HOUR,
			billingpb.FilterAggregationUnit_FILTER_AGGREGATION_UNIT_DAY,
			billingpb.FilterAggregationUnit_FILTER_AGGREGATION_UNIT_WEEK,
			billingpb.FilterAggregationUnit_FILTER_AGGREGATION_UNIT_MONTH,
			billingpb.FilterAggregationUnit_FILTER_AGGREGATION_UNIT_MINUTE,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetCurrency() != "usd" {
		t.Errorf("currency = %q, want usd", resp.GetCurrency())
	}
	wantUnits := []string{"hour", "day", "week", "month", "minute"}
	if len(resp.GetResourceCosts()) != 2*len(wantUnits) || len(resp.GetTotalCosts()) != len(wantUnits) {
		t.Fatalf("%d resource costs, %d totals; want %d and %d", len(resp.GetResourceCosts()), len(resp.GetTotalCosts()), 2*len(wantUnits), len(wantUnits))
	}
	// Resource-major: the VM at each unit, then the disk at each unit.
	vmWant := map[string]billingUnitCost{"hour": {"23.6", "23.60"}, "day": {"566.4", "566.40"}, "week": {"3964.8", "3964.80"}, "month": {"16992", "16992.00"}}
	for i, u := range wantUnits {
		rc := resp.GetResourceCosts()[i]
		if rc.GetAggregationUnit().GetUnit() != u || rc.GetAggregationUnit().GetUnitType() != billingpb.AggregationUnit_UNIT_TYPE_TIME_BASED {
			t.Fatalf("resource cost %d unit = %v, want %s time based", i, rc.GetAggregationUnit(), u)
		}
		if !proto.Equal(rc.GetMetadata(), vmMeta) {
			t.Errorf("resource cost %d metadata = %v, want the request's %v", i, rc.GetMetadata(), vmMeta)
		}
		total := rc.GetGeneral().GetTotal()
		if want, ok := vmWant[u]; ok && (total.GetCost() != want.cost || total.GetCostRounded() != want.rounded) {
			t.Errorf("vm %s cost = %s (%s), want %s (%s)", u, total.GetCost(), total.GetCostRounded(), want.cost, want.rounded)
		}
		sku := total.GetSkuCosts()
		if len(sku) != 1 || sku[0].GetSkuId() == "" || sku[0].GetCost() != total.GetCost() {
			t.Errorf("vm %s sku costs = %v", u, sku)
		}
		if u == "hour" && (sku[0].GetQuantity() != "8" || sku[0].GetQuantityRounded() != "8.00") {
			t.Errorf("vm hour quantity = %s (%s), want 8 GPU-hours", sku[0].GetQuantity(), sku[0].GetQuantityRounded())
		}
		if u == "month" && sku[0].GetQuantity() != "5760" {
			t.Errorf("vm month quantity = %s, want 5760 GPU-hours", sku[0].GetQuantity())
		}
	}
	diskMonth := resp.GetResourceCosts()[len(wantUnits)+3].GetGeneral().GetTotal()
	if diskMonth.GetCost() != "70.992" {
		t.Errorf("disk month cost = %s, want 70.992", diskMonth.GetCost())
	}
	for i, u := range wantUnits {
		tc := resp.GetTotalCosts()[i]
		if tc.GetAggregationUnit().GetUnit() != u {
			t.Fatalf("total %d unit = %s, want %s", i, tc.GetAggregationUnit().GetUnit(), u)
		}
		if u == "hour" {
			total := tc.GetGeneral().GetTotal()
			if total.GetCost() != "23.6986" || len(total.GetSkuCosts()) != 2 {
				t.Errorf("hour total = %s over %d SKUs, want 23.6986 over 2", total.GetCost(), len(total.GetSkuCosts()))
			}
		}
		if u == "month" && tc.GetGeneral().GetTotal().GetCost() != "17062.992" {
			t.Errorf("month total = %s, want 17062.992", tc.GetGeneral().GetTotal().GetCost())
		}
	}
}

func TestBillingCalculatorV1Defaults(t *testing.T) {
	t.Parallel()
	env := startBillingCalc(t)
	project := env.inst.ProjectID
	resp, err := env.v1.EstimateBatch(t.Context(), &billingpb.EstimateBatchRequest{
		ResourceSpecs: []*billingpb.ResourceSpec{billingAny(t, billingInstanceReq(project, "cpu-e2", "4vcpu-16gb"))},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetCurrency() != "usd" || len(resp.GetTotalCosts()) != 2 ||
		resp.GetTotalCosts()[0].GetAggregationUnit().GetUnit() != "hour" || resp.GetTotalCosts()[1].GetAggregationUnit().GetUnit() != "month" {
		t.Fatalf("defaults: currency %q, totals %v; want usd, hour and month", resp.GetCurrency(), resp.GetTotalCosts())
	}
	// Two SKUs: vCPU and memory.
	h := resp.GetResourceCosts()[0].GetGeneral().GetTotal()
	if h.GetCost() != "0.0832" || len(h.GetSkuCosts()) != 2 || h.GetSkuCosts()[0].GetQuantity() != "4" || h.GetSkuCosts()[1].GetQuantity() != "16" {
		t.Fatalf("hourly breakdown = %v", h)
	}
}

func TestBillingCalculatorV1Errors(t *testing.T) {
	t.Parallel()
	env := startBillingCalc(t)
	project := env.inst.ProjectID
	okSpec := billingAny(t, billingInstanceReq(project, "cpu-e2", "4vcpu-16gb"))
	unit := func(v ...billingpb.FilterAggregationUnit_FilterAggregationUnitValue) *billingpb.FilterAggregationUnit {
		return &billingpb.FilterAggregationUnit{FilterAggregationUnitValues: v}
	}
	tests := []struct {
		name  string
		req   *billingpb.EstimateBatchRequest
		code  codes.Code
		field string
	}{
		{"no specs", &billingpb.EstimateBatchRequest{}, codes.InvalidArgument, ""},
		{"empty Any", &billingpb.EstimateBatchRequest{ResourceSpecs: []*billingpb.ResourceSpec{{}}}, codes.InvalidArgument, "resource_specs[0].spec"},
		{"spec type not priced", &billingpb.EstimateBatchRequest{ResourceSpecs: []*billingpb.ResourceSpec{okSpec, billingAny(t, &dnspb.CreateZoneRequest{})}}, codes.InvalidArgument, "resource_specs[1].spec"},
		{"currency not in the price list", &billingpb.EstimateBatchRequest{ResourceSpecs: []*billingpb.ResourceSpec{okSpec}, Currency: "eur"}, codes.InvalidArgument, "currency"},
		{"unit unspecified", &billingpb.EstimateBatchRequest{
			ResourceSpecs:         []*billingpb.ResourceSpec{okSpec},
			FilterAggregationUnit: unit(billingpb.FilterAggregationUnit_FILTER_AGGREGATION_UNIT_DAY, billingpb.FilterAggregationUnit_FILTER_AGGREGATION_UNIT_UNSPECIFIED),
		}, codes.InvalidArgument, "filter_aggregation_unit.filter_aggregation_unit_values[1]"},
		{"offer type unspecified", &billingpb.EstimateBatchRequest{
			ResourceSpecs: []*billingpb.ResourceSpec{okSpec}, OfferTypes: []billingpb.OfferType{billingpb.OfferType_OFFER_TYPE_UNSPECIFIED},
		}, codes.InvalidArgument, "offer_types[0]"},
		{"unknown platform", &billingpb.EstimateBatchRequest{ResourceSpecs: []*billingpb.ResourceSpec{
			billingAny(t, billingInstanceReq(project, "cpu-nope", "4vcpu-16gb")),
		}}, codes.InvalidArgument, "resource_specs[0].spec.spec.resources.platform"},
		{"unknown project", &billingpb.EstimateBatchRequest{ResourceSpecs: []*billingpb.ResourceSpec{
			billingAny(t, billingDiskReq("project-e00mock000000000ghost", computepb.DiskSpec_NETWORK_SSD, 1)),
		}}, codes.NotFound, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := env.v1.EstimateBatch(t.Context(), tc.req)
			assertCode(t, err, tc.code)
			if tc.field != "" {
				billingCalcWantInvalid(t, err, tc.field)
			}
		})
	}
}

func TestBillingCalculatorAccepts(t *testing.T) {
	t.Parallel()
	env := startBillingCalc(t)
	project := env.inst.ProjectID
	// Every accepted v1 spec type prices.
	for name, m := range map[string]proto.Message{
		"CreateInstanceRequest":   billingInstanceReq(project, "cpu-e2", "2vcpu-8gb"),
		"UpdateInstanceRequest":   &computepb.UpdateInstanceRequest{Metadata: &commonpb.ResourceMetadata{ParentId: project}, Spec: billingInstanceReq(project, "cpu-e2", "2vcpu-8gb").GetSpec()},
		"CreateDiskRequest":       billingDiskReq(project, computepb.DiskSpec_NETWORK_SSD, 5),
		"UpdateDiskRequest":       &computepb.UpdateDiskRequest{Metadata: &commonpb.ResourceMetadata{ParentId: project}, Spec: billingDiskReq(project, computepb.DiskSpec_NETWORK_SSD, 5).GetSpec()},
		"CreateFilesystemRequest": billingFSReq(project, computepb.FilesystemSpec_NETWORK_HDD, 5),
		"UpdateFilesystemRequest": &computepb.UpdateFilesystemRequest{Metadata: &commonpb.ResourceMetadata{ParentId: project}, Spec: billingFSReq(project, computepb.FilesystemSpec_NETWORK_HDD, 5).GetSpec()},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			resp, err := env.v1.EstimateBatch(t.Context(), &billingpb.EstimateBatchRequest{ResourceSpecs: []*billingpb.ResourceSpec{billingAny(t, m)}})
			if err != nil {
				t.Fatal(err)
			}
			if resp.GetTotalCosts()[0].GetGeneral().GetTotal().GetCost() == "" {
				t.Fatalf("no cost: %v", resp)
			}
		})
	}
}

func TestBillingCalculatorAuth(t *testing.T) {
	t.Parallel()
	env := startBillingCalc(t, testkit.WithAuthz(authz.ModeEnforce), testkit.WithAuthnRequired())
	project := env.inst.ProjectID
	v1req := &billingpb.EstimateBatchRequest{ResourceSpecs: []*billingpb.ResourceSpec{billingAny(t, billingDiskReq(project, computepb.DiskSpec_NETWORK_SSD, 10))}}
	alphaReq := &billingv1alpha1pb.EstimateRequest{ResourceSpec: &billingv1alpha1pb.ResourceSpec{ResourceSpec: &billingv1alpha1pb.ResourceSpec_ComputeDiskSpec{ComputeDiskSpec: billingDiskReq(project, computepb.DiskSpec_NETWORK_SSD, 10)}}}

	_, err := env.anonCalc.EstimateBatch(t.Context(), v1req)
	assertCode(t, err, codes.Unauthenticated)
	_, err = env.anonAlpha.Estimate(t.Context(), alphaReq)
	assertCode(t, err, codes.Unauthenticated)
	_, err = env.anonAlpha.EstimateBatch(t.Context(), &billingv1alpha1pb.EstimateBatchRequest{ResourceSpecs: []*billingv1alpha1pb.ResourceSpec{alphaReq.GetResourceSpec()}})
	assertCode(t, err, codes.Unauthenticated)

	// Any authenticated principal may estimate, even a read-only viewer.
	viewer := billingv1.NewCalculatorService(env.inst.SDKAs(t, "nmock-dev-viewer"))
	if _, err := viewer.EstimateBatch(t.Context(), v1req); err != nil {
		t.Fatalf("viewer estimate: %v", err)
	}
	if _, err := env.v1.EstimateBatch(t.Context(), v1req); err != nil {
		t.Fatalf("admin estimate: %v", err)
	}
}
