package webhook

import (
	"errors"
	"testing"
	"time"
)

func TestSignVerify(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	body := []byte(`{"type":"payment.captured"}`)
	h := Sign("whsec_test", now, body)

	tests := []struct {
		name   string
		secret string
		header string
		body   []byte
		now    time.Time
		want   error
	}{
		{"valid", "whsec_test", h, body, now, nil},
		{"valid within window", "whsec_test", h, body, now.Add(4 * time.Minute), nil},
		{"replay after window", "whsec_test", h, body, now.Add(6 * time.Minute), ErrTooOld},
		{"future timestamp", "whsec_test", h, body, now.Add(-6 * time.Minute), ErrTooOld},
		{"tampered body", "whsec_test", h, []byte(`{"type":"payment.refunded"}`), now, ErrBadSignature},
		{"wrong secret", "other", h, body, now, ErrBadSignature},
		{"no v1", "whsec_test", "t=1800000000", body, now, ErrBadHeader},
		{"garbage", "whsec_test", "nonsense", body, now, ErrBadHeader},
		{"bad t", "whsec_test", "t=abc,v1=00", body, now, ErrBadHeader},
		{"rotation: second v1 matches", "whsec_test", h + ",v1=deadbeef", body, now, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := Verify(tt.secret, tt.header, tt.body, tt.now, 5*time.Minute); !errors.Is(err, tt.want) {
				t.Fatalf("Verify = %v, want %v", err, tt.want)
			}
		})
	}
}
