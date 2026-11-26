package domain_test

import (
	"slices"
	"testing"
	"time"

	"github.com/kfirgollan/NebiusMocker/services/billing/domain"
)

func TestSpotListSortedByRegionThenSKU(t *testing.T) {
	s, clk := newCalcSpot(t)
	page, snap, err := s.List(0, "")
	if err != nil {
		t.Fatal(err)
	}
	if page.NextPageToken != "" || len(page.Items) == 0 {
		t.Fatalf("page = %d items, token %q; want one full page", len(page.Items), page.NextPageToken)
	}
	if !snap.Equal(clk.Now()) {
		t.Errorf("snapshot = %v, want the clock's now %v", snap, clk.Now())
	}
	if !slices.IsSortedFunc(page.Items, func(a, b domain.SpotPrice) int {
		if a.Region != b.Region {
			return compareStrings(a.Region, b.Region)
		}
		return compareStrings(a.SKU, b.SKU)
	}) {
		t.Error("items are not sorted by region then sku_id")
	}
	var found bool
	for i := range page.Items {
		p := &page.Items[i]
		if p.Platform == "gpu-h100-sxm" {
			found = true
			if p.Region != "eu-north1" || p.Price.String() != "1.25" || p.Currency != "usd" || p.UnitTitle != "GPU hour" {
				t.Errorf("h100 spot = %+v", p)
			}
			if want := domain.SpotEffectiveFrom(calcStarted); !p.EffectiveTime.Equal(want) {
				t.Errorf("effective = %v, want %v", p.EffectiveTime, want)
			}
		}
	}
	if !found {
		t.Error("no H100 spot price")
	}
}

