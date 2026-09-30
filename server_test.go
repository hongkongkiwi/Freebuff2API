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
	plainJSON      bool
	startCount     int
}

func newUpstreamStub() *upstreamStub {
	return &upstreamStub{chatStatus: http.StatusOK}
}

func (s *upstreamStub) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/agent-runs", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.startCount++
		s.mu.Unlock()
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
		s.mu.Lock()
		plain := s.plainJSON
		s.mu.Unlock()
		if plain {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"chatcmpl-plain","object":"chat.completion","created":1700000000,"model":"minimax/minimax-m3","choices":[{"index":0,"message":{"role":"assistant","content":"plain json reply","tool_calls":[{"id":"call_p","type":"function","function":{"name":"mcp__bash","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":7,"completion_tokens":2,"total_tokens":9}}`))
			return
		}
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
	return newTestServerWithConfig(t, stub, upstreamURL, nil)
}

func newTestServerWithConfig(t *testing.T, stub *upstreamStub, upstreamURL string, mutate func(*Config)) *Server {
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
	if mutate != nil {
		mutate(&cfg)
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

func TestServerStreamingFrameSeparation(t *testing.T) {
	stub := newUpstreamStub()
	upstream := httptest.NewServer(stub.handler())
	defer upstream.Close()

	server := newTestServer(t, stub, upstream.URL)

	rec := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"minimax/minimax-m3","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Authorization", "Bearer sk-test-client")
	server.Handler().ServeHTTP(rec, request)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type = %q", ct)
	}

	// SSE events must be blank-line separated; the relay must not glue all
	// data lines into a single event.
	frames := strings.Split(strings.TrimSuffix(rec.Body.String(), "\n\n"), "\n\n")
	dataFrames := 0
	sawDone := false
	for _, frame := range frames {
		if !strings.HasPrefix(frame, "data: ") {
			t.Fatalf("malformed frame %q", frame)
		}
		dataFrames++
		if strings.TrimPrefix(frame, "data: ") == "[DONE]" {
			sawDone = true
		}
	}
	if dataFrames < 2 {
		t.Fatalf("only %d SSE frames, want content frame + [DONE]; body:\n%s", dataFrames, rec.Body.String())
	}
	if !sawDone {
		t.Fatalf("[DONE] frame missing from relayed stream")
	}
	if !strings.Contains(rec.Body.String(), "hello from stub") {
		t.Fatalf("content delta missing from stream: %s", rec.Body.String())
	}
}

func TestServerNonStreamForceOff(t *testing.T) {
	cases := []struct {
		name       string
		path       string
		body       string
		expectType string
		check      func(t *testing.T, body []byte)
	}{
		{
			name:       "openai passthrough strips camouflage",
			path:       "/v1/chat/completions",
			body:       `{"model":"minimax/minimax-m3","messages":[{"role":"user","content":"hi"}]}`,
			expectType: "chat.completion",
			check: func(t *testing.T, body []byte) {
				var completion map[string]any
				if err := json.Unmarshal(body, &completion); err != nil {
					t.Fatalf("decode: %v", err)
				}
				choice := completion["choices"].([]any)[0].(map[string]any)
				message := choice["message"].(map[string]any)
				calls := message["tool_calls"].([]any)
				fn := calls[0].(map[string]any)["function"].(map[string]any)
				if fn["name"] != "bash" {
					t.Fatalf("tool name = %v, want camouflage stripped", fn["name"])
				}
				if message["content"] != "plain json reply" {
					t.Fatalf("content = %v", message["content"])
				}
			},
		},
		{
			name:       "claude conversion of plain json",
			path:       "/v1/messages",
			body:       `{"model":"minimax/minimax-m3","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`,
			expectType: "message",
			check: func(t *testing.T, body []byte) {
				var message map[string]any
				if err := json.Unmarshal(body, &message); err != nil {
					t.Fatalf("decode: %v", err)
				}
				if message["type"] != "message" {
					t.Fatalf("type = %v", message["type"])
				}
				if message["stop_reason"] != "tool_use" {
					t.Fatalf("stop_reason = %v, want tool_use", message["stop_reason"])
				}
				found := false
				for _, block := range message["content"].([]any) {
					if block.(map[string]any)["type"] == "tool_use" {
						found = true
					}
				}
				if !found {
					t.Fatalf("tool_use block missing: %v", message["content"])
				}
			},
		},
		{
			name:       "responses conversion of plain json",
			path:       "/v1/responses",
			body:       `{"model":"minimax/minimax-m3","input":"hi"}`,
			expectType: "response",
			check: func(t *testing.T, body []byte) {
				var response map[string]any
				if err := json.Unmarshal(body, &response); err != nil {
					t.Fatalf("decode: %v", err)
				}
				if response["object"] != "response" || response["status"] != "completed" {
					t.Fatalf("response = %v", response)
				}
				output := response["output"].([]any)
				if len(output) != 2 {
					t.Fatalf("output = %v", output)
				}
				call := output[1].(map[string]any)
				if call["type"] != "function_call" || call["name"] != "bash" {
					t.Fatalf("function call = %v", call)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := newUpstreamStub()
			stub.mu.Lock()
			stub.plainJSON = true
			stub.mu.Unlock()
			upstream := httptest.NewServer(stub.handler())
			defer upstream.Close()

			server := newTestServerWithConfig(t, stub, upstream.URL, func(cfg *Config) {
				cfg.ForceUpstreamStream = false
			})

			rec := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			request.Header.Set("Authorization", "Bearer sk-test-client")
			server.Handler().ServeHTTP(rec, request)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
			}
			tc.check(t, rec.Body.Bytes())
		})
	}
}

func TestProxyRetriesExhaustedWritesError(t *testing.T) {
	stub := newUpstreamStub()
	stub.mu.Lock()
	stub.chatStatus = http.StatusConflict
	stub.chatResponse = `{"error":"session_superseded"}`
	stub.mu.Unlock()
	upstream := httptest.NewServer(stub.handler())
	defer upstream.Close()

	server := newTestServer(t, stub, upstream.URL)

	rec := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"minimax/minimax-m3","messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Authorization", "Bearer sk-test-client")
	server.Handler().ServeHTTP(rec, request)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if strings.TrimSpace(rec.Body.String()) == "" {
		t.Fatal("empty response body after retries exhausted")
	}
}

func TestAuthorizedEitherHeader(t *testing.T) {
	stub := newUpstreamStub()
	upstream := httptest.NewServer(stub.handler())
	defer upstream.Close()

	server := newTestServer(t, stub, upstream.URL)

	cases := []struct {
		name    string
		headers map[string]string
		want    bool
	}{
		{"bearer only", map[string]string{"Authorization": "Bearer sk-test-client"}, true},
		{"x-api-key only", map[string]string{"x-api-key": "sk-test-client"}, true},
		{"wrong x-api-key must not shadow valid bearer", map[string]string{"x-api-key": "nope", "Authorization": "Bearer sk-test-client"}, true},
		{"wrong bearer with valid x-api-key", map[string]string{"x-api-key": "sk-test-client", "Authorization": "Bearer nope"}, true},
		{"both wrong", map[string]string{"x-api-key": "nope", "Authorization": "Bearer nope"}, false},
		{"none", map[string]string{}, false},
	}
	for _, tc := range cases {
		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		for key, value := range tc.headers {
			request.Header.Set(key, value)
		}
		if got := server.authorized(request); got != tc.want {
			t.Errorf("%s: authorized = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestDecideOnlyFinishDowngrade(t *testing.T) {
	stub := newUpstreamStub()
	stub.mu.Lock()
	stub.chatResponse = "data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"model\":\"minimax/minimax-m3\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"model\":\"minimax/minimax-m3\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"d1\",\"type\":\"function\",\"function\":{\"name\":\"decide\",\"arguments\":\"{\\\"decision\\\":\\\"x\\\"}\"}}]},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"model\":\"minimax/minimax-m3\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"model\":\"minimax/minimax-m3\",\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n" +
		"data: [DONE]\n\n"
	stub.mu.Unlock()
	upstream := httptest.NewServer(stub.handler())
	defer upstream.Close()

	server := newTestServer(t, stub, upstream.URL)

	rec := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"minimax/minimax-m3","messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Authorization", "Bearer sk-test-client")
	server.Handler().ServeHTTP(rec, request)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	var completion map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &completion); err != nil {
		t.Fatalf("decode: %v\n%s", err, rec.Body.String())
	}
	choice := completion["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "stop" {
		t.Fatalf("finish_reason = %v, want stop (decide-only response)", choice["finish_reason"])
	}
	message := choice["message"].(map[string]any)
	if _, present := message["tool_calls"]; present {
		t.Fatalf("tool_calls leaked: %v", message["tool_calls"])
	}
}

func TestNativeMcpToolPassthrough(t *testing.T) {
	stub := newUpstreamStub()
	stub.mu.Lock()
	stub.chatResponse = "data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"model\":\"minimax/minimax-m3\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"n1\",\"type\":\"function\",\"function\":{\"name\":\"mcp__github__get_me\",\"arguments\":\"{}\"}},{\"index\":1,\"id\":\"c1\",\"type\":\"function\",\"function\":{\"name\":\"mcp__bash\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n\n"
	stub.mu.Unlock()
	upstream := httptest.NewServer(stub.handler())
	defer upstream.Close()

	server := newTestServer(t, stub, upstream.URL)

	rec := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		"model":"minimax/minimax-m3",
		"messages":[{"role":"user","content":"hi"}],
		"tools":[
			{"type":"function","function":{"name":"bash"}},
			{"type":"function","function":{"name":"mcp__github__get_me"}}
		]
	}`))
	request.Header.Set("Authorization", "Bearer sk-test-client")
	server.Handler().ServeHTTP(rec, request)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	var completion map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &completion); err != nil {
		t.Fatalf("decode: %v", err)
	}
	message := completion["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	calls := message["tool_calls"].([]any)
	names := map[string]bool{}
	for _, raw := range calls {
		fn := raw.(map[string]any)["function"].(map[string]any)
		names[fn["name"].(string)] = true
	}
	if !names["mcp__github__get_me"] {
		t.Fatalf("native mcp__ tool name was stripped: %v", names)
	}
	if !names["bash"] {
		t.Fatalf("camouflaged tool name not stripped back: %v", names)
	}
	if len(names) != 2 {
		t.Fatalf("unexpected tool names: %v", names)
	}
}

func TestBlankDoneOnlyStreamSurfaces502(t *testing.T) {
	stub := newUpstreamStub()
	stub.mu.Lock()
	stub.chatResponse = "data: [DONE]\n\n"
	stub.mu.Unlock()
	upstream := httptest.NewServer(stub.handler())
	defer upstream.Close()

	server := newTestServer(t, stub, upstream.URL)

	rec := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"minimax/minimax-m3","messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Authorization", "Bearer sk-test-client")
	server.Handler().ServeHTTP(rec, request)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 for a [DONE]-only upstream stream", rec.Code)
	}
	if strings.TrimSpace(rec.Body.String()) == "" {
		t.Fatal("empty response body")
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.chatBodies) != maxProxyAttempts {
		t.Fatalf("upstream attempts = %d, want %d (blank stream must be retried)", len(stub.chatBodies), maxProxyAttempts)
	}
}

func TestConcurrentRotationSingleFlight(t *testing.T) {
	stub := newUpstreamStub()
	upstream := httptest.NewServer(stub.handler())
	defer upstream.Close()

	cfg := Config{RequestTimeout: 5 * time.Second, RotationInterval: time.Hour}
	client := NewUpstreamClient(cfg)
	client.baseURL = upstream.URL
	pool := &tokenPool{
		name: "token-test", token: "k", cfg: cfg, client: client,
		runs: make(map[string]*managedRun), logger: discardLogger(),
		chatGate: make(chan struct{}, 1),
	}

	const workers = 5
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		go func() {
			errs <- pool.rotateAgent(context.Background(), "base2-free")
		}()
	}
	for i := 0; i < workers; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("rotateAgent: %v", err)
		}
	}

	stub.mu.Lock()
	starts := stub.startCount
	stub.mu.Unlock()
	if starts != 1 {
		t.Fatalf("StartRun calls = %d, want 1 (concurrent rotations must collapse)", starts)
	}
	pool.mu.Lock()
	runs := len(pool.runs)
	pool.mu.Unlock()
	if runs != 1 {
		t.Fatalf("runs = %d, want 1", runs)
	}
}

func TestClaudeNonStreamKeepsThinkingBlocks(t *testing.T) {
	stub := newUpstreamStub()
	stub.mu.Lock()
	stub.chatResponse = "data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"model\":\"minimax/minimax-m3\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"reasoning_content\":\"thinking hard\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"model\":\"minimax/minimax-m3\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	stub.mu.Unlock()
	upstream := httptest.NewServer(stub.handler())
	defer upstream.Close()

	server := newTestServer(t, stub, upstream.URL)

	rec := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"minimax/minimax-m3","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Authorization", "Bearer sk-test-client")
	server.Handler().ServeHTTP(rec, request)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	var message map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &message); err != nil {
		t.Fatalf("decode: %v", err)
	}
	foundThinking := false
	for _, block := range message["content"].([]any) {
		if block.(map[string]any)["type"] == "thinking" {
			foundThinking = true
		}
	}
	if !foundThinking {
		t.Fatalf("reasoning was promoted to text instead of a thinking block: %v", message["content"])
	}
}

func TestAccumulatorFinalizeToolCallsHaveNoIndexField(t *testing.T) {
	// Non-streaming chat.completion tool_calls carry no index (that field
	// exists only on streaming deltas); strict validators reject it.
	acc := newChatCompletionAccumulator()
	chunk := accumulatorChunk(`{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"bash","arguments":"{}"}}]},"finish_reason":"tool_calls"}`)
	if err := acc.ingest(chunk); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	final := acc.finalize()
	message := finalizedMessage(t, final)
	for _, rawCall := range message["tool_calls"].([]any) {
		call := rawCall.(map[string]any)
		if _, present := call["index"]; present {
			t.Fatalf("tool_call carries a streaming-only index field: %v", call)
		}
		for _, key := range []string{"id", "type", "function"} {
			if _, present := call[key]; !present {
				t.Fatalf("tool_call missing %q: %v", key, call)
			}
		}
	}
}
