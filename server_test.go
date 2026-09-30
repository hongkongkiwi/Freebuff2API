package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// upstreamStub implements the slice of the Codebuff API the proxy talks to.
type upstreamStub struct {
	mu             sync.Mutex
	chatBodies     []map[string]any
	sessionGets    int
	rateLimitProbe bool
	chatStatus     int
	chatResponse   string
	queuedPost     bool
}

func newUpstreamStub() *upstreamStub {
	return &upstreamStub{chatStatus: http.StatusOK}
}

func (s *upstreamStub) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/agent-runs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"runId":"run-stub-1"}`))
	})
	mux.HandleFunc("/api/v1/freebuff/session", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			s.mu.Lock()
			s.sessionGets++
			if r.Header.Get("x-freebuff-include-unused-rate-limits") == "1" {
				s.rateLimitProbe = true
			}
			s.mu.Unlock()
			_, _ = w.Write([]byte(`{"status":"active","instanceId":"inst-stub-1","expiresAt":"2030-01-01T00:00:00Z","accessTier":"full","rateLimits":[{"model":"minimax/minimax-m3","remaining":4,"limit":25}]}`))
		case http.MethodDelete:
			w.WriteHeader(http.StatusOK)
		default:
			s.mu.Lock()
			queued := s.queuedPost
			s.mu.Unlock()
			if queued {
				_, _ = w.Write([]byte(`{"status":"queued","instanceId":"inst-queued-1","position":2,"queueDepth":5,"estimatedWaitMs":1000}`))
				return
			}
			_, _ = w.Write([]byte(`{"status":"active","instanceId":"inst-stub-1","expiresAt":"2030-01-01T00:00:00Z"}`))
		}
	})
	mux.HandleFunc("/api/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.mu.Lock()
		s.chatBodies = append(s.chatBodies, body)
		status := s.chatStatus
		payload := s.chatResponse
		s.mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(status)
		if payload != "" {
			_, _ = w.Write([]byte(payload))
			return
		}
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl-stub\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"minimax/minimax-m3\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hello from stub\"},\"finish_reason\":null}]}\n\n" +
			"data: {\"id\":\"chatcmpl-stub\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"minimax/minimax-m3\",\"choices\":[],\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":3,\"total_tokens\":12}}\n\n" +
			"data: [DONE]\n\n"))
	})
	return mux
}

func (s *upstreamStub) lastChatBody(t *testing.T) map[string]any {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.chatBodies) == 0 {
		t.Fatalf("no chat requests reached the stub")
	}
	return s.chatBodies[len(s.chatBodies)-1]
}

func newTestServer(t *testing.T, stub *upstreamStub, upstreamURL string) *Server {
	t.Helper()
	cfg := Config{
		ListenAddr:          ":0",
		UpstreamBaseURL:     upstreamURL,
		AuthTokens:          []string{"test-upstream-token"},
		RotationInterval:    time.Hour,
		RequestTimeout:      5 * time.Second,
		APIKeys:             []string{"sk-test-client"},
		MaxRequestBodyMB:    1,
		ForceUpstreamStream: true,
		ToolCamouflage:      true,
		HarnessRewrites:     true,
		BuffyGuard:          true,
	}
	registry := NewModelRegistry(nil, discardLogger())
	registry.loadFallback()

	server := NewServer(cfg, discardLogger(), registry)
	return server
}

func TestServerOpenAIEndToEnd(t *testing.T) {
	stub := newUpstreamStub()
	handler := stub.handler()
	upstream := httptest.NewServer(handler)
	defer upstream.Close()

	server := newTestServer(t, stub, upstream.URL)

	rec := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		"model": "minimax/minimax-m3",
		"messages": [
			{"role": "system", "content": "You are Claude Code, Anthropic's official CLI. cc_version=9"},
			{"role": "user", "content": "hi"}
		],
		"tools": [{"type": "function", "function": {"name": "bash", "parameters": {"type": "object"}}}]
	}`))
	request.Header.Set("Authorization", "Bearer sk-test-client")
	server.Handler().ServeHTTP(rec, request)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var completion map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &completion); err != nil {
		t.Fatalf("non-stream response not JSON: %v\n%s", err, rec.Body.String())
	}
	if completion["object"] != "chat.completion" {
		t.Fatalf("object = %v", completion["object"])
	}
	choices := completion["choices"].([]any)
	choice := choices[0].(map[string]any)
	message := choice["message"].(map[string]any)
	if message["content"] != "hello from stub" {
		t.Fatalf("content = %v", message["content"])
	}
	if choice["finish_reason"] != "stop" {
		t.Fatalf("finish_reason = %v", choice["finish_reason"])
	}
	usage := completion["usage"].(map[string]any)
	if usage["total_tokens"] != 12.0 {
		t.Fatalf("usage = %v", usage)
	}

	body := stub.lastChatBody(t)

	// Forced upstream streaming.
	if body["stream"] != true {
		t.Fatalf("upstream stream flag = %v, want true", body["stream"])
	}
	// Buffy guard prepends the opening to the harness-rewritten system prompt.
	messages := body["messages"].([]any)
	system := messages[0].(map[string]any)
	content := system["content"].(string)
	if !strings.HasPrefix(content, buffySystemPromptOpening) {
		t.Fatalf("system prompt missing Buffy opening: %q", content)
	}
	if strings.Contains(content, "You are Claude Code") || strings.Contains(content, "cc_version=") {
		t.Fatalf("harness markers not rewritten: %q", content)
	}
	// Default stop sentinel and provider privacy flag.
	if stop, ok := body["stop"].([]any); !ok || len(stop) != 1 || stop[0] != defaultUpstreamStop {
		t.Fatalf("stop = %v", body["stop"])
	}
	provider := body["provider"].(map[string]any)
	if provider["data_collection"] != "deny" {
		t.Fatalf("provider = %v", provider)
	}
	// Tool camouflage on the wire.
	tools := body["tools"].([]any)
	names := toolFunctionNames(t, tools)
	if countNamedTools(names, "mcp__bash") != 1 || countNamedTools(names, "decide") != 1 {
		t.Fatalf("wire tool names = %v", names)
	}
	// Metadata fields.
	metadata := body["codebuff_metadata"].(map[string]any)
	if metadata["run_id"] == "" || metadata["cost_mode"] != "free" || metadata["client_id"] == "" || metadata["trace_session_id"] == "" {
		t.Fatalf("metadata = %v", metadata)
	}
	if metadata["freebuff_instance_id"] != "inst-stub-1" {
		t.Fatalf("instance id = %v", metadata["freebuff_instance_id"])
	}
}

