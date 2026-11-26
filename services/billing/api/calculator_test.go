package api_test

import (
	"context"
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

	"github.com/kfirgollan/NebiusMocker/services/billing/api"
	"github.com/kfirgollan/NebiusMocker/services/billing/domain"
)

type apiNoRegions struct{}

func (apiNoRegions) Region(context.Context, string) (string, error) { return "eu-north1", nil }

type apiNoPlatforms struct{}

func (apiNoPlatforms) Platform(context.Context, string, string) (domain.Platform, bool, error) {
	return domain.Platform{}, false, nil
}

type apiNoHierarchy struct{}

func (apiNoHierarchy) Parent(context.Context, string) (string, error) { return "", nil }

const apiProject = "project-e00mock000000000default"

func newAPICalc(t *testing.T) *domain.Calculator {
	t.Helper()
	pl, err := domain.DefaultPriceList([]string{"eu-north1"}, "e00")
	if err != nil {
		t.Fatal(err)
	}
	c, err := domain.NewCalculator(domain.CalculatorDeps{
		Prices: pl, Regions: apiNoRegions{}, Platforms: apiNoPlatforms{}, Hierarchy: apiNoHierarchy{},
		Config: domain.Config{ProjectID: apiProject},
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func apiDisk(gib int64) *computepb.CreateDiskRequest {
	return &computepb.CreateDiskRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: apiProject, Name: "d", Labels: map[string]string{"a": "b"}},
		Spec:     &computepb.DiskSpec{Type: computepb.DiskSpec_NETWORK_SSD, Size: &computepb.DiskSpec_SizeGibibytes{SizeGibibytes: gib}},
	}
}

func TestV1ResourceCostsCloneMetadataAndFillBreakdown(t *testing.T) {
	h := api.NewCalculatorHandler(newAPICalc(t))
	d := apiDisk(100)
	a, err := anypb.New(d)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := h.EstimateBatch(t.Context(), &billingpb.EstimateBatchRequest{
		ResourceSpecs: []*billingpb.ResourceSpec{{Spec: a}},
		FilterAggregationUnit: &billingpb.FilterAggregationUnit{FilterAggregationUnitValues: []billingpb.FilterAggregationUnit_FilterAggregationUnitValue{
			billingpb.FilterAggregationUnit_FILTER_AGGREGATION_UNIT_MINUTE,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	rc := resp.GetResourceCosts()[0]
	if !proto.Equal(rc.GetMetadata(), d.GetMetadata()) || rc.GetMetadata() == d.GetMetadata() {
		t.Error("metadata must be an equal copy of the request's")
	}
	sku := rc.GetGeneral().GetTotal().GetSkuCosts()[0]
	// 100 GiB x 0.0000986 per hour, per minute is 1/60 of that: rounded up.
	if sku.GetQuantityRounded() != "1.67" || sku.GetCostRounded() != "0.01" {
		t.Errorf("sku cost = %v", sku)
	}
	if resp.GetTotalCosts()[0].GetAggregationUnit().GetUnit() != "minute" {
		t.Errorf("total unit = %v", resp.GetTotalCosts()[0].GetAggregationUnit())
	}
}

func TestV1RejectsUnlistedSpecTypesWithBadRequest(t *testing.T) {
	h := api.NewCalculatorHandler(newAPICalc(t))
	a, err := anypb.New(&dnspb.CreateZoneRequest{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.EstimateBatch(t.Context(), &billingpb.EstimateBatchRequest{ResourceSpecs: []*billingpb.ResourceSpec{{Spec: a}}})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("err = %v, want InvalidArgument", err)
	}
	// An Any of an unknown type URL cannot be unpacked at all.
	_, err = h.EstimateBatch(t.Context(), &billingpb.EstimateBatchRequest{
		ResourceSpecs: []*billingpb.ResourceSpec{{Spec: &anypb.Any{TypeUrl: "type.googleapis.com/nebius.nope.Thing"}}},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("err = %v, want InvalidArgument", err)
	}
}

func TestAlphaOneofSelectsTheSpecKind(t *testing.T) {
	h := api.NewCalculatorAlphaHandler(newAPICalc(t))
	disk := apiDisk(1000)
	fs := &computepb.CreateFilesystemRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: apiProject},
		Spec:     &computepb.FilesystemSpec{Type: computepb.FilesystemSpec_NETWORK_SSD, Size: &computepb.FilesystemSpec_SizeGibibytes{SizeGibibytes: 1000}},
	}
	for name, tc := range map[string]struct {
		spec *billingv1alpha1pb.ResourceSpec
		want string
	}{
		"disk":       {&billingv1alpha1pb.ResourceSpec{ResourceSpec: &billingv1alpha1pb.ResourceSpec_ComputeDiskSpec{ComputeDiskSpec: disk}}, "0.0986"},
		"filesystem": {&billingv1alpha1pb.ResourceSpec{ResourceSpec: &billingv1alpha1pb.ResourceSpec_ComputeFilesystemSpec{ComputeFilesystemSpec: fs}}, "0.1111"},
	} {
		t.Run(name, func(t *testing.T) {
			resp, err := h.Estimate(t.Context(), &billingv1alpha1pb.EstimateRequest{ResourceSpec: tc.spec})
			if err != nil {
				t.Fatal(err)
			}
			if got := resp.GetHourlyCost().GetGeneral().GetTotal().GetCost(); got != tc.want {
				t.Fatalf("hourly = %s, want %s", got, tc.want)
			}
		})
	}
	if _, err := h.Estimate(t.Context(), &billingv1alpha1pb.EstimateRequest{ResourceSpec: &billingv1alpha1pb.ResourceSpec{}}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("unset oneof: %v, want InvalidArgument", err)
	}
}
