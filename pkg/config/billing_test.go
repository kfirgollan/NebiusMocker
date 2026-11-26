package config

import "testing"

func TestFromLookupBillingPricesFile(t *testing.T) {
	t.Parallel()
	if got := Default().Billing.PricesFile; got != "" {
		t.Fatalf("default prices file = %q, want built-in (empty)", got)
	}
	c, err := FromLookup(func(k string) (string, bool) {
		if k == "NEBIUS_MOCKER_BILLING_PRICES_FILE" {
			return "/tmp/prices.yaml", true
		}
		return "", false
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.Billing.PricesFile != "/tmp/prices.yaml" {
		t.Fatalf("PricesFile = %q", c.Billing.PricesFile)
	}
}
