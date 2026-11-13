package resilience

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestBackoffCeiling(t *testing.T) {
	b := Backoff{Base: 100 * time.Millisecond, Max: time.Second}
	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{0, 100 * time.Millisecond},
		{1, 200 * time.Millisecond},
		{3, 800 * time.Millisecond},
		{4, time.Second},
		{50, time.Second},
	}
	for _, tt := range tests {
		if got := b.Ceiling(tt.attempt); got != tt.want {
			t.Errorf("Ceiling(%d) = %v, want %v", tt.attempt, got, tt.want)
		}
		for i := 0; i < 100; i++ {
			if d := b.Delay(tt.attempt); d < 0 || d > tt.want {
				t.Fatalf("Delay(%d) = %v out of [0,%v]", tt.attempt, d, tt.want)
			}
		}
	}
}

var errTemp = errors.New("temporary")
var errPerm = errors.New("permanent")

func isTemp(err error) bool { return errors.Is(err, errTemp) }

func TestRetry(t *testing.T) {
	fast := Backoff{Base: time.Millisecond, Max: time.Millisecond}
	tests := []struct {
		name      string
		errs      []error
		attempts  int
		wantCalls int
		wantErr   error
	}{
		{"success first", []error{nil}, 3, 1, nil},
		{"success after retry", []error{errTemp, nil}, 3, 2, nil},
		{"budget exhausted", []error{errTemp, errTemp, errTemp, nil}, 3, 3, errTemp},
		{"permanent stops", []error{errPerm, nil}, 3, 1, errPerm},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			err := Retry(context.Background(), tt.attempts, fast, isTemp, func(context.Context) error {
				calls++
				return tt.errs[calls-1]
			})
			if calls != tt.wantCalls || !errors.Is(err, tt.wantErr) {
				t.Fatalf("calls=%d err=%v, want calls=%d err=%v", calls, err, tt.wantCalls, tt.wantErr)
			}
		})
	}
}

func TestRetryStopsOnContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	slow := Backoff{Base: time.Hour, Max: time.Hour}
	start := time.Now()
	err := Retry(ctx, 5, slow, isTemp, func(context.Context) error { return errTemp })
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
		t.Fatalf("err = %v after %v", err, time.Since(start))
	}
}

func TestBreaker(t *testing.T) {
	now := time.Unix(0, 0)
	var changes []State
	b := &Breaker{Threshold: 2, Cooldown: time.Minute, Now: func() time.Time { return now },
		OnChange: func(s State) { changes = append(changes, s) }}

	b.Failure()
	if b.Allow() != nil || b.State() != Closed {
		t.Fatal("one failure must not open")
	}
	b.Failure()
	if !errors.Is(b.Allow(), ErrOpen) || b.State() != Open {
		t.Fatal("two failures must open")
	}
	now = now.Add(time.Minute)
	if b.Allow() != nil || b.State() != HalfOpen {
		t.Fatal("after cooldown one trial is allowed")
	}
	if !errors.Is(b.Allow(), ErrOpen) {
		t.Fatal("only one trial at a time")
	}
	b.Failure()
	if b.State() != Open {
		t.Fatal("failed trial must reopen")
	}
	now = now.Add(time.Minute)
	_ = b.Allow()
	b.Success()
	if b.State() != Closed || b.Allow() != nil {
		t.Fatal("successful trial must close")
	}
	want := []State{Open, HalfOpen, Open, HalfOpen, Closed}
	if len(changes) != len(want) {
		t.Fatalf("changes = %v, want %v", changes, want)
	}
	for i := range want {
		if changes[i] != want[i] {
			t.Fatalf("changes = %v, want %v", changes, want)
		}
	}
	if Closed.String() != "closed" || HalfOpen.String() != "half_open" {
		t.Fatal("state names")
	}
}
