package config

// BillingConfig configures the billing module (NEBIUS_MOCKER_BILLING_*).
type BillingConfig struct {
	// PricesFile, when set, replaces the built-in price list (YAML or JSON;
	// see docs/services/billing.md): the unit prices the calculator uses and
	// the spot markets of preemptible platforms that pricing policies and
	// SpotPriceService read.
	PricesFile string
}

func billingParsers(b *BillingConfig) []struct {
	name  string
	apply func(string) error
} {
	return []struct {
		name  string
		apply func(string) error
	}{
		{"BILLING_PRICES_FILE", func(v string) error { b.PricesFile = v; return nil }},
	}
}
