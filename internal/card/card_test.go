package card

import (
	"errors"
	"testing"
	"time"
)

func TestLuhn(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"4111111111111111", true},
		{"5555555555554444", true},
		{"378282246310005", true},
		{"4111111111111112", false},
		{"79927398713", true},
		{"79927398710", false},
		{"", false},
		{"41111111111111a1", false},
		{"0", true},
	}
	for _, tt := range tests {
		if got := Luhn(tt.in); got != tt.want {
			t.Errorf("Luhn(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestNormalize(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"4111 1111 1111 1111", "4111111111111111", false},
		{"4111-1111-1111-1111", "4111111111111111", false},
		{"41111111111", "", true},          // 11 digits
		{"41111111111111111111", "", true}, // 20 digits
		{"4111x11111111111", "", true},
		{"", "", true},
	}
	for _, tt := range tests {
		got, err := Normalize(tt.in)
		if tt.wantErr != (err != nil) || got != tt.want {
			t.Errorf("Normalize(%q) = %q, %v", tt.in, got, err)
		}
	}
}

func TestGuessBrand(t *testing.T) {
	tests := map[string]string{
		"4111111111111111": BrandVisa,
		"5555555555554444": BrandMastercard,
		"2223003122003222": BrandMastercard,
		"378282246310005":  BrandAmex,
		"6362970000457013": BrandElo,
		"4389350000000000": BrandElo,
		"6011111111111117": BrandUnknown,
		"9":                BrandUnknown,
	}
	for in, want := range tests {
		if got := GuessBrand(in); got != want {
			t.Errorf("GuessBrand(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidCVV(t *testing.T) {
	tests := []struct {
		cvv, brand string
		want       bool
	}{
		{"123", BrandVisa, true},
		{"1234", BrandVisa, false},
		{"1234", BrandAmex, true},
		{"12a", BrandVisa, false},
		{"", BrandVisa, false},
	}
	for _, tt := range tests {
		if got := ValidCVV(tt.cvv, tt.brand); got != tt.want {
			t.Errorf("ValidCVV(%q,%q) = %v", tt.cvv, tt.brand, got)
		}
	}
}

func TestValidateExpiry(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		m, y int
		want error
	}{
		{11, 2026, nil},
		{12, 2030, nil},
		{10, 2026, ErrExpired},
		{0, 2027, ErrInvalidExpiry},
		{13, 2027, ErrInvalidExpiry},
		{1, 1999, ErrInvalidExpiry},
	}
	for _, tt := range tests {
		if err := ValidateExpiry(tt.m, tt.y, now); !errors.Is(err, tt.want) {
			t.Errorf("ValidateExpiry(%d,%d) = %v, want %v", tt.m, tt.y, err, tt.want)
		}
	}
}

func TestLast4(t *testing.T) {
	if Last4("4111111111111111") != "1111" || Last4("12") != "" {
		t.Fatal("Last4")
	}
}

// luhnReference is a second, independent implementation used to
// cross-check Luhn in the fuzz test.
func luhnReference(s string) bool {
	if s == "" {
		return false
	}
	total := 0
	for i := 0; i < len(s); i++ {
		c := s[len(s)-1-i]
		if c < '0' || c > '9' {
			return false
		}
		d := int(c - '0')
		if i%2 == 1 {
			d = []int{0, 2, 4, 6, 8, 1, 3, 5, 7, 9}[d]
		}
		total += d
	}
	return total%10 == 0
}

func FuzzLuhn(f *testing.F) {
	for _, s := range []string{"4111111111111111", "79927398713", "", "abc", "0000"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		got := Luhn(s)
		if got != luhnReference(s) {
			t.Fatalf("Luhn(%q) = %v, reference disagrees", s, got)
		}
		if got && len(s) > 0 {
			// Changing the check digit must break the checksum.
			last := s[len(s)-1]
			bumped := s[:len(s)-1] + string(rune('0'+(last-'0'+1)%10))
			if Luhn(bumped) {
				t.Fatalf("Luhn accepted %q and %q", s, bumped)
			}
		}
	})
}

func FuzzNormalize(f *testing.F) {
	for _, s := range []string{"4111 1111 1111 1111", "4111-1111", "x"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		pan, err := Normalize(s)
		if err != nil {
			return
		}
		if len(pan) < 12 || len(pan) > 19 {
			t.Fatalf("Normalize(%q) = %q has bad length", s, pan)
		}
		for i := 0; i < len(pan); i++ {
			if pan[i] < '0' || pan[i] > '9' {
				t.Fatalf("Normalize(%q) = %q has non-digit", s, pan)
			}
		}
	})
}
