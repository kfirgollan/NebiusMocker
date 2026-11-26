package api

import (
	"context"
	"fmt"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"

	billingpb "github.com/nebius/gosdk/proto/nebius/billing/v1"
	billingv1alpha1pb "github.com/nebius/gosdk/proto/nebius/billing/v1alpha1"
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/serviceerror"
	"github.com/kfirgollan/NebiusMocker/services/billing/domain"
)

// CalculatorHandler serves nebius.billing.v1.CalculatorService.
type CalculatorHandler struct {
	billingpb.UnimplementedCalculatorServiceServer
	calc *domain.Calculator
}

var _ billingpb.CalculatorServiceServer = (*CalculatorHandler)(nil)

// NewCalculatorHandler returns the handler.
func NewCalculatorHandler(calc *domain.Calculator) *CalculatorHandler {
	return &CalculatorHandler{calc: calc}
}

// acceptedSpecTypes are the message names a v1 resource spec may carry.
var acceptedSpecTypes = []string{
	"nebius.compute.v1.CreateInstanceRequest", "nebius.compute.v1.UpdateInstanceRequest",
	"nebius.compute.v1.CreateDiskRequest", "nebius.compute.v1.UpdateDiskRequest",
	"nebius.compute.v1.CreateFilesystemRequest", "nebius.compute.v1.UpdateFilesystemRequest",
}

// EstimateBatch prices resource specs, aggregated over the selected units.
func (h *CalculatorHandler) EstimateBatch(ctx context.Context, req *billingpb.EstimateBatchRequest) (*billingpb.EstimateBatchResponse, error) {
	resp, err := h.estimate(ctx, req)
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return resp, nil
}

func (h *CalculatorHandler) estimate(ctx context.Context, req *billingpb.EstimateBatchRequest) (*billingpb.EstimateBatchResponse, error) {
	items := make([]domain.Item, 0, len(req.GetResourceSpecs()))
	for i, rs := range req.GetResourceSpecs() {
		it, err := itemFromAny(fmt.Sprintf("resource_specs[%d].spec", i), rs.GetSpec())
		if err != nil {
			return nil, err
		}
		items = append(items, it)
	}
	offers := make([]domain.OfferType, 0, len(req.GetOfferTypes()))
	for _, o := range req.GetOfferTypes() {
		offers = append(offers, domain.OfferType(o))
	}
	currency, err := h.calc.Currency(req.GetCurrency())
	if err != nil {
		return nil, err
	}
	var units []domain.TimeUnit
	for _, v := range req.GetFilterAggregationUnit().GetFilterAggregationUnitValues() {
		units = append(units, timeUnit(v))
	}
	units, err = h.calc.Units(units)
	if err != nil {
		return nil, err
	}
	priced, err := h.calc.Price(ctx, items, offers)
	if err != nil {
		return nil, err
	}
	resp := &billingpb.EstimateBatchResponse{Currency: currency}
	for _, p := range priced {
		for _, u := range units {
			resp.ResourceCosts = append(resp.ResourceCosts, &billingpb.ResourceCost{
				Metadata:        proto.CloneOf(p.Metadata),
				AggregationUnit: aggregationUnit(u),
				CostType: &billingpb.ResourceCost_General{General: &billingpb.GeneralResourceCost{
					Total: costBreakdown(domain.Scale(p.Lines, u.Hours())),
				}},
			})
		}
	}
	for _, u := range units {
		var all [][]domain.SkuLine
		for _, p := range priced {
			all = append(all, domain.Scale(p.Lines, u.Hours()))
		}
		resp.TotalCosts = append(resp.TotalCosts, &billingpb.TotalCost{
			AggregationUnit: aggregationUnit(u),
			CostType: &billingpb.TotalCost_General{General: &billingpb.GeneralResourceCost{
				Total: costBreakdown(domain.Merge(all...)),
			}},
		})
	}
	return resp, nil
}

func aggregationUnit(u domain.TimeUnit) *billingpb.AggregationUnit {
	return &billingpb.AggregationUnit{Unit: string(u), UnitType: billingpb.AggregationUnit_UNIT_TYPE_TIME_BASED}
}

