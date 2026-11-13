package money

import (
	"errors"
	"math"
	"testing"
)

func TestFormat(t *testing.T) {
	tests := []struct {
		in   Amount
		want string
	}{
		{0, "0.00"},
		{5, "0.05"},
		{10, "0.10"},
		{1050, "10.50"},
		{123456789, "1234567.89"},
		{-5, "-0.05"},
		{math.MaxInt64, "92233720368547758.07"},
		{math.MinInt64, "-92233720368547758.08"},
	}
	for _, tt := range tests {
		if got := Format(tt.in); got != tt.want {
			t.Errorf("Format(%d) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestParse(t *testing.T) {
	tests := []struct {
		in      string
		want    Amount
		wantErr bool
	}{
		{"0.00", 0, false},
		{"0.05", 5, false},
		{"10.50", 1050, false},
		{"92233720368547758.07", math.MaxInt64, false},
		{"92233720368547758.08", 0, true}, // overflow
		{"999999999999999999.99", 0, true},
		{"10.5", 0, true},
		{"10", 0, true},
		{".50", 0, true},
		{"01.00", 0, true},
		{"-1.00", 0, true},
		{"+1.00", 0, true},
		{"1,00", 0, true},
		{"1.0a", 0, true},
		{"1e2.00", 0, true},
		{" 1.00", 0, true},
		{"", 0, true},
	}
	for _, tt := range tests {
		got, err := Parse(tt.in)
		if tt.wantErr {
			if !errors.Is(err, ErrInvalidAmount) {
				t.Errorf("Parse(%q) err = %v, want ErrInvalidAmount", tt.in, err)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Errorf("Parse(%q) = %d, %v; want %d", tt.in, got, err, tt.want)
		}
	}
}

func TestValidCurrency(t *testing.T) {
	if !ValidCurrency("BRL") || ValidCurrency("USD") || ValidCurrency("brl") {
		t.Fatal("only BRL must be valid")
	}
}

// FuzzParse checks that Parse never panics and that every accepted
// input survives a Format round trip unchanged.
func FuzzParse(f *testing.F) {
	for _, s := range []string{"0.00", "10.50", "92233720368547758.07", "1.0", "abc", "-1.00", "00.00"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		a, err := Parse(s)
		if err != nil {
			return
		}
		if a < 0 {
			t.Fatalf("Parse(%q) returned negative %d", s, a)
		}
		if got := Format(a); got != s {
			t.Fatalf("round trip: Parse(%q)=%d, Format=%q", s, a, got)
		}
	})
}

// FuzzFormat checks Parse(Format(a)) == a for every non-negative amount.
func FuzzFormat(f *testing.F) {
	for _, v := range []int64{0, 1, 99, 100, 1050, math.MaxInt64} {
		f.Add(v)
	}
	f.Fuzz(func(t *testing.T, v int64) {
		if v < 0 {
			return
		}
		got, err := Parse(Format(Amount(v)))
		if err != nil || got != Amount(v) {
			t.Fatalf("Parse(Format(%d)) = %d, %v", v, got, err)
		}
	})
}
