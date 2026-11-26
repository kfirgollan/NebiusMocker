package domain

import "time"

// Config holds the settings the billing domain needs from the mocker.
type Config struct {
	// ProjectID is the seeded project; it always exists, even without IAM.
	ProjectID string
	// Started is when the module started (mock clock). The spot prices of
	// the price list are in effect from SpotEffectiveFrom(Started).
	Started time.Time
}

// SpotHistory is how far back spot price series reach (spot_price_service.proto:
// start_time must be within the last 30 days).
const SpotHistory = 30 * 24 * time.Hour

// SpotEffectiveFrom is the effective_time of the price list's spot prices:
// the whole history window before started, on an hour boundary, so every
// series query in the last 30 days sees the configured prices. The prices
// are constant: the mock's market does not move (docs/services/billing.md).
func SpotEffectiveFrom(started time.Time) time.Time {
	return started.Add(-SpotHistory).UTC().Truncate(time.Hour)
}