func timeUnit(v billingpb.FilterAggregationUnit_FilterAggregationUnitValue) domain.TimeUnit {
	switch v {
	case billingpb.FilterAggregationUnit_FILTER_AGGREGATION_UNIT_MINUTE:
		return domain.UnitMinute
	case billingpb.FilterAggregationUnit_FILTER_AGGREGATION_UNIT_HOUR:
		return domain.UnitHour
	case billingpb.FilterAggregationUnit_FILTER_AGGREGATION_UNIT_DAY:
		return domain.UnitDay
	case billingpb.FilterAggregationUnit_FILTER_AGGREGATION_UNIT_WEEK:
		return domain.UnitWeek
	case billingpb.FilterAggregationUnit_FILTER_AGGREGATION_UNIT_MONTH:
		return domain.UnitMonth
	case billingpb.FilterAggregationUnit_FILTER_AGGREGATION_UNIT_UNSPECIFIED:
		return "" // rejected by Calculator.Units
	default:
		return ""
	}
}

func costBreakdown(lines []domain.SkuLine) *billingpb.CostBreakdown {
	out := &billingpb.CostBreakdown{}
	for i := range lines {
		l := &lines[i]
		out.SkuCosts = append(out.SkuCosts, &billingpb.SkuCost{
			SkuId: l.SKU, Quantity: l.Quantity.String(), QuantityRounded: l.Quantity.Rounded(),
			Cost: l.Cost.String(), CostRounded: l.Cost.Rounded(),
		})
	}
	total := domain.Total(lines)
	out.Cost, out.CostRounded = total.String(), total.Rounded()
	return out
}

// itemFromAny unpacks the google.protobuf.Any of a v1 resource spec.
func itemFromAny(field string, a *anypb.Any) (domain.Item, error) {
	if a == nil {
		return domain.Item{}, errs.BadRequest(errs.Violation{Field: field, Message: "required"})
	}
	accepted := strings.Join(acceptedSpecTypes, ", ")
	msg, err := a.UnmarshalNew()
	if err != nil {
		return domain.Item{}, errs.BadRequest(errs.Violation{
			Field:   field,
			Message: fmt.Sprintf("cannot unpack %q; accepted types: %s", a.GetTypeUrl(), accepted),
		})
	}
	it := domain.Item{Field: field}
	switch m := msg.(type) {
	case *computepb.CreateInstanceRequest:
		it.Metadata, it.Instance = m.GetMetadata(), m.GetSpec()
	case *computepb.UpdateInstanceRequest:
		it.Metadata, it.Instance, it.Update = m.GetMetadata(), m.GetSpec(), true
	case *computepb.CreateDiskRequest:
		it.Metadata, it.Disk = m.GetMetadata(), m.GetSpec()
	case *computepb.UpdateDiskRequest:
		it.Metadata, it.Disk, it.Update = m.GetMetadata(), m.GetSpec(), true
	case *computepb.CreateFilesystemRequest:
		it.Metadata, it.Filesystem = m.GetMetadata(), m.GetSpec()
	case *computepb.UpdateFilesystemRequest:
		it.Metadata, it.Filesystem, it.Update = m.GetMetadata(), m.GetSpec(), true
	default:
		return domain.Item{}, errs.BadRequest(errs.Violation{
			Field:   field,
			Message: fmt.Sprintf("type %s is not priced; accepted types: %s", msg.ProtoReflect().Descriptor().FullName(), accepted),
		})
	}
	return it, nil
}

// CalculatorAlphaHandler serves nebius.billing.v1alpha1.CalculatorService.
type CalculatorAlphaHandler struct {
	billingv1alpha1pb.UnimplementedCalculatorServiceServer
	calc *domain.Calculator
}

var _ billingv1alpha1pb.CalculatorServiceServer = (*CalculatorAlphaHandler)(nil)

// NewCalculatorAlphaHandler returns the handler.
func NewCalculatorAlphaHandler(calc *domain.Calculator) *CalculatorAlphaHandler {
	return &CalculatorAlphaHandler{calc: calc}
}

// Estimate prices one resource spec.
func (h *CalculatorAlphaHandler) Estimate(ctx context.Context, req *billingv1alpha1pb.EstimateRequest) (*billingv1alpha1pb.EstimateResponse, error) {
	item, err := alphaItem("resource_spec", req.GetResourceSpec())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	hourly, monthly, err := h.estimate(ctx, []domain.Item{item}, req.GetOfferTypes())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return &billingv1alpha1pb.EstimateResponse{HourlyCost: hourly, MonthlyCost: monthly}, nil
}

