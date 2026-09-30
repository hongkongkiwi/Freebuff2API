package main

import (
	"strings"
	"testing"
	"time"
)

func TestTokenPoolCircuitBreaker(t *testing.T) {
	pool := &tokenPool{name: "token-test", logger: discardLogger()}

	for i := 0; i < breakerThreshold-1; i++ {
		pool.reportFailure("upstream status 500")
	}
	snapshot := pool.snapshot()
	if snapshot.CooldownUntil != (time.Time{}) {
		t.Fatalf("cooldown set before threshold: %v", snapshot.CooldownUntil)
	}
	if snapshot.ConsecFailures != breakerThreshold-1 {
		t.Fatalf("consecutive failures = %d", snapshot.ConsecFailures)
	}

	pool.reportFailure("upstream status 500")
	snapshot = pool.snapshot()
	if snapshot.CooldownUntil.IsZero() {
		t.Fatalf("breaker did not trip after %d failures", breakerThreshold)
	}
	if snapshot.Health != healthOK {
		t.Fatalf("health = %q; only markCooldown paths set it", snapshot.Health)
	}
	if time.Until(snapshot.CooldownUntil) > breakerCooldown {
		t.Fatalf("cooldown %v exceeds breaker cooldown", snapshot.CooldownUntil)
	}

	// Success after the cooldown window resets failures.
	pool.cooldownUntil = time.Now().Add(-time.Minute)
	pool.reportSuccess()
	snapshot = pool.snapshot()
	if !snapshot.CooldownUntil.IsZero() && time.Now().Before(snapshot.CooldownUntil) {
		t.Fatalf("cooldown still active: %v", snapshot.CooldownUntil)
	}
	if snapshot.ConsecFailures != 0 {
		t.Fatalf("failures not reset on success: %d", snapshot.ConsecFailures)
	}
	if snapshot.Health != healthOK {
		t.Fatalf("health = %q after success", snapshot.Health)
	}
}

func TestTokenPoolSetHealth(t *testing.T) {
	pool := &tokenPool{name: "token-test", logger: discardLogger()}
	pool.setHealth(healthBanned)
	if got := pool.snapshot().Health; got != healthBanned {
		t.Fatalf("health = %q, want %q", got, healthBanned)
	}
	// A later success clears the classification.
	pool.reportSuccess()
	if got := pool.snapshot().Health; got != healthOK {
		t.Fatalf("health = %q after success, want %q", got, healthOK)
	}
}

func TestSnapshotQuotaFields(t *testing.T) {
	pool := &tokenPool{name: "token-test", logger: discardLogger()}
	pool.session = &cachedSession{
		status:     sessionStatusActive,
		instanceID: "inst-1",
		accessTier: "full",
		rateLimits: []freeSessionRateLimit{{Model: "minimax/minimax-m3", Remaining: 4, Limit: 25}},
	}

	snapshot := pool.snapshot()
	if snapshot.SessionAccessTier != "full" {
		t.Fatalf("access tier = %q", snapshot.SessionAccessTier)
	}
	if len(snapshot.RateLimits) != 1 || snapshot.RateLimits[0].Remaining != 4 {
		t.Fatalf("rate limits = %v", snapshot.RateLimits)
	}
	if snapshot.Health != healthOK {
		t.Fatalf("default health = %q", snapshot.Health)
	}
}

func TestParseUpstreamCooldown(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		status int
		want   time.Duration
	}{
		{"retryAfterMs field", `{"error":{"retryAfterMs":90000}}`, 429, 90 * time.Second},
		{"try-again text", `{"error":{"message":"try again in 1h 2m 3s"}}`, 429, time.Hour + 2*time.Minute + 3*time.Second},
		{"429 default", `{"error":{"message":"slow down"}}`, 429, defaultRateLimitCooldown},
		{"cap at 6h", `{"error":{"retryAfterMs":99999999}}`, 429, maxUpstreamCooldown},
		{"non-429 default", `{}`, 500, otherErrorCooldown},
	}
	for _, tc := range cases {
		if got := parseUpstreamCooldown([]byte(tc.body), tc.status); got != tc.want {
			t.Errorf("%s: parseUpstreamCooldown = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestClassifyForbidden(t *testing.T) {
	cases := []struct {
		body string
		want string
	}{
		{`{"error":"account banned for abuse"}`, healthBanned},
		{`{"error":"service unavailable in your country"}`, healthCountryBlock},
		{`{"error":"forbidden"}`, healthBlocked},
	}
	for _, tc := range cases {
		if got := classifyForbidden([]byte(tc.body)); got != tc.want {
			t.Errorf("classifyForbidden(%q) = %q, want %q", tc.body, got, tc.want)
		}
	}
}

func TestWaitingRoomErrorOrdering(t *testing.T) {
	first := &waitingRoomError{Position: 5, QueueDepth: 10}
	second := &waitingRoomError{Position: 2, QueueDepth: 10}
	if first.Error() == "" || second.Error() == "" {
		t.Fatal("error messages empty")
	}
	if !strings.Contains(first.Error(), "5/10") {
		t.Fatalf("message = %q, want position 5/10", first.Error())
	}
	_ = second
}