func compareStrings(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func TestSpotListPaging(t *testing.T) {
	s, _ := newCalcSpot(t)
	all, _, err := s.List(0, "")
	if err != nil {
		t.Fatal(err)
	}
	var got []domain.SpotPrice
	token := ""
	for pages := 0; ; pages++ {
		if pages > len(all.Items) {
			t.Fatal("paging does not terminate")
		}
		p, _, err := s.List(3, token)
		if err != nil {
			t.Fatal(err)
		}
		if len(p.Items) > 3 {
			t.Fatalf("page of %d items, size 3", len(p.Items))
		}
		got = append(got, p.Items...)
		if token = p.NextPageToken; token == "" {
			break
		}
	}
	if len(got) != len(all.Items) {
		t.Fatalf("paged %d items, one page has %d", len(got), len(all.Items))
	}
	for i := range got {
		if got[i].SKU != all.Items[i].SKU || got[i].Region != all.Items[i].Region {
			t.Fatalf("item %d differs between paged and full listing", i)
		}
	}
}

func TestSpotListValidation(t *testing.T) {
	s, _ := newCalcSpot(t)
	_, _, err := s.List(-1, "")
	calcWantBad(t, err, "page_size", "negative")
	_, _, err = s.List(10, "not base64 !!")
	calcWantBad(t, err, "page_token", "malformed")
	p, _, err := s.List(10, "OTk5") // offset 999, past the end
	if err != nil || len(p.Items) != 0 || p.NextPageToken != "" {
		t.Fatalf("page past the end = %+v, %v; want empty", p, err)
	}
}

func h100SKU(t *testing.T, s *domain.SpotService) string {
	t.Helper()
	p, _, err := s.List(0, "")
	if err != nil {
		t.Fatal(err)
	}
	for i := range p.Items {
		if p.Items[i].Platform == "gpu-h100-sxm" {
			return p.Items[i].SKU
		}
	}
	t.Fatal("no h100 spot SKU")
	return ""
}

func TestSpotSeriesPointsAreStepEnds(t *testing.T) {
	s, clk := newCalcSpot(t)
	clk.Advance(24 * time.Hour)
	now := clk.Now() // 2026-11-02 12:30 UTC
	sku := h100SKU(t, s)
	day := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		start, end time.Time
		gran       domain.Granularity
		want       []time.Time
	}{
		{
			"proto example: 06:00 to 09:00 hourly", day.Add(6 * time.Hour), day.Add(9 * time.Hour), domain.GranularityHour1,
			[]time.Time{day.Add(7 * time.Hour), day.Add(8 * time.Hour), day.Add(9 * time.Hour)},
		},
		{
			"unaligned start rounds up to the next boundary", day.Add(5*time.Hour + 30*time.Minute), day.Add(7 * time.Hour), domain.GranularityHour1,
			[]time.Time{day.Add(6 * time.Hour), day.Add(7 * time.Hour)},
		},
		{
			"15 minute steps", day.Add(10 * time.Hour), day.Add(10*time.Hour + 45*time.Minute), domain.GranularityMinute15,
			[]time.Time{day.Add(10*time.Hour + 15*time.Minute), day.Add(10*time.Hour + 30*time.Minute), day.Add(10*time.Hour + 45*time.Minute)},
		},
		{"interval shorter than a step has no point", day.Add(10*time.Hour + 5*time.Minute), day.Add(10*time.Hour + 10*time.Minute), domain.GranularityHour1, nil},
		{
			"end defaults to now", now.Add(-2 * time.Hour),
			time.Time{},
			domain.GranularityHour1,
			[]time.Time{day.Add(11 * time.Hour), day.Add(12 * time.Hour)},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			page, _, err := s.ListSeries(domain.SeriesQuery{
				SKUIDs: []string{sku}, Start: tc.start, End: tc.end, Granularity: tc.gran, Method: domain.MethodLastPrice,
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(page.Items) != 1 {
				t.Fatalf("series = %d, want 1 (H100 spot is in one region)", len(page.Items))
			}
			ser := page.Items[0]
			if ser.Region != "eu-north1" || ser.Currency != "usd" || ser.UnitTitle != "GPU hour" || ser.Platform != "gpu-h100-sxm" {
				t.Errorf("series = %+v", ser)
			}
			var got []time.Time
			for _, p := range ser.Points {
				got = append(got, p.Timestamp)
				if p.Price.String() != "1.25" {
					t.Errorf("point %v price %s, want the constant 1.25", p.Timestamp, p.Price)
				}
			}
			if !slices.EqualFunc(got, tc.want, time.Time.Equal) {
				t.Fatalf("points = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSpotSeriesMethodsAllGiveTheConstantPrice(t *testing.T) {
	s, clk := newCalcSpot(t)
	for _, m := range []domain.Method{domain.MethodFirstPrice, domain.MethodLastPrice, domain.MethodAvgPrice} {
		page, _, err := s.ListSeries(domain.SeriesQuery{
			Start: clk.Now().Add(-3 * time.Hour), Granularity: domain.GranularityHour1, Method: m,
		})
		if err != nil {
			t.Fatalf("method %d: %v", m, err)
		}
		for _, ser := range page.Items {
			for _, p := range ser.Points {
				if p.Price.IsZero() {
					t.Fatalf("method %d: zero price in %s/%s", m, ser.Region, ser.SKU)
				}
			}
		}
	}
}

func TestSpotSeriesAllSKUsPagedAndSorted(t *testing.T) {
	s, clk := newCalcSpot(t)
	q := domain.SeriesQuery{Start: clk.Now().Add(-2 * time.Hour), Granularity: domain.GranularityHour1, Method: domain.MethodAvgPrice}
	all, _, err := s.ListSeries(q)
	if err != nil {
		t.Fatal(err)
	}
	prices, _, _ := s.List(0, "")
	if len(all.Items) != len(prices.Items) {
		t.Fatalf("series = %d, prices = %d; want one series per (sku, region)", len(all.Items), len(prices.Items))
	}
	for i := range all.Items {
		if all.Items[i].SKU != prices.Items[i].SKU || all.Items[i].Region != prices.Items[i].Region {
			t.Fatalf("series %d is %s/%s, List has %s/%s", i, all.Items[i].Region, all.Items[i].SKU, prices.Items[i].Region, prices.Items[i].SKU)
		}
	}
	q.PageSize = 4
	var n int
	for {
		p, _, err := s.ListSeries(q)
		if err != nil {
			t.Fatal(err)
		}
		n += len(p.Items)
		if q.PageToken = p.NextPageToken; q.PageToken == "" {
			break
		}
	}
	if n != len(all.Items) {
		t.Fatalf("paged %d series, want %d", n, len(all.Items))
	}
}

func TestSpotSeriesValidation(t *testing.T) {
	s, clk := newCalcSpot(t)
	now := clk.Now()
	sku := h100SKU(t, s)
	base := func() domain.SeriesQuery {
		return domain.SeriesQuery{Start: now.Add(-3 * time.Hour), Granularity: domain.GranularityHour1, Method: domain.MethodAvgPrice}
	}
	tests := []struct {
		name         string
		mut          func(*domain.SeriesQuery)
		field, wants string
	}{
		{"start required", func(q *domain.SeriesQuery) { q.Start = time.Time{} }, "filters.start_time", "required"},
		{"start older than 30 days", func(q *domain.SeriesQuery) { q.Start = now.Add(-domain.SpotHistory - time.Minute) }, "filters.start_time", "30 days"},
		{"start not before end", func(q *domain.SeriesQuery) { q.End = q.Start }, "filters.start_time", "before"},
		{"end in the future", func(q *domain.SeriesQuery) { q.End = now.Add(time.Hour) }, "filters.end_time", "future"},
		{"granularity required", func(q *domain.SeriesQuery) { q.Granularity = domain.GranularityUnspecified }, "aggregation.granularity", "required"},
		{"method required", func(q *domain.SeriesQuery) { q.Method = domain.MethodUnspecified }, "aggregation.method", "required"},
		{"15 minutes over 7 days", func(q *domain.SeriesQuery) {
			q.Start = now.Add(-8 * 24 * time.Hour)
			q.Granularity = domain.GranularityMinute15
		}, "aggregation.granularity", "7 days"},
		{"unknown sku", func(q *domain.SeriesQuery) { q.SKUIDs = []string{sku, "sku-e00nope"} }, "filters.sku_ids[1]", "sku-e00nope"},
		{"too many skus", func(q *domain.SeriesQuery) {
			for range 101 {
				q.SKUIDs = append(q.SKUIDs, sku)
			}
		}, "filters.sku_ids", "100"},
		{"negative page size", func(q *domain.SeriesQuery) { q.PageSize = -1 }, "page_size", "negative"},
		{"bad token", func(q *domain.SeriesQuery) { q.PageToken = "!!" }, "page_token", "malformed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			q := base()
			tc.mut(&q)
			_, _, err := s.ListSeries(q)
			calcWantBad(t, err, tc.field, tc.wants)
		})
	}
	// 15 minutes is fine for exactly 7 days.
	q := base()
	q.Start, q.Granularity = now.Add(-7*24*time.Hour), domain.GranularityMinute15
	if _, _, err := s.ListSeries(q); err != nil {
		t.Fatalf("7 day interval at 15 minutes: %v", err)
	}
}

func TestSpotSeriesStartAtTheHistoryEdge(t *testing.T) {
	s, clk := newCalcSpot(t)
	_, _, err := s.ListSeries(domain.SeriesQuery{
		Start: clk.Now().Add(-domain.SpotHistory), Granularity: domain.GranularityHour1, Method: domain.MethodFirstPrice,
	})
	if err != nil {
		t.Fatalf("start exactly 30 days ago: %v", err)
	}
}
