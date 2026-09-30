package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestConvertClaudeSystemAndThinking(t *testing.T) {
	payload, model, stream, err := convertClaudeMessagesRequestToOpenAI([]byte(`{
		"model": "minimax/minimax-m3",
		"max_tokens": 512,
		"stream": true,
		"system": "Be brief.",
		"thinking": {"type": "enabled", "budget_tokens": 12000},
		"messages": [{"role": "user", "content": "hello"}]
	}`))
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if model != "minimax/minimax-m3" || !stream {
		t.Fatalf("model=%q stream=%v", model, stream)
	}
	if payload["reasoning_effort"] != "high" {
		t.Fatalf("reasoning_effort = %v, want high for 12000 budget", payload["reasoning_effort"])
	}
	messages := payload["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("messages = %v", messages)
	}
	if messages[0].(map[string]any)["role"] != "system" {
		t.Fatalf("first message = %v", messages[0])
	}
}

func TestConvertClaudeToolResultOrdering(t *testing.T) {
	payload, _, _, err := convertClaudeMessagesRequestToOpenAI([]byte(`{
		"model": "m",
		"messages": [
			{"role": "assistant", "content": [
				{"type": "text", "text": "running"},
				{"type": "tool_use", "id": "toolu_1", "name": "bash", "input": {"cmd": "ls"}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "toolu_1", "content": "file.txt"}
			]}
		],
		"tools": [{"name": "bash", "description": "run", "input_schema": {"type": "object"}}]
	}`))
	if err != nil {
		t.Fatalf("convert: %v", err)
	}

	messages := payload["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("messages = %d: %v", len(messages), messages)
	}
	assistant := messages[0].(map[string]any)
	if assistant["role"] != "assistant" {
		t.Fatalf("first = %v", assistant)
	}
	calls := assistant["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("tool calls = %v", calls)
	}
	fn := calls[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "bash" {
		t.Fatalf("fn = %v", fn)
	}
	toolMsg := messages[1].(map[string]any)
	if toolMsg["role"] != "tool" || toolMsg["tool_call_id"] != "toolu_1" {
		t.Fatalf("tool message = %v", toolMsg)
	}

	tools := payload["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %v", tools)
	}
	convertedFn := tools[0].(map[string]any)["function"].(map[string]any)
	if convertedFn["name"] != "bash" || convertedFn["parameters"] == nil {
		t.Fatalf("converted tool = %v", tools[0])
	}
}

func TestConvertOpenAINonStreamResponseToClaude(t *testing.T) {
	body := []byte(`{
		"id": "chatcmpl-1",
		"model": "minimax/minimax-m3",
		"choices": [{
			"index": 0,
			"message": {
				"role": "assistant",
				"content": "answer",
				"tool_calls": [{"id": "call_1", "type": "function", "function": {"name": "bash", "arguments": "{\"x\":1}"}}]
			},
			"finish_reason": "tool_calls"
		}],
		"usage": {"prompt_tokens": 100, "completion_tokens": 10, "total_tokens": 110, "prompt_tokens_details": {"cached_tokens": 40}}
	}`)

	converted, err := convertOpenAINonStreamResponseToClaude(body)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	var message map[string]any
	if err := json.Unmarshal(converted, &message); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if message["stop_reason"] != "tool_use" {
		t.Fatalf("stop_reason = %v", message["stop_reason"])
	}
	content := message["content"].([]any)
	foundToolUse := false
	for _, block := range content {
		blockMap := block.(map[string]any)
		if blockMap["type"] == "tool_use" && blockMap["name"] == "bash" {
			foundToolUse = true
		}
	}
	if !foundToolUse {
		t.Fatalf("tool_use block missing: %v", content)
	}
	usage := message["usage"].(map[string]any)
	if usage["input_tokens"] != 60.0 || usage["output_tokens"] != 10.0 || usage["cache_read_input_tokens"] != 40.0 {
		t.Fatalf("usage = %v (cached tokens must be subtracted from input)", usage)
	}
}

func TestConvertOpenAINonStreamEmptyContentGuard(t *testing.T) {
	converted, err := convertOpenAINonStreamResponseToClaude([]byte(`{
		"id": "chatcmpl-2", "model": "m",
		"choices": [{"index": 0, "message": {"role": "assistant", "content": ""}, "finish_reason": "stop"}]
	}`))
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if !strings.Contains(string(converted), `"type":"text"`) {
		t.Fatalf("empty content guard missing text block: %s", converted)
	}
}

func TestNormalizeClaudeErrorType(t *testing.T) {
	cases := []struct {
		status   int
		upstream string
		want     string
	}{
		{429, "", "rate_limit_error"},
		{401, "", "authentication_error"},
		{503, "", "overloaded_error"},
		{500, "weird_upstream_type", "api_error"},
		{400, "invalid_request_error", "invalid_request_error"},
	}
	for _, tc := range cases {
		if got := normalizeClaudeErrorType(tc.status, tc.upstream); got != tc.want {
			t.Errorf("normalizeClaudeErrorType(%d, %q) = %q, want %q", tc.status, tc.upstream, got, tc.want)
		}
	}
}

func TestMapOpenAIFinishReasonToClaude(t *testing.T) {
	if got := mapOpenAIFinishReasonToClaude("tool_calls"); got != "tool_use" {
		t.Fatalf("tool_calls → %q", got)
	}
	if got := mapOpenAIFinishReasonToClaude("length"); got != "max_tokens" {
		t.Fatalf("length → %q", got)
	}
	if got := mapOpenAIFinishReasonToClaude("stop"); got != "end_turn" {
		t.Fatalf("stop → %q", got)
	}
}

func TestClaudeBlankDoneOnlyStreamIsRetryable(t *testing.T) {
	rec := httptest.NewRecorder()
	resp := &http.Response{StatusCode: http.StatusOK, Body: readCloserFromString("data: [DONE]\n\n")}
	err := writeClaudeStreamingResponse(rec, resp, "test-model")
	if err != errBlankUpstreamStream {
		t.Fatalf("err = %v, want errBlankUpstreamStream for a [DONE]-only stream", err)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("bytes written for blank stream: %q", rec.Body.String())
	}
}

func TestClaudeStreamPostFinishDeltaStaysWellFormed(t *testing.T) {
	body := "data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"done text\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		// Sloppy upstream: content delta after the finish chunk.
		"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"straggler\"},\"finish_reason\":null}]}\n\n" +
		"data: [DONE]\n\n"

	rec := httptest.NewRecorder()
	resp := &http.Response{StatusCode: http.StatusOK, Body: readCloserFromString(body)}
	if err := writeClaudeStreamingResponse(rec, resp, "test-model"); err != nil {
		t.Fatalf("write: %v", err)
	}

	starts, stops := 0, 0
	lastEvent := ""
	for _, frame := range strings.Split(rec.Body.String(), "\n\n") {
		if !strings.HasPrefix(frame, "event: ") {
			continue
		}
		name := strings.TrimSpace(strings.TrimPrefix(strings.SplitN(frame, "\n", 2)[0], "event: "))
		switch name {
		case "content_block_start":
			starts++
		case "content_block_stop":
			stops++
		}
		lastEvent = name
	}
	if starts != stops {
		t.Fatalf("unbalanced blocks: %d starts vs %d stops\n%s", starts, stops, rec.Body.String())
	}
	if lastEvent != "message_stop" {
		t.Fatalf("last event = %q, want message_stop", lastEvent)
	}
	if strings.Contains(rec.Body.String(), "straggler") {
		t.Fatal("post-finish delta leaked into the terminal stream")
	}
}