// EstimateBatch prices resource specs and sums them.
func (h *CalculatorAlphaHandler) EstimateBatch(ctx context.Context, req *billingv1alpha1pb.EstimateBatchRequest) (*billingv1alpha1pb.EstimateBatchResponse, error) {
	items := make([]domain.Item, 0, len(req.GetResourceSpecs()))
	for i, rs := range req.GetResourceSpecs() {
		item, err := alphaItem(fmt.Sprintf("resource_specs[%d]", i), rs)
		if err != nil {
			return nil, serviceerror.FromDomain(ctx, err)
		}
		items = append(items, item)
	}
	hourly, monthly, err := h.estimate(ctx, items, req.GetOfferTypes())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return &billingv1alpha1pb.EstimateBatchResponse{HourlyCost: hourly, MonthlyCost: monthly}, nil
}

func (h *CalculatorAlphaHandler) estimate(ctx context.Context, items []domain.Item, offerTypes []billingv1alpha1pb.OfferType) (hourly, monthly *billingv1alpha1pb.ResourceGroupCost, err error) {
	offers := make([]domain.OfferType, 0, len(offerTypes))
	for _, o := range offerTypes {
		offers = append(offers, domain.OfferType(o))
	}
	priced, err := h.calc.Price(ctx, items, offers)
	if err != nil {
		return nil, nil, err
	}
	var sum domain.Decimal
	for _, p := range priced {
		sum = sum.Add(domain.Total(p.Lines))
	}
	return groupCost(sum), groupCost(sum.Mul(domain.DecimalInt(domain.HoursPerMonth))), nil
}

func groupCost(d domain.Decimal) *billingv1alpha1pb.ResourceGroupCost {
	return &billingv1alpha1pb.ResourceGroupCost{CostType: &billingv1alpha1pb.ResourceGroupCost_General{
		General: &billingv1alpha1pb.GeneralTotalCost{Total: &billingv1alpha1pb.CostBreakdown{Cost: d.String(), CostRounded: d.Rounded()}},
	}}
}

// alphaItem converts the resource_spec oneof; the violation path of the item
// names the oneof member.
func alphaItem(path string, rs *billingv1alpha1pb.ResourceSpec) (domain.Item, error) {
	it := domain.Item{}
	switch s := rs.GetResourceSpec().(type) {
	case *billingv1alpha1pb.ResourceSpec_ComputeInstanceSpec:
		it.Field, it.Metadata, it.Instance = path+".compute_instance_spec", s.ComputeInstanceSpec.GetMetadata(), s.ComputeInstanceSpec.GetSpec()
	case *billingv1alpha1pb.ResourceSpec_ComputeInstanceUpdateSpec:
		it.Field, it.Metadata, it.Instance, it.Update = path+".compute_instance_update_spec", s.ComputeInstanceUpdateSpec.GetMetadata(), s.ComputeInstanceUpdateSpec.GetSpec(), true
	case *billingv1alpha1pb.ResourceSpec_ComputeDiskSpec:
		it.Field, it.Metadata, it.Disk = path+".compute_disk_spec", s.ComputeDiskSpec.GetMetadata(), s.ComputeDiskSpec.GetSpec()
	case *billingv1alpha1pb.ResourceSpec_ComputeDiskUpdateSpec:
		it.Field, it.Metadata, it.Disk, it.Update = path+".compute_disk_update_spec", s.ComputeDiskUpdateSpec.GetMetadata(), s.ComputeDiskUpdateSpec.GetSpec(), true
	case *billingv1alpha1pb.ResourceSpec_ComputeFilesystemSpec:
		it.Field, it.Metadata, it.Filesystem = path+".compute_filesystem_spec", s.ComputeFilesystemSpec.GetMetadata(), s.ComputeFilesystemSpec.GetSpec()
	case *billingv1alpha1pb.ResourceSpec_ComputeFilesystemUpdateSpec:
		it.Field, it.Metadata, it.Filesystem, it.Update = path+".compute_filesystem_update_spec", s.ComputeFilesystemUpdateSpec.GetMetadata(), s.ComputeFilesystemUpdateSpec.GetSpec(), true
	default:
		return it, errs.BadRequest(errs.Violation{Field: path, Message: "one of the compute_*_spec fields is required"})
	}
	return it, nil
}
