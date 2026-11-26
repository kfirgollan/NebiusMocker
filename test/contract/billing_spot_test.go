package contract

import (
	"slices"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/timestamppb"

	billingv1alpha1pb "github.com/nebius/gosdk/proto/nebius/billing/v1alpha1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
)

func billingSeriesReq(start time.Time, g billingv1alpha1pb.ListSpotPriceSeriesRequest_Aggregation_Granularity, skus ...string) *billingv1alpha1pb.ListSpotPriceSeriesRequest {
	return &billingv1alpha1pb.ListSpotPriceSeriesRequest{
		Filters: &billingv1alpha1pb.ListSpotPriceSeriesRequest_Filters{SkuIds: skus, StartTime: timestamppb.New(start)},
		Aggregation: &billingv1alpha1pb.ListSpotPriceSeriesRequest_Aggregation{
			Granularity: g, Method: billingv1alpha1pb.ListSpotPriceSeriesRequest_Aggregation_METHOD_AVG_PRICE,
		},
	}
}

const (
	billingHour1    = billingv1alpha1pb.ListSpotPriceSeriesRequest_Aggregation_GRANULARITY_HOUR_1
	billingMinute15 = billingv1alpha1pb.ListSpotPriceSeriesRequest_Aggregation_GRANULARITY_MINUTE_15
)

func TestBillingSpotList(t *testing.T) {
	t.Parallel()
	env := startBillingCalc(t)
	before := billingNow().Add(-time.Minute)
	// Without a token: the spot market is public.
	resp, err := env.anonSpot.List(t.Context(), &billingv1alpha1pb.ListSpotPricesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.GetItems()) == 0 || resp.GetNextPageToken() != "" {
		t.Fatalf("%d items, token %q", len(resp.GetItems()), resp.GetNextPageToken())
	}
	if ts := resp.GetSnapshotTime().AsTime(); ts.Before(before) || ts.After(billingNow().Add(time.Minute)) {
		t.Errorf("snapshot_time = %v", ts)
	}
	if !slices.IsSortedFunc(resp.GetItems(), func(a, b *billingv1alpha1pb.SpotPrice) int {
		if a.GetRegion() != b.GetRegion() {
			return compareBillingStrings(a.GetRegion(), b.GetRegion())
		}
		return compareBillingStrings(a.GetSkuId(), b.GetSkuId())
	}) {
		t.Error("items are not sorted by region then sku_id")
	}
	var h100 *billingv1alpha1pb.SpotPrice
	for _, p := range resp.GetItems() {
		if p.GetComputeInstance().GetPlatform() == "gpu-h100-sxm" {
			h100 = p
		}
		if p.GetSkuId() == "" || p.GetProductTitle() == "" || p.GetRegion() == "" || p.GetEffectiveTime() == nil ||
			p.GetPrice().GetCurrency() != "usd" || p.GetPrice().GetUnitTitle() != "GPU hour" {
			t.Errorf("incomplete spot price: %v", p)
		}
	}
	if h100 == nil || h100.GetRegion() != "eu-north1" || h100.GetPrice().GetValue() != "1.25" ||
		h100.GetProductTitle() != "Preemptible NVIDIA® H100 NVLink" {
		t.Fatalf("h100 spot = %v", h100)
	}
	if h100.GetEffectiveTime().AsTime().After(billingNow().Add(-28 * 24 * time.Hour)) {
		t.Errorf("effective_time %v does not reach back over the 30 day history", h100.GetEffectiveTime().AsTime())
	}
}

