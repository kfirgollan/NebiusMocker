package domain

import (
	"cmp"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

const (
	spotDefaultPageSize = 100
	spotMaxSKUFilter    = 100
	// spotMinuteLimit is the longest interval GranularityMinute15 allows.
	spotMinuteLimit = 7 * 24 * time.Hour
	// spotUnitTitle is the unit of every spot price.
	spotUnitTitle = "GPU hour"
)

// Granularity is the length of a time step of a spot price series. The zero
// value means "unspecified" and is rejected.
type Granularity int

// Granularities.
const (
	GranularityUnspecified Granularity = iota
	GranularityMinute15
	GranularityHour1
)

func (g Granularity) step() time.Duration {
	if g == GranularityMinute15 {
		return 15 * time.Minute
	}
	return time.Hour
}

// Method is how a time step's price is taken from the prices in the step.
// The zero value means "unspecified" and is rejected.
type Method int

// Methods. The mock's spot prices are constant over time, so the first, last
// and mean price of every step are all the configured price.
const (
	MethodUnspecified Method = iota
	MethodFirstPrice
	MethodLastPrice
	MethodAvgPrice
)

// SpotDeps are the dependencies of a SpotService. All are required.
type SpotDeps struct {
	Prices *PriceList
	Clock  clock.Clock
	Config Config
}

// SpotService implements nebius.billing.v1alpha1.SpotPriceService from the
// price list. The prices never change.
type SpotService struct{ d SpotDeps }

// NewSpotService wires the service.
func NewSpotService(d SpotDeps) (*SpotService, error) {
	if d.Prices == nil || d.Clock == nil {
		return nil, errors.New("billing spot prices: prices and clock are required")
	}
	return &SpotService{d: d}, nil
}

// SpotPrice is the current price of one spot market in one region.
type SpotPrice struct {
	SKU           string
	ProductTitle  string
	Region        string
	Platform      string
	Price         Decimal
	Currency      string
	UnitTitle     string
	EffectiveTime time.Time
}

// SpotPoint is the price at the end of one time step.
type SpotPoint struct {
	Timestamp time.Time
	Price     Decimal
}

// SpotSeries is the price series of one spot market in one region.
type SpotSeries struct {
	SKU          string
	ProductTitle string
	Region       string
	Platform     string
	Currency     string
	UnitTitle    string
	Points       []SpotPoint
}

// SeriesQuery selects spot price series.
type SeriesQuery struct {
	// SKUIDs filters the series; empty means all.
	SKUIDs []string
	// Start is required. End is exclusive and defaults to now when zero.
	Start, End  time.Time
	Granularity Granularity
	Method      Method
	PageSize    int64
	PageToken   string
}

// spotEntry is one (market, region) pair.
type spotEntry struct {
	market *ComputePrice
	region string
	price  Decimal
}

// entries lists every (market, region) with a price, by region then SKU.
func (s *SpotService) entries() []spotEntry {
	var out []spotEntry
	for _, m := range s.d.Prices.SpotMarkets() {
		for region, price := range m.Spot.Prices {
			out = append(out, spotEntry{market: m, region: region, price: price})
		}
	}
	slices.SortFunc(out, func(a, b spotEntry) int {
		return cmp.Or(cmp.Compare(a.region, b.region), cmp.Compare(a.market.Spot.SKU, b.market.Spot.SKU))
	})
	return out
}

// List returns one page of the current spot prices and the snapshot time.
func (s *SpotService) List(pageSize int64, pageToken string) (Page[SpotPrice], time.Time, error) {
	size, err := spotPageSize(pageSize)
	if err != nil {
		return Page[SpotPrice]{}, time.Time{}, err
	}
	now := s.d.Clock.Now()
	effective := SpotEffectiveFrom(s.d.Config.Started)
	var all []SpotPrice
	for _, e := range s.entries() {
		all = append(all, SpotPrice{
			SKU: e.market.Spot.SKU, ProductTitle: e.market.Spot.ProductTitle, Region: e.region,
			Platform: e.market.Platform, Price: e.price, Currency: s.d.Prices.Currency,
			UnitTitle: spotUnitTitle, EffectiveTime: effective,
		})
	}
	page, err := spotPage(all, size, pageToken)
	return page, now, err
}

// ListSeries returns one page of spot price series and the snapshot time.
func (s *SpotService) ListSeries(q SeriesQuery) (Page[SpotSeries], time.Time, error) {
	size, err := spotPageSize(q.PageSize)
	if err != nil {
		return Page[SpotSeries]{}, time.Time{}, err
	}
	now := s.d.Clock.Now()
	start, end, err := s.interval(q, now)
	if err != nil {
		return Page[SpotSeries]{}, time.Time{}, err
	}
	filter, err := s.skuFilter(q.SKUIDs)
	if err != nil {
		return Page[SpotSeries]{}, time.Time{}, err
	}
	points := seriesTimes(start, end, q.Granularity.step())
	var all []SpotSeries
	for _, e := range s.entries() {
		if len(filter) > 0 && !filter[e.market.Spot.SKU] {
			continue
		}
		ser := SpotSeries{
			SKU: e.market.Spot.SKU, ProductTitle: e.market.Spot.ProductTitle, Region: e.region,
			Platform: e.market.Platform, Currency: s.d.Prices.Currency, UnitTitle: spotUnitTitle,
		}
		for _, t := range points {
			ser.Points = append(ser.Points, SpotPoint{Timestamp: t, Price: e.price})
		}
		all = append(all, ser)
	}
	page, err := spotPage(all, size, q.PageToken)
	return page, now, err
}

// interval validates and resolves the query's time interval and aggregation.
func (s *SpotService) interval(q SeriesQuery, now time.Time) (start, end time.Time, err error) {
	var vs []errs.Violation
	bad := func(field, msg string) { vs = append(vs, errs.Violation{Field: field, Message: msg}) }
	start, end = q.Start, q.End
	if end.IsZero() {
		end = now
	}
	switch {
	case start.IsZero():
		bad("filters.start_time", "required")
	case start.Before(now.Add(-SpotHistory)):
		bad("filters.start_time", "must be within the last 30 days")
	case !start.Before(end):
		bad("filters.start_time", "must be before filters.end_time")
	}
	if end.After(now) {
		bad("filters.end_time", "must not be in the future")
	}
	switch q.Granularity {
	case GranularityUnspecified:
		bad("aggregation.granularity", "required")
	case GranularityMinute15:
		if end.Sub(start) > spotMinuteLimit {
			bad("aggregation.granularity", "GRANULARITY_MINUTE_15 is allowed for an interval of up to 7 days")
		}
	case GranularityHour1:
	}
	if q.Method == MethodUnspecified {
		bad("aggregation.method", "required")
	}
	if len(vs) > 0 {
		return time.Time{}, time.Time{}, errs.BadRequest(vs...)
	}
	return start, end, nil
}

// skuFilter resolves filters.sku_ids to a set; an empty set means all SKUs.
func (s *SpotService) skuFilter(ids []string) (map[string]bool, error) {
	if len(ids) == 0 {
		return map[string]bool{}, nil
	}
	if len(ids) > spotMaxSKUFilter {
		return nil, errs.BadRequest(errs.Violation{Field: "filters.sku_ids", Message: fmt.Sprintf("at most %d SKUs", spotMaxSKUFilter)})
	}
	known := map[string]bool{}
	for _, m := range s.d.Prices.SpotMarkets() {
		known[m.Spot.SKU] = true
	}
	set := map[string]bool{}
	var vs []errs.Violation
	for i, id := range ids {
		if !known[id] {
			vs = append(vs, errs.Violation{Field: fmt.Sprintf("filters.sku_ids[%d]", i), Message: fmt.Sprintf("%q is not a spot market SKU (see SpotPriceService.List)", id)})
		}
		set[id] = true
	}
	if len(vs) > 0 {
		return nil, errs.BadRequest(vs...)
	}
	return set, nil
}

// seriesTimes returns the ends of the time steps in (start, end]: the step
// boundaries aligned to step, from the first after start through end.
// Interval 12:00-15:00 with one hour steps gives 13:00, 14:00 and 15:00.
func seriesTimes(start, end time.Time, step time.Duration) []time.Time {
	var out []time.Time
	for t := start.UTC().Truncate(step).Add(step); !t.After(end); t = t.Add(step) {
		out = append(out, t)
	}
	return out
}

func spotPageSize(n int64) (int, error) {
	switch {
	case n < 0:
		return 0, errs.BadRequest(errs.Violation{Field: "page_size", Message: "must not be negative"})
	case n == 0:
		return spotDefaultPageSize, nil
	}
	return int(n), nil
}

// spotPage returns the page of items at the offset in pageToken.
func spotPage[T any](items []T, size int, pageToken string) (Page[T], error) {
	offset := 0
	if pageToken != "" {
		b, err := base64.RawURLEncoding.DecodeString(pageToken)
		n, perr := strconv.Atoi(string(b))
		if err != nil || perr != nil || n < 0 {
			return Page[T]{}, errs.BadRequest(errs.Violation{Field: "page_token", Message: "malformed page token"})
		}
		offset = n
	}
	if offset >= len(items) {
		return Page[T]{Items: []T{}}, nil
	}
	end := min(offset+size, len(items))
	page := Page[T]{Items: items[offset:end]}
	if end < len(items) {
		page.NextPageToken = base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(end)))
	}
	return page, nil
}
