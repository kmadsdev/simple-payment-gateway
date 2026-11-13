package pix

import (
	"errors"
	"strings"
	"testing"
)

func TestCRC16KnownValue(t *testing.T) {
	// Standard check value for CRC-16/CCITT-FALSE.
	if got := CRC16("123456789"); got != 0x29B1 {
		t.Fatalf("CRC16 = %04X, want 29B1", got)
	}
}

func TestBuildParseRoundTrip(t *testing.T) {
	in := Payload{TxID: "pay123ABC", Amount: 15990, MerchantName: "Loja Exemplo", City: "Sao Paulo"}
	s, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s, SimulatedGUI) {
		t.Fatal("payload must carry the simulated marker")
	}
	got, err := Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	if got != in {
		t.Fatalf("got %+v, want %+v", got, in)
	}
}

func TestParseRejectsTampering(t *testing.T) {
	s, err := Build(Payload{TxID: "abc", Amount: 100, MerchantName: "X", City: "Y"})
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(s, "1.00", "9.00", 1)
	if _, err := Parse(tampered); !errors.Is(err, ErrInvalidPayload) {
		t.Fatalf("tampered payload accepted: %v", err)
	}
	if _, err := Parse("short"); !errors.Is(err, ErrInvalidPayload) {
		t.Fatal("short payload accepted")
	}
}

func TestBuildValidates(t *testing.T) {
	tests := []Payload{
		{TxID: "", Amount: 100},
		{TxID: "has-dash", Amount: 100},
		{TxID: strings.Repeat("a", 26), Amount: 100},
		{TxID: "ok", Amount: 0},
	}
	for _, p := range tests {
		if _, err := Build(p); err == nil {
			t.Errorf("Build(%+v) accepted", p)
		}
	}
}

func FuzzParse(f *testing.F) {
	s, _ := Build(Payload{TxID: "abc", Amount: 100, MerchantName: "X", City: "Y"})
	f.Add(s)
	f.Add("0002016304ABCD")
	f.Fuzz(func(t *testing.T, s string) {
		_, _ = Parse(s) // must not panic
	})
}
