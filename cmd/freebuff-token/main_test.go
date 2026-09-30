package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func stubStatusServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	return server, &requests
}

func TestPollForTokenSuccess(t *testing.T) {
	server, requests := stubStatusServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"user":{"authToken":"tok-123","email":"user@example.com"}}`))
	})

	code := &cliCodeResponse{LoginURL: "https://x", FingerprintHash: "h", ExpiresAt: time.Now().Add(time.Hour).Format(time.RFC3339)}
	token, email, err := pollForToken(server.Client(), server.URL, "codebuff-cli-test", code, 5*time.Second, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("pollForToken: %v", err)
	}
	if token != "tok-123" || email != "user@example.com" {
		t.Fatalf("token=%q email=%q", token, email)
	}
	if requests.Load() != 1 {
		t.Fatalf("requests = %d, want 1", requests.Load())
	}
}

func TestPollForTokenRefusesAlreadyExpiredCode(t *testing.T) {
	server, requests := stubStatusServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("status endpoint must not be polled for an already-expired code")
	})

	code := &cliCodeResponse{LoginURL: "https://x", FingerprintHash: "h", ExpiresAt: time.Now().Add(-time.Minute).Format(time.RFC3339)}
	_, _, err := pollForToken(server.Client(), server.URL, "fp", code, 5*time.Second, 10*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "already expired") {
		t.Fatalf("err = %v, want an already-expired error", err)
	}
	if requests.Load() != 0 {
		t.Fatalf("requests = %d, want 0", requests.Load())
	}
}

func TestPollForTokenDeadlineHonorsServerExpiry(t *testing.T) {
	// Server code expires in 50ms even though the local timeout is 5s; the
	// poll must stop near the server expiry instead of running the full 5s.
	server, _ := stubStatusServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})

	code := &cliCodeResponse{LoginURL: "https://x", FingerprintHash: "h", ExpiresAt: time.Now().Add(50 * time.Millisecond).Format(time.RFC3339)}
	start := time.Now()
	_, _, err := pollForToken(server.Client(), server.URL, "fp", code, 5*time.Second, 10*time.Millisecond)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected an error when the login code expires")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("poll ran %v, want it to stop near the server-side expiry", elapsed)
	}
}

func TestPollForTokenTimesOutWithReason(t *testing.T) {
	server, _ := stubStatusServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"upstream down"}`))
	})

	code := &cliCodeResponse{LoginURL: "https://x", FingerprintHash: "h", ExpiresAt: time.Now().Add(time.Hour).Format(time.RFC3339)}
	_, _, err := pollForToken(server.Client(), server.URL, "fp", code, 40*time.Millisecond, 10*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v, want a timeout error", err)
	}
}

func TestPollForTokenWaitsThroughTransientErrors(t *testing.T) {
	var calls atomic.Int64
	server, _ := stubStatusServer(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"user":{"authToken":"late-token","email":"u@x"}}`))
	})

	code := &cliCodeResponse{LoginURL: "https://x", FingerprintHash: "h", ExpiresAt: time.Now().Add(time.Hour).Format(time.RFC3339)}
	token, _, err := pollForToken(server.Client(), server.URL, "fp", code, 5*time.Second, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("pollForToken: %v", err)
	}
	if token != "late-token" {
		t.Fatalf("token = %q", token)
	}
}
