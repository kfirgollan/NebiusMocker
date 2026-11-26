package domain_test

import (
	"testing"

	"github.com/kfirgollan/NebiusMocker/services/billing/domain"
)

func TestParseDecimalRejectsNonDecimals(t *testing.T) {
	for _, s := range []string{"", "-1", "1e3", "1.", ".5", "abc", "1,5", " 1"} {
		if _, err := domain.ParseDecimal(s); err == nil {
			t.Errorf("ParseDecimal(%q) succeeded, want an error", s)
		}
	}
}

func TestDecimalArithmetic(t *testing.T) {
	tests := []struct {
		name string
		got  domain.Decimal
		want string
	}{
		{"mul exact", mustDec(t, "2.95").Mul(domain.DecimalInt(8)), "23.6"},
		{"mul fraction", mustDec(t, "0.0027").Mul(domain.DecimalInt(16)), "0.0432"},
		{"add", mustDec(t, "0.04").Add(mustDec(t, "0.0432")), "0.0832"},
		{"integer has no point", domain.DecimalInt(3), "3"},
		{"ratio is exact", domain.DecimalRatio(1, 4), "0.25"},
		{"ratio of GiB", domain.DecimalRatio(512, 1<<10), "0.5"},
		{"div int by 60", mustDec(t, "23.6").DivInt(60).Mul(domain.DecimalInt(60)), "23.6"},
		{"trailing zeros trimmed", mustDec(t, "1.2300"), "1.23"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.got.String(); got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
}

func TestDecimalRoundedUp(t *testing.T) {
	tests := []struct{ in, want string }{
		{"0", "0.00"}, {"23.6", "23.60"}, {"0.0832", "0.09"}, {"0.01", "0.01"}, {"0.011", "0.02"}, {"16992", "16992.00"},
	}
	for _, tc := range tests {
		if got := mustDec(t, tc.in).Rounded(); got != tc.want {
			t.Errorf("Rounded(%s) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

func TestDecimalCompare(t *testing.T) {
	if mustDec(t, "1.5").Cmp(mustDec(t, "1.50")) != 0 {
		t.Error("1.5 != 1.50")
	}
	if mustDec(t, "1.5").Cmp(mustDec(t, "2")) >= 0 {
		t.Error("1.5 >= 2")
	}
	if !domain.DecimalInt(0).IsZero() || mustDec(t, "0.1").IsZero() {
		t.Error("IsZero is wrong")
	}
}