func compareBillingStrings(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func TestBillingSpotListPaging(t *testing.T) {
	t.Parallel()
	env := startBillingCalc(t)
	all, err := env.spot.List(t.Context(), &billingv1alpha1pb.ListSpotPricesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	token := ""
	for range 50 {
		page, err := env.spot.List(t.Context(), &billingv1alpha1pb.ListSpotPricesRequest{PageSize: 2, PageToken: token})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.GetItems()) > 2 {
			t.Fatalf("page has %d items, size 2", len(page.GetItems()))
		}
		for _, p := range page.GetItems() {
			got = append(got, p.GetRegion()+"/"+p.GetSkuId())
		}
		if token = page.GetNextPageToken(); token == "" {
			break
		}
	}
	var want []string
	for _, p := range all.GetItems() {
		want = append(want, p.GetRegion()+"/"+p.GetSkuId())
	}
	if !slices.Equal(got, want) || len(want) < 3 {
		t.Fatalf("paged %v, want %v", got, want)
	}
	_, err = env.spot.List(t.Context(), &billingv1alpha1pb.ListSpotPricesRequest{PageSize: -1})
	assertCode(t, err, codes.InvalidArgument)
	_, err = env.spot.List(t.Context(), &billingv1alpha1pb.ListSpotPricesRequest{PageToken: "garbage!"})
	billingCalcWantInvalid(t, err, "page_token")
}

func TestBillingSpotListSeries(t *testing.T) {
	t.Parallel()
	env := startBillingCalc(t)
	prices, err := env.anonSpot.List(t.Context(), &billingv1alpha1pb.ListSpotPricesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var h100SKU string
	for _, p := range prices.GetItems() {
		if p.GetComputeInstance().GetPlatform() == "gpu-h100-sxm" {
			h100SKU = p.GetSkuId()
		}
	}

	start := billingNow().Add(-3 * time.Hour)
	// Anonymous too.
	resp, err := env.anonSpot.ListSeries(t.Context(), billingSeriesReq(start, billingHour1, h100SKU))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.GetItems()) != 1 {
		t.Fatalf("%d series, want 1", len(resp.GetItems()))
	}
	s := resp.GetItems()[0]
	if s.GetSkuId() != h100SKU || s.GetRegion() != "eu-north1" || s.GetCurrency() != "usd" || s.GetUnitTitle() != "GPU hour" ||
		s.GetComputeInstance().GetPlatform() != "gpu-h100-sxm" || s.GetProductTitle() == "" {
		t.Fatalf("series = %v", s)
	}
	if n := len(s.GetPoints()); n < 2 || n > 4 {
		t.Fatalf("%d hourly points over 3 hours, want 2 to 4", n)
	}
	for i, p := range s.GetPoints() {
		ts := p.GetTimestamp().AsTime()
		if ts.Minute() != 0 || ts.Second() != 0 || !ts.After(start) || ts.After(billingNow().Add(time.Minute)) || p.GetValue() != "1.25" {
			t.Errorf("point %d = %v %s", i, ts, p.GetValue())
		}
		if i > 0 && !ts.After(s.GetPoints()[i-1].GetTimestamp().AsTime()) {
			t.Errorf("points are not oldest first")
		}
	}

	t.Run("15 minute steps", func(t *testing.T) {
		t.Parallel()
		resp, err := env.spot.ListSeries(t.Context(), billingSeriesReq(billingNow().Add(-2*time.Hour), billingMinute15, h100SKU))
		if err != nil {
			t.Fatal(err)
		}
		pts := resp.GetItems()[0].GetPoints()
		if len(pts) < 7 || len(pts) > 9 {
			t.Fatalf("%d points over 2 hours at 15 minutes, want about 8", len(pts))
		}
		if d := pts[1].GetTimestamp().AsTime().Sub(pts[0].GetTimestamp().AsTime()); d != 15*time.Minute {
			t.Errorf("step = %v, want 15m", d)
		}
	})
	t.Run("explicit end", func(t *testing.T) {
		t.Parallel()
		end := billingNow().Add(-time.Hour).Truncate(time.Hour)
		req := billingSeriesReq(end.Add(-3*time.Hour), billingHour1, h100SKU)
		req.Filters.EndTime = timestamppb.New(end)
		resp, err := env.spot.ListSeries(t.Context(), req)
		if err != nil {
			t.Fatal(err)
		}
		pts := resp.GetItems()[0].GetPoints()
		if len(pts) != 3 || !pts[2].GetTimestamp().AsTime().Equal(end) || !pts[0].GetTimestamp().AsTime().Equal(end.Add(-2*time.Hour)) {
			t.Fatalf("points = %v, want the step ends %v..%v", pts, end.Add(-2*time.Hour), end)
		}
	})
	t.Run("all SKUs and paging", func(t *testing.T) {
		t.Parallel()
		all, err := env.spot.ListSeries(t.Context(), billingSeriesReq(start, billingHour1))
		if err != nil {
			t.Fatal(err)
		}
		if len(all.GetItems()) != len(prices.GetItems()) {
			t.Fatalf("%d series, want %d (one per sku and region)", len(all.GetItems()), len(prices.GetItems()))
		}
		var got []string
		req := billingSeriesReq(start, billingHour1)
		req.PageSize = 3
		token := ""
		for range 50 {
			req.PageToken = token
			page, err := env.spot.ListSeries(t.Context(), req)
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range page.GetItems() {
				got = append(got, s.GetRegion()+"/"+s.GetSkuId())
			}
			if token = page.GetNextPageToken(); token == "" {
				break
			}
		}
		var want []string
		for _, s := range all.GetItems() {
			want = append(want, s.GetRegion()+"/"+s.GetSkuId())
		}
		if !slices.Equal(got, want) {
			t.Fatalf("paged %v, want %v", got, want)
		}
	})
}

func TestBillingSpotListSeriesErrors(t *testing.T) {
	t.Parallel()
	env := startBillingCalc(t)
	start := billingNow().Add(-3 * time.Hour)
	tests := []struct {
		name  string
		req   func() *billingv1alpha1pb.ListSpotPriceSeriesRequest
		field string
	}{
		{"start missing", func() *billingv1alpha1pb.ListSpotPriceSeriesRequest {
			r := billingSeriesReq(start, billingHour1)
			r.Filters.StartTime = nil
			return r
		}, ""},
		{"start older than 30 days", func() *billingv1alpha1pb.ListSpotPriceSeriesRequest {
			return billingSeriesReq(billingNow().Add(-31*24*time.Hour), billingHour1)
		}, "filters.start_time"},
		{"start after end", func() *billingv1alpha1pb.ListSpotPriceSeriesRequest {
			r := billingSeriesReq(start, billingHour1)
			r.Filters.EndTime = timestamppb.New(start.Add(-time.Hour))
			return r
		}, "filters.start_time"},
		{"granularity unspecified", func() *billingv1alpha1pb.ListSpotPriceSeriesRequest {
			return billingSeriesReq(start, billingv1alpha1pb.ListSpotPriceSeriesRequest_Aggregation_GRANULARITY_UNSPECIFIED)
		}, "aggregation.granularity"},
		{"method unspecified", func() *billingv1alpha1pb.ListSpotPriceSeriesRequest {
			r := billingSeriesReq(start, billingHour1)
			r.Aggregation.Method = billingv1alpha1pb.ListSpotPriceSeriesRequest_Aggregation_METHOD_UNSPECIFIED
			return r
		}, "aggregation.method"},
		{"15 minutes over 7 days", func() *billingv1alpha1pb.ListSpotPriceSeriesRequest {
			return billingSeriesReq(billingNow().Add(-8*24*time.Hour), billingMinute15)
		}, "aggregation.granularity"},
		{"unknown sku", func() *billingv1alpha1pb.ListSpotPriceSeriesRequest {
			return billingSeriesReq(start, billingHour1, "sku-e00doesnotexist")
		}, "filters.sku_ids[0]"},
		{"filters missing", func() *billingv1alpha1pb.ListSpotPriceSeriesRequest {
			r := billingSeriesReq(start, billingHour1)
			r.Filters = nil
			return r
		}, ""},
		{"aggregation missing", func() *billingv1alpha1pb.ListSpotPriceSeriesRequest {
			r := billingSeriesReq(start, billingHour1)
			r.Aggregation = nil
			return r
		}, ""},
		{"bad page token", func() *billingv1alpha1pb.ListSpotPriceSeriesRequest {
			r := billingSeriesReq(start, billingHour1)
			r.PageToken = "%%%"
			return r
		}, "page_token"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := env.spot.ListSeries(t.Context(), tc.req())
			assertCode(t, err, codes.InvalidArgument)
			if tc.field != "" {
				billingCalcWantInvalid(t, err, tc.field)
			}
		})
	}
}

// TestBillingSpotPublicUnderEnforcedAuth: the spot market answers without a
// token even when authentication and authorization are enforced.
func TestBillingSpotPublicUnderEnforcedAuth(t *testing.T) {
	t.Parallel()
	env := startBillingCalc(t, testkit.WithAuthz(authz.ModeEnforce), testkit.WithAuthnRequired())
	if _, err := env.anonSpot.List(t.Context(), &billingv1alpha1pb.ListSpotPricesRequest{}); err != nil {
		t.Fatalf("List without a token: %v", err)
	}
	if _, err := env.anonSpot.ListSeries(t.Context(), billingSeriesReq(billingNow().Add(-2*time.Hour), billingHour1)); err != nil {
		t.Fatalf("ListSeries without a token: %v", err)
	}
}
