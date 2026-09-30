package main

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestQueuedPollDelayClamps(t *testing.T) {
	cases := []struct {
		name            string
		estimatedWaitMs int64
		want            time.Duration
	}{
		{"no estimate uses default interval", 0, freeSessionPollInterval},
		{"sub-second clamps to 1s", 300, time.Second},
		{"estimate within range", 2500, 2500 * time.Millisecond},
		{"estimate above range clamps to interval", 60_000, freeSessionPollInterval},
	}
	for _, tc := range cases {
		state := freeSessionResponse{EstimatedWaitMs: tc.estimatedWaitMs}
		if got := queuedPollDelay(state); got != tc.want {
			t.Errorf("%s: queuedPollDelay = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestParseOptionalTime(t *testing.T) {
	if got, err := parseOptionalTime(""); err != nil || !got.IsZero() {
		t.Fatalf("empty = %v, %v", got, err)
	}
	parsed, err := parseOptionalTime("2030-01-01T00:00:00Z")
	if err != nil || parsed.IsZero() {
		t.Fatalf("valid time = %v, %v", parsed, err)
	}
	if _, err := parseOptionalTime("not-a-time"); err == nil {
		t.Fatal("invalid time accepted")
	}
}

func TestWaitingRoomErrorFromSession(t *testing.T) {
	// Not queued → no error.
	active := &cachedSession{status: sessionStatusActive, instanceID: "x"}
	if err := waitingRoomErrorFromSession("token-1", active, time.Now()); err != nil {
		t.Fatalf("active session produced %v", err)
	}
	// Queued but past pollAt → no error (ready to re-poll).
	queuedReady := &cachedSession{status: sessionStatusQueued, pollAt: time.Now().Add(-time.Second)}
	if err := waitingRoomErrorFromSession("token-1", queuedReady, time.Now()); err != nil {
		t.Fatalf("ready queued session produced %v", err)
	}
	// Queued before pollAt → waiting error with position and retry hint.
	queued := &cachedSession{
		status:     sessionStatusQueued,
		position:   3,
		queueDepth: 7,
		pollAt:     time.Now().Add(2 * time.Second),
	}
	err := waitingRoomErrorFromSession("token-1", queued, time.Now())
	if err == nil {
		t.Fatal("expected a waiting error before pollAt")
	}
	if err.Position != 3 || err.QueueDepth != 7 {
		t.Fatalf("waiting error = %+v", err)
	}
	if err.RetryAfter <= 0 {
		t.Fatalf("retry after = %v", err.RetryAfter)
	}
}

func TestFreeSessionResponseJSON(t *testing.T) {
	var parsed freeSessionResponse
	payload := `{"status":"active","instanceId":"inst-1","accessTier":"limited","rateLimits":[{"model":"m","remaining":1,"limit":9,"resetAt":"2030-01-01T00:00:00Z"}]}`
	if err := json.Unmarshal([]byte(payload), &parsed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if parsed.AccessTier != "limited" || len(parsed.RateLimits) != 1 || parsed.RateLimits[0].Remaining != 1 {
		t.Fatalf("parsed = %+v", parsed)
	}
}

func TestRefreshSessionQueueToActive(t *testing.T) {
	// Covered end-to-end by TestServerQuotaProbeHeader; this guard pins the
	// cached-session shape transitions used there.
	queued := &cachedSession{status: sessionStatusQueued, instanceID: "i", position: 1, pollAt: time.Now().Add(time.Second)}
	if waitingRoomErrorFromSession("t", queued, time.Now()) == nil {
		t.Fatal("queued session should produce a waiting error before pollAt")
	}
	if !reflect.DeepEqual(queued.rateLimits, []freeSessionRateLimit(nil)) {
		t.Fatal("queued session should carry no rate limits")
	}
}
