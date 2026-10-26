package transfer

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"
)

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		value string
		wait  time.Duration
		ok    bool
	}{
		{"", 0, false},
		{"  ", 0, false},
		{"0", 0, true},
		{"7", 7 * time.Second, true},
		{" 120 ", 120 * time.Second, true},
		{now.Add(90 * time.Second).Format(http.TimeFormat), 90 * time.Second, true},
		{now.Add(-time.Minute).Format(http.TimeFormat), 0, true},
		{"-5", 0, false},
		{"1.5", 0, false},
		{"soon", 0, false},
		{"99999999999999999999999", maxDuration, true},
		// A long wait is returned in full, never shortened to a local limit.
		{"864000", 10 * 24 * time.Hour, true},
		{now.Add(48 * time.Hour).Format(http.TimeFormat), 48 * time.Hour, true},
	}
	for _, tt := range tests {
		wait, ok := ParseRetryAfter(tt.value, now)
		if wait != tt.wait || ok != tt.ok {
			t.Errorf("ParseRetryAfter(%q) = %v, %v; want %v, %v", tt.value, wait, ok, tt.wait, tt.ok)
		}
	}
}

func TestBackoffGrowsWithJitterAndCaps(t *testing.T) {
	for attempt, base := range map[int]time.Duration{1: time.Second, 2: 2 * time.Second, 3: 4 * time.Second,
		5: 16 * time.Second, 6: 30 * time.Second, 9: 30 * time.Second, 64: 30 * time.Second} {
		low := Backoff(attempt, func() float64 { return 0 })
		high := Backoff(attempt, func() float64 { return 0.999999 })
		if low != base/2 || high > base || high < base*9/10 {
			t.Errorf("attempt %d: backoff range [%v, %v], want [%v, %v]", attempt, low, high, base/2, base)
		}
	}
	if Backoff(0, func() float64 { return 0 }) != 500*time.Millisecond {
		t.Fatal("attempts below one use the first step")
	}
}

func TestDeferredErrorIsRecognisedThroughWrapping(t *testing.T) {
	until := time.Date(2026, 10, 26, 12, 5, 0, 0, time.UTC)
	err := fmt.Errorf("task: %w", &DeferredError{Until: until, Status: 429})
	deferred, ok := AsDeferred(err)
	if !ok || !deferred.Until.Equal(until) || deferred.Status != 429 {
		t.Fatalf("AsDeferred = %v, %v", deferred, ok)
	}
	if _, ok := AsDeferred(errors.New("plain")); ok {
		t.Fatal("plain errors are not deferrals")
	}
	if !IsRateLimitStatus(429) || !IsRateLimitStatus(503) || IsRateLimitStatus(500) || IsRateLimitStatus(403) {
		t.Fatal("only 429 and 503 are rate limits")
	}
}
