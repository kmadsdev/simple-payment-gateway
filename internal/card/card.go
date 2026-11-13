// Package card validates test card numbers and derives the data the
// gateway is allowed to keep: a brand guess and the last four digits.
//
// The full number (PAN) and the security code (CVV) exist only in memory
// during the tokenization request. They are never stored and never logged.
package card

import (
	"errors"
	"strings"
	"time"
)

// Brand names returned by GuessBrand. A guess from the number prefix only;
// no card network is integrated.
const (
	BrandVisa       = "visa"
	BrandMastercard = "mastercard"
	BrandAmex       = "amex"
	BrandElo        = "elo"
	BrandUnknown    = "unknown"
)

// Validation errors. The messages never include the input value.
var (
	ErrInvalidNumber = errors.New("card number is not valid")
	ErrInvalidCVV    = errors.New("security code is not valid")
	ErrInvalidExpiry = errors.New("expiry date is not valid")
	ErrExpired       = errors.New("card is expired")
)

// Normalize removes spaces and dashes and checks that only digits remain
// and that the length is between 12 and 19.
func Normalize(number string) (string, error) {
	var b strings.Builder
	b.Grow(len(number))
	for _, r := range number {
		switch {
		case r == ' ' || r == '-':
			continue
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			return "", ErrInvalidNumber
		}
	}
	pan := b.String()
	if len(pan) < 12 || len(pan) > 19 {
		return "", ErrInvalidNumber
	}
	return pan, nil
}

// Luhn reports whether a string of digits passes the Luhn (mod 10) check.
// It returns false for an empty string or any non-digit character.
func Luhn(digits string) bool {
	if digits == "" {
		return false
	}
	sum := 0
	double := false
	for i := len(digits) - 1; i >= 0; i-- {
		c := digits[i]
		if c < '0' || c > '9' {
			return false
		}
		d := int(c - '0')
		if double {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		double = !double
	}
	return sum%10 == 0
}

// GuessBrand guesses the brand from the number prefix (IIN ranges).
func GuessBrand(pan string) string {
	p := func(n int) int {
		if len(pan) < n {
			return -1
		}
		v := 0
		for i := 0; i < n; i++ {
			v = v*10 + int(pan[i]-'0')
		}
		return v
	}
	// Elo first: some Elo ranges start with 4 or 5.
	for _, pre := range []string{"401178", "401179", "438935", "457631", "457632", "431274", "451416", "457393", "504175", "506699", "636297", "636368", "650031"} {
		if strings.HasPrefix(pan, pre) {
			return BrandElo
		}
	}
	switch {
	case p(1) == 4:
		return BrandVisa
	case p(2) >= 51 && p(2) <= 55, p(4) >= 2221 && p(4) <= 2720:
		return BrandMastercard
	case p(2) == 34 || p(2) == 37:
		return BrandAmex
	default:
		return BrandUnknown
	}
}

// Last4 returns the last four digits of a normalized number.
func Last4(pan string) string {
	if len(pan) < 4 {
		return ""
	}
	return pan[len(pan)-4:]
}

// ValidCVV accepts 3 digits, or 4 digits for Amex.
func ValidCVV(cvv, brand string) bool {
	want := 3
	if brand == BrandAmex {
		want = 4
	}
	if len(cvv) != want {
		return false
	}
	for i := 0; i < len(cvv); i++ {
		if cvv[i] < '0' || cvv[i] > '9' {
			return false
		}
	}
	return true
}

// ValidateExpiry checks month 1..12 and that the card is not expired at now.
// A card is valid until the last day of its expiry month.
func ValidateExpiry(month, year int, now time.Time) error {
	if month < 1 || month > 12 || year < 2000 || year > 2100 {
		return ErrInvalidExpiry
	}
	firstOfNext := time.Date(year, time.Month(month)+1, 1, 0, 0, 0, 0, time.UTC)
	if !now.Before(firstOfNext) {
		return ErrExpired
	}
	return nil
}