func TestServerQuotaProbeHeader(t *testing.T) {
	stub := newUpstreamStub()
	stub.mu.Lock()
	stub.queuedPost = true
	stub.mu.Unlock()
	upstream := httptest.NewServer(stub.handler())
	defer upstream.Close()

	cfg := Config{RequestTimeout: 5 * time.Second}
	client := NewUpstreamClient(cfg)
	client.baseURL = upstream.URL
	pool := &tokenPool{
		name: "token-test", token: "k", cfg: cfg, client: client,
		runs: make(map[string]*managedRun), logger: discardLogger(),
	}
	// A cached queued session makes refreshSession poll via GET.
	pool.session = &cachedSession{status: sessionStatusQueued, instanceID: "inst-queued-1"}

	session, _, err := pool.refreshSession(context.Background())
	if err != nil {
		t.Fatalf("refreshSession: %v", err)
	}
	if session == nil || session.status != sessionStatusActive || session.instanceID != "inst-stub-1" {
		t.Fatalf("session = %+v, want active inst-stub-1", session)
	}
	if session.accessTier != "full" || len(session.rateLimits) != 1 {
		t.Fatalf("quota info not captured: tier=%q limits=%v", session.accessTier, session.rateLimits)
	}

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if !stub.rateLimitProbe {
		t.Fatalf("session GET did not send x-freebuff-include-unused-rate-limits")
	}
}

func TestServer429CooldownAndRetryAfter(t *testing.T) {
	stub := newUpstreamStub()
	stub.mu.Lock()
	stub.chatStatus = http.StatusTooManyRequests
	stub.chatResponse = `{"error":{"message":"try again in 0h 5m 0s","type":"rate_limit_error"}}`
	stub.mu.Unlock()
	upstream := httptest.NewServer(stub.handler())
	defer upstream.Close()

	server := newTestServer(t, stub, upstream.URL)

	rec := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"minimax/minimax-m3","messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Authorization", "Bearer sk-test-client")
	server.Handler().ServeHTTP(rec, request)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if retryAfter := rec.Header().Get("Retry-After"); retryAfter == "" {
		t.Fatalf("Retry-After header missing on 429 response")
	}

	snapshot := server.runs.Snapshots()[0]
	if snapshot.Health != healthRateLimited {
		t.Fatalf("health = %q, want %q", snapshot.Health, healthRateLimited)
	}
	if snapshot.CooldownUntil.IsZero() {
		t.Fatalf("pool not cooled down after 429")
	}
}

func TestServerAuthRejected(t *testing.T) {
	stub := newUpstreamStub()
	upstream := httptest.NewServer(stub.handler())
	defer upstream.Close()

	server := newTestServer(t, stub, upstream.URL)

	rec := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"minimax/minimax-m3","messages":[]}`))
	request.Header.Set("Authorization", "Bearer wrong-key")
	server.Handler().ServeHTTP(rec, request)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestServerModelsList(t *testing.T) {
	stub := newUpstreamStub()
	upstream := httptest.NewServer(stub.handler())
	defer upstream.Close()

	server := newTestServer(t, stub, upstream.URL)

	rec := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.Header.Set("Authorization", "Bearer sk-test-client")
	server.Handler().ServeHTTP(rec, request)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var list struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode models: %v", err)
	}
	if len(list.Data) == 0 {
		t.Fatalf("no models in fallback list")
	}
}

func TestServerBodyTooLarge(t *testing.T) {
	stub := newUpstreamStub()
	upstream := httptest.NewServer(stub.handler())
	defer upstream.Close()

	server := newTestServer(t, stub, upstream.URL)

	huge := `{"model":"minimax/minimax-m3","messages":[{"role":"user","content":"` + strings.Repeat("x", 2<<20) + `"}]}`
	rec := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(huge))
	request.Header.Set("Authorization", "Bearer sk-test-client")
	server.Handler().ServeHTTP(rec, request)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
}
