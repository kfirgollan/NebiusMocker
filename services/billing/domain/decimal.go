package domain

import (
	"fmt"
	"math/big"
	"regexp"
	"strings"
)

// Decimal is an exact non-negative decimal amount (prices, quantities,
// costs). Billing never uses floats: every amount is a terminating decimal,
// so products and sums stay exact.
type Decimal struct{ r big.Rat }

var decimalRE = regexp.MustCompile(`^\d+(\.\d+)?$`)

// ParseDecimal parses a non-negative decimal string such as "2.95" or "3".
func ParseDecimal(s string) (Decimal, error) {
	if !decimalRE.MatchString(s) {
		return Decimal{}, fmt.Errorf("%q is not a non-negative decimal number such as \"2.95\"", s)
	}
	var d Decimal
	if _, ok := d.r.SetString(s); !ok {
		return Decimal{}, fmt.Errorf("%q is not a decimal number", s)
	}
	return d, nil
}

// FractionDigits returns the number of digits after the point in s.
func FractionDigits(s string) int {
	if _, frac, ok := strings.Cut(s, "."); ok {
		return len(frac)
	}
	return 0
}

// DecimalInt returns n as a Decimal.
func DecimalInt(n int64) Decimal {
	var d Decimal
	d.r.SetInt64(n)
	return d
}

// Mul returns d × o.
func (d Decimal) Mul(o Decimal) Decimal {
	var out Decimal
	out.r.Mul(&d.r, &o.r)
	return out
}

// Add returns d + o.
func (d Decimal) Add(o Decimal) Decimal {
	var out Decimal
	out.r.Add(&d.r, &o.r)
	return out
}

// Cmp compares d and o (-1, 0, +1).
func (d Decimal) Cmp(o Decimal) int { return d.r.Cmp(&o.r) }

// IsZero reports whether d is 0.
func (d Decimal) IsZero() bool { return d.r.Sign() == 0 }

// String formats d with full precision and no trailing zeros ("42.123", "3").
func (d Decimal) String() string {
	s := d.r.FloatString(maxScale)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	return s
}

// Rounded formats d rounded up to two decimal places ("42.13"), as the
// *_rounded fields of the billing protos specify.
func (d Decimal) Rounded() string {
	var scaled big.Rat
	scaled.Mul(&d.r, big.NewRat(100, 1))
	q, m := new(big.Int).QuoRem(scaled.Num(), scaled.Denom(), new(big.Int))
	if m.Sign() > 0 {
		q.Add(q, big.NewInt(1))
	}
	var out big.Rat
	out.SetFrac(q, big.NewInt(100))
	return out.FloatString(2)
}

// maxScale bounds String's precision; every amount is a product of a few
// terminating decimals, far below this many digits.
const maxScale = 30

// DecimalRatio returns n/d as an exact Decimal (d > 0). The result may not
// terminate (1/60); String then cuts at maxScale digits.
func DecimalRatio(n, d int64) Decimal {
	var out Decimal
	out.r.SetFrac(big.NewInt(n), big.NewInt(d))
	return out
}

// DivInt returns d ÷ n (n > 0), e.g. an hourly price per minute.
func (d Decimal) DivInt(n int64) Decimal {
	var out Decimal
	out.r.Quo(&d.r, new(big.Rat).SetInt64(n))
	return out
}
