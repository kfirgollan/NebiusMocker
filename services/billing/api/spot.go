package api

import (
	"context"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	billingv1alpha1pb "github.com/nebius/gosdk/proto/nebius/billing/v1alpha1"

	"github.com/kfirgollan/NebiusMocker/pkg/serviceerror"
	"github.com/kfirgollan/NebiusMocker/services/billing/domain"
)

// SpotPriceHandler serves nebius.billing.v1alpha1.SpotPriceService.
type SpotPriceHandler struct {
	billingv1alpha1pb.UnimplementedSpotPriceServiceServer
	svc *domain.SpotService
}

var _ billingv1alpha1pb.SpotPriceServiceServer = (*SpotPriceHandler)(nil)

// NewSpotPriceHandler returns the handler.
func NewSpotPriceHandler(svc *domain.SpotService) *SpotPriceHandler {
	return &SpotPriceHandler{svc: svc}
}

// List returns one page of the current spot prices.
func (h *SpotPriceHandler) List(ctx context.Context, req *billingv1alpha1pb.ListSpotPricesRequest) (*billingv1alpha1pb.ListSpotPricesResponse, error) {
	page, snapshot, err := h.svc.List(req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	resp := &billingv1alpha1pb.ListSpotPricesResponse{NextPageToken: page.NextPageToken, SnapshotTime: timestamppb.New(snapshot)}
	for i := range page.Items {
		p := &page.Items[i]
		resp.Items = append(resp.Items, &billingv1alpha1pb.SpotPrice{
			SkuId: p.SKU, ProductTitle: p.ProductTitle, Region: p.Region,
			ComputeInstance: &billingv1alpha1pb.SpotPriceComputeInstance{Platform: p.Platform},
			Price:           &billingv1alpha1pb.SpotPriceValue{Value: p.Price.String(), Currency: p.Currency, UnitTitle: p.UnitTitle},
			EffectiveTime:   timestamppb.New(p.EffectiveTime),
		})
	}
	return resp, nil
}

// ListSeries returns one page of spot price series.
func (h *SpotPriceHandler) ListSeries(ctx context.Context, req *billingv1alpha1pb.ListSpotPriceSeriesRequest) (*billingv1alpha1pb.ListSpotPriceSeriesResponse, error) {
	q := domain.SeriesQuery{
		SKUIDs:      req.GetFilters().GetSkuIds(),
		Start:       optTime(req.GetFilters().GetStartTime()),
		End:         optTime(req.GetFilters().GetEndTime()),
		Granularity: granularity(req.GetAggregation().GetGranularity()),
		Method:      method(req.GetAggregation().GetMethod()),
		PageSize:    req.GetPageSize(),
		PageToken:   req.GetPageToken(),
	}
	page, snapshot, err := h.svc.ListSeries(q)
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	resp := &billingv1alpha1pb.ListSpotPriceSeriesResponse{NextPageToken: page.NextPageToken, SnapshotTime: timestamppb.New(snapshot)}
	for _, s := range page.Items {
		out := &billingv1alpha1pb.SpotPriceSeries{
			SkuId: s.SKU, ProductTitle: s.ProductTitle, Region: s.Region,
			ComputeInstance: &billingv1alpha1pb.SpotPriceComputeInstance{Platform: s.Platform},
			Currency:        s.Currency, UnitTitle: s.UnitTitle,
		}
		for _, pt := range s.Points {
			out.Points = append(out.Points, &billingv1alpha1pb.SpotPricePoint{Timestamp: timestamppb.New(pt.Timestamp), Value: pt.Price.String()})
		}
		resp.Items = append(resp.Items, out)
	}
	return resp, nil
}

// optTime is the zero time for an unset timestamp.
func optTime(ts *timestamppb.Timestamp) time.Time {
	if ts == nil {
		return time.Time{}
	}
	return ts.AsTime()
}

func granularity(g billingv1alpha1pb.ListSpotPriceSeriesRequest_Aggregation_Granularity) domain.Granularity {
	switch g {
	case billingv1alpha1pb.ListSpotPriceSeriesRequest_Aggregation_GRANULARITY_MINUTE_15:
		return domain.GranularityMinute15
	case billingv1alpha1pb.ListSpotPriceSeriesRequest_Aggregation_GRANULARITY_HOUR_1:
		return domain.GranularityHour1
	case billingv1alpha1pb.ListSpotPriceSeriesRequest_Aggregation_GRANULARITY_UNSPECIFIED:
		return domain.GranularityUnspecified
	default:
		return domain.GranularityUnspecified
	}
}

func method(m billingv1alpha1pb.ListSpotPriceSeriesRequest_Aggregation_Method) domain.Method {
	switch m {
	case billingv1alpha1pb.ListSpotPriceSeriesRequest_Aggregation_METHOD_FIRST_PRICE:
		return domain.MethodFirstPrice
	case billingv1alpha1pb.ListSpotPriceSeriesRequest_Aggregation_METHOD_LAST_PRICE:
		return domain.MethodLastPrice
	case billingv1alpha1pb.ListSpotPriceSeriesRequest_Aggregation_METHOD_AVG_PRICE:
		return domain.MethodAvgPrice
	case billingv1alpha1pb.ListSpotPriceSeriesRequest_Aggregation_METHOD_UNSPECIFIED:
		return domain.MethodUnspecified
	default:
		return domain.MethodUnspecified
	}
}
