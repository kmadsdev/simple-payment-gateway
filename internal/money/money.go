// Package money holds the amount type of the gateway.
//
// Every amount is an integer number of minor units (centavos for BRL).
// The code never uses floating point numbers for money: a guard test in
// internal/archtest fails the build if a float type shows up outside the
// metrics package.
package money

import (
	"errors"
	"math"
	"strconv"
)

// Amount is a value in minor units. 1050 means R$ 10,50.
type Amount int64

// CurrencyBRL is the only currency this gateway accepts.
const CurrencyBRL = "BRL"

// ErrInvalidAmount is returned by Parse for malformed input.
var ErrInvalidAmount = errors.New("invalid amount")

// ValidCurrency reports whether the currency code is supported.
func ValidCurrency(c string) bool { return c == CurrencyBRL }

// Positive reports whether the amount is greater than zero.
func (a Amount) Positive() bool { return a > 0 }

// String renders the amount in the canonical decimal form, see Format.
func (a Amount) String() string { return Format(a) }

// Format renders minor units as a decimal string with exactly two
// fraction digits and a dot separator: 1050 -> "10.50", -5 -> "-0.05".
// This is the form used inside the Pix payload (EMV field 54).
func Format(a Amount) string {
	neg := a < 0
	// uint64(-a) is correct even for math.MinInt64 (two's complement).
	u := uint64(a)
	if neg {
		u = uint64(-a)
	}
	units := strconv.FormatUint(u/100, 10)
	cents := u % 100
	out := units + "." + string(rune('0'+cents/10)) + string(rune('0'+cents%10))
	if neg {
		return "-" + out
	}
	return out
}

// Parse reads a non-negative decimal string with exactly two fraction
// digits ("10.50") and returns minor units. It rejects signs, spaces,
// exponents, commas, extra digits and values that overflow int64.
func Parse(s string) (Amount, error) {
	dot := len(s) - 3
	if dot < 1 || s[dot] != '.' {
		return 0, ErrInvalidAmount
	}
	if len(s) > 1 && s[0] == '0' && dot > 1 {
		return 0, ErrInvalidAmount // no leading zeros: "01.00"
	}
	var units uint64
	for i := 0; i < dot; i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, ErrInvalidAmount
		}
		units = units*10 + uint64(c-'0')
		if units > math.MaxInt64/100 {
			return 0, ErrInvalidAmount // stops before uint64 can overflow
		}
	}
	c1, c2 := s[dot+1], s[dot+2]
	if c1 < '0' || c1 > '9' || c2 < '0' || c2 > '9' {
		return 0, ErrInvalidAmount
	}
	total := units*100 + uint64(c1-'0')*10 + uint64(c2-'0')
	if total > math.MaxInt64 {
		return 0, ErrInvalidAmount
	}
	return Amount(total), nil
}
