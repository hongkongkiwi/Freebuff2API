package main

import (
	"encoding/json"
	"reflect"
	"testing"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

const decideSignatureToolJSON = `{"type":"function","function":{"name":"decide","description":"Records the routing decision taken for the current step. Bookkeeping only.","parameters":{"type":"object","properties":{"decision":{"type":"string","description":"Short label for the decision taken."}},"required":["decision"]}}}`

func mkFunctionTool(name string) map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name": name,
		},
	}
}

func toolsFromPayload(t *testing.T, payload map[string]any) []any {
	t.Helper()
	tools, ok := payload["tools"].([]any)
	if !ok {
		t.Fatalf("payload[\"tools\"] is not []any: %T", payload["tools"])
	}
	return tools
}

func toolFunctionNames(t *testing.T, tools []any) []string {
	t.Helper()
	names := make([]string, 0, len(tools))
	for i, raw := range tools {
		tool, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("tools[%d] is %T, want map[string]any", i, raw)
		}
		fn, ok := tool["function"].(map[string]any)
		if !ok {
			t.Fatalf("tools[%d][\"function\"] is %T, want map[string]any", i, tool["function"])
		}
		name, _ := fn["name"].(string)
		names = append(names, name)
	}
	return names
}

func countNamedTools(names []string, want string) int {
	count := 0
	for _, name := range names {
		if name == want {
			count++
		}
	}
	return count
}

func canonicalDecideTool(t *testing.T) map[string]any {
	t.Helper()
	var decoded any
	if err := json.Unmarshal([]byte(decideSignatureToolJSON), &decoded); err != nil {
		t.Fatalf("decode canonical decide tool JSON: %v", err)
	}
	tool, ok := decoded.(map[string]any)
	if !ok {
		t.Fatalf("canonical decide tool JSON is %T, want object", decoded)
	}
	return tool
}

// assertJSONNumber compares a decoded JSON-ish value against a numeric
// expectation without pinning the concrete numeric type (float64 vs int).
func assertJSONNumber(t *testing.T, label string, got any, want float64) {
	t.Helper()
	switch v := got.(type) {
	case float64:
		if v != want {
			t.Fatalf("%s = %v, want %v", label, v, want)
		}
	case int:
		if float64(v) != want {
			t.Fatalf("%s = %v, want %v", label, v, want)
		}
	case int64:
		if float64(v) != want {
			t.Fatalf("%s = %v, want %v", label, v, want)
		}
	case json.Number:
		f, err := v.Float64()
		if err != nil || f != want {
			t.Fatalf("%s = %v, want %v", label, got, want)
		}
	default:
		t.Fatalf("%s has unexpected type %T (%v), want number %v", label, got, got, want)
	}
}

func finalizedChoices(t *testing.T, final map[string]any) []any {
	t.Helper()
	raw, ok := final["choices"].([]any)
	if !ok {
		t.Fatalf("final[\"choices\"] is %T, want []any", final["choices"])
	}
	return raw
}

func finalizedChoice0(t *testing.T, final map[string]any) map[string]any {
	t.Helper()
	choices := finalizedChoices(t, final)
	if len(choices) != 1 {
		t.Fatalf("len(final[\"choices\"]) = %d, want 1", len(choices))
	}
	choice, ok := choices[0].(map[string]any)
	if !ok {
		t.Fatalf("choices[0] is %T, want map[string]any", choices[0])
	}
	return choice
}

func finalizedMessage(t *testing.T, final map[string]any) map[string]any {
	t.Helper()
	choice := finalizedChoice0(t, final)
	message, ok := choice["message"].(map[string]any)
	if !ok {
		t.Fatalf("choices[0][\"message\"] is %T, want map[string]any", choice["message"])
	}
	return message
}

func messageToolCallCount(t *testing.T, message map[string]any) int {
	t.Helper()
	raw, present := message["tool_calls"]
	if !present {
		return 0
	}
	calls, ok := raw.([]any)
	if !ok {
		t.Fatalf("message[\"tool_calls\"] is %T, want []any", raw)
	}
	return len(calls)
}

func findToolCallByFunctionName(t *testing.T, message map[string]any, name string) map[string]any {
	t.Helper()
	raw, present := message["tool_calls"]
	if !present {
		t.Fatalf("message has no tool_calls; want one named %q", name)
	}
	calls, ok := raw.([]any)
	if !ok {
		t.Fatalf("message[\"tool_calls\"] is %T, want []any", raw)
	}
	for _, rawCall := range calls {
		call, ok := rawCall.(map[string]any)
		if !ok {
			t.Fatalf("tool_call is %T, want map[string]any", rawCall)
		}
		fn, ok := call["function"].(map[string]any)
		if !ok {
			t.Fatalf("tool_call[\"function\"] is %T, want map[string]any", call["function"])
		}
		if fnName, _ := fn["name"].(string); fnName == name {
			return call
		}
	}
	t.Fatalf("no tool_call with function.name %q found", name)
	return nil
}

// accumulatorChunk builds a chat.completion.chunk body with the given choices
// payload embedded.
func accumulatorChunk(choicesPayload string) []byte {
	return []byte(`{"id":"chatcmpl-123","object":"chat.completion.chunk","created":1700000000,"model":"test-model","choices":[` + choicesPayload + `]}`)
}

// ---------------------------------------------------------------------------
// Tool-name camouflage helpers
// ---------------------------------------------------------------------------

func TestMcpToolPrefixConstant(t *testing.T) {
	if mcpToolPrefix != "mcp__" {
		t.Fatalf("mcpToolPrefix = %q, want %q", mcpToolPrefix, "mcp__")
	}
}

func TestToWireToolName(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty stays empty", "", ""},
		{"plain name prefixed", "bash", "mcp__bash"},
		{"underscores preserved", "read_file", "mcp__read_file"},
	}
	for _, tc := range cases {
		if got := toWireToolName(tc.in); got != tc.want {
			t.Errorf("%s: toWireToolName(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

func TestFromWireToolName(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"prefixed stripped", "mcp__bash", "bash"},
		{"plain unchanged", "bash", "bash"},
		{"empty unchanged", "", ""},
		{"bare prefix stripped to empty", "mcp__", ""},
		{"only one prefix stripped", "mcp__mcp__bash", "mcp__bash"},
	}
	for _, tc := range cases {
		if got := fromWireToolName(tc.in); got != tc.want {
			t.Errorf("%s: fromWireToolName(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

func TestToolNameRoundTrip(t *testing.T) {
	for _, name := range []string{"bash", "read_file", "a", "tool_with_underscores", "mcp-safe"} {
		wire := toWireToolName(name)
		if got := fromWireToolName(wire); got != name {
			t.Errorf("round trip %q: fromWireToolName(%q) = %q, want %q", name, wire, got, name)
		}
	}
}

func TestIsSignatureToolName(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"decide", true},
		{"", false},
		{"mcp__decide", false},
		{"decide2", false},
		{"Decide", false},
		{" decide", false},
	}
	for _, tc := range cases {
		if got := isSignatureToolName(tc.in); got != tc.want {
			t.Errorf("isSignatureToolName(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Request-side camouflage
// ---------------------------------------------------------------------------

func TestCamouflageToolsForUpstreamRenamesAndAppendsDecide(t *testing.T) {
	payload := map[string]any{
		"model": "test-model",
		"tools": []any{mkFunctionTool("bash"), mkFunctionTool("read_file")},
	}

	camouflageToolsForUpstream(payload)

	names := toolFunctionNames(t, toolsFromPayload(t, payload))
	if len(names) != 3 {
		t.Fatalf("tool names after camouflage = %v, want 3 tools", names)
	}
	if countNamedTools(names, "mcp__bash") != 1 || countNamedTools(names, "mcp__read_file") != 1 {
		t.Fatalf("tool names after camouflage = %v, want mcp__bash and mcp__read_file", names)
	}
	if countNamedTools(names, "decide") != 1 {
		t.Fatalf("tool names after camouflage = %v, want exactly one decide tool", names)
	}

	var wantDecide any
	if err := json.Unmarshal([]byte(decideSignatureToolJSON), &wantDecide); err != nil {
		t.Fatalf("decode canonical decide tool: %v", err)
	}
	for _, raw := range toolsFromPayload(t, payload) {
		tool := raw.(map[string]any)
		fn := tool["function"].(map[string]any)
		if fn["name"] != "decide" {
			continue
		}
		if !reflect.DeepEqual(tool, wantDecide) {
			encoded, _ := json.Marshal(tool)
			t.Fatalf("decide tool = %s, want canonical shape %s", encoded, decideSignatureToolJSON)
		}
		return
	}
	t.Fatal("decide tool not found after camouflage")
}

func TestCamouflageToolsForUpstreamIsIdempotent(t *testing.T) {
	payload := map[string]any{
		"model": "test-model",
		"tools": []any{mkFunctionTool("bash")},
	}

	camouflageToolsForUpstream(payload)
	once := append([]string(nil), toolFunctionNames(t, toolsFromPayload(t, payload))...)
	camouflageToolsForUpstream(payload)
	twice := toolFunctionNames(t, toolsFromPayload(t, payload))

	if !reflect.DeepEqual(once, twice) {
		t.Fatalf("second camouflage changed tool names: once = %v, twice = %v", once, twice)
	}
	for _, name := range twice {
		if len(name) >= 2*len(mcpToolPrefix) && name[:2*len(mcpToolPrefix)] == mcpToolPrefix+mcpToolPrefix {
			t.Fatalf("double prefix introduced: %q in %v", name, twice)
		}
	}
	if countNamedTools(twice, "mcp__bash") != 1 {
		t.Fatalf("tool names = %v, want exactly one mcp__bash", twice)
	}
	if countNamedTools(twice, "decide") != 1 {
		t.Fatalf("tool names = %v, want exactly one decide after two calls", twice)
	}
}

func TestCamouflageToolsForUpstreamKeepsExistingDecide(t *testing.T) {
	payload := map[string]any{
		"model": "test-model",
		"tools": []any{mkFunctionTool("bash"), mkFunctionTool("decide")},
	}

	camouflageToolsForUpstream(payload)
	camouflageToolsForUpstream(payload)

	tools := toolsFromPayload(t, payload)
	names := toolFunctionNames(t, tools)
	if len(tools) != 2 {
		t.Fatalf("len(tools) = %d (%v), want 2 (existing decide must not be duplicated)", len(tools), names)
	}
	if countNamedTools(names, "decide") != 1 {
		t.Fatalf("tool names = %v, want exactly one decide", names)
	}
	if countNamedTools(names, "mcp__bash") != 1 {
		t.Fatalf("tool names = %v, want exactly one mcp__bash", names)
	}
}

// ---------------------------------------------------------------------------
// Response-side camouflage stripping
// ---------------------------------------------------------------------------

func TestStripCamouflageFromPayloadRemovesDecideAndPrefixes(t *testing.T) {
	payload := map[string]any{
		"id":     "chatcmpl-x",
		"object": "chat.completion",
		"choices": []any{
			map[string]any{
				"index": 0,
				"message": map[string]any{
					"role":    "assistant",
					"content": "",
					"tool_calls": []any{
						map[string]any{
							"id":   "call_1",
							"type": "function",
							"function": map[string]any{
								"name":      "mcp__bash",
								"arguments": "{}",
							},
						},
						map[string]any{
							"id":   "call_2",
							"type": "function",
							"function": map[string]any{
								"name":      "decide",
								"arguments": `{"decision":"use bash"}`,
							},
						},
						map[string]any{
							"id":   "call_3",
							"type": "function",
							"function": map[string]any{
								"name":      "plain_tool",
								"arguments": "{}",
							},
						},
					},
				},
				"finish_reason": "tool_calls",
			},
			map[string]any{
				"index": 1,
				"delta": map[string]any{
					"tool_calls": []any{
						map[string]any{
							"index": 0,
							"id":    "call_4",
							"function": map[string]any{
								"name":      "mcp__edit",
								"arguments": "",
							},
						},
						map[string]any{
							"index": 1,
							"function": map[string]any{
								"name":      "decide",
								"arguments": "",
							},
						},
					},
				},
			},
		},
	}

	stripCamouflageFromPayload(payload)

	choices, ok := payload["choices"].([]any)
	if !ok || len(choices) != 2 {
		t.Fatalf("choices after strip = %v, want 2 entries", payload["choices"])
	}

	first := choices[0].(map[string]any)
	message := first["message"].(map[string]any)
	messageCalls := message["tool_calls"].([]any)
	if len(messageCalls) != 2 {
		t.Fatalf("message.tool_calls after strip has %d entries, want 2 (decide removed)", len(messageCalls))
	}
	names := toolFunctionNamesFromCalls(t, messageCalls)
	if !reflect.DeepEqual(names, []string{"bash", "plain_tool"}) {
		t.Fatalf("message.tool_calls names after strip = %v, want [bash plain_tool]", names)
	}

	second := choices[1].(map[string]any)
	delta := second["delta"].(map[string]any)
	deltaCalls := delta["tool_calls"].([]any)
	if len(deltaCalls) != 1 {
		t.Fatalf("delta.tool_calls after strip has %d entries, want 1 (decide removed)", len(deltaCalls))
	}
	deltaNames := toolFunctionNamesFromCalls(t, deltaCalls)
	if !reflect.DeepEqual(deltaNames, []string{"edit"}) {
		t.Fatalf("delta.tool_calls names after strip = %v, want [edit]", deltaNames)
	}
}

func toolFunctionNamesFromCalls(t *testing.T, calls []any) []string {
	t.Helper()
	names := make([]string, 0, len(calls))
	for i, raw := range calls {
		call, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("tool_calls[%d] is %T, want map[string]any", i, raw)
		}
		fn, ok := call["function"].(map[string]any)
		if !ok {
			t.Fatalf("tool_calls[%d][\"function\"] is %T, want map[string]any", i, call["function"])
		}
		name, _ := fn["name"].(string)
		names = append(names, name)
	}
	return names
}

// ---------------------------------------------------------------------------
// Forced upstream streaming
// ---------------------------------------------------------------------------

func TestForceUpstreamStreaming(t *testing.T) {
	cases := []struct {
		name         string
		payload      map[string]any
		wantStream   bool
		checkOptions func(t *testing.T, payload map[string]any)
	}{
		{
			name:       "absent stream flags",
			payload:    map[string]any{"model": "test-model"},
			wantStream: true,
			checkOptions: func(t *testing.T, payload map[string]any) {
				options, ok := payload["stream_options"].(map[string]any)
				if !ok {
					t.Fatalf("stream_options = %v (%T), want map with include_usage", payload["stream_options"], payload["stream_options"])
				}
				if options["include_usage"] != true {
					t.Fatalf("stream_options.include_usage = %v, want true", options["include_usage"])
				}
			},
		},
		{
			name:       "stream false overwritten, other keys preserved",
			payload:    map[string]any{"model": "test-model", "stream": false, "temperature": 0.5},
			wantStream: true,
			checkOptions: func(t *testing.T, payload map[string]any) {
				if payload["model"] != "test-model" || payload["temperature"] != 0.5 {
					t.Fatalf("unrelated keys mutated: %v", payload)
				}
			},
		},
		{
			name:       "stream true preserved",
			payload:    map[string]any{"stream": true},
			wantStream: true,
			checkOptions: func(t *testing.T, payload map[string]any) {
				if _, ok := payload["stream_options"].(map[string]any); !ok {
					t.Fatalf("stream_options missing or not a map: %v", payload["stream_options"])
				}
			},
		},
		{
			name:       "existing stream_options preserved and extended",
			payload:    map[string]any{"stream": false, "stream_options": map[string]any{"custom_key": "kept"}},
			wantStream: true,
			checkOptions: func(t *testing.T, payload map[string]any) {
				options := payload["stream_options"].(map[string]any)
				if options["custom_key"] != "kept" {
					t.Fatalf("stream_options.custom_key = %v, want kept", options["custom_key"])
				}
				if options["include_usage"] != true {
					t.Fatalf("stream_options.include_usage = %v, want true", options["include_usage"])
				}
			},
		},
		{
			name:       "existing include_usage false forced to true",
			payload:    map[string]any{"stream": true, "stream_options": map[string]any{"include_usage": false}},
			wantStream: true,
			checkOptions: func(t *testing.T, payload map[string]any) {
				options := payload["stream_options"].(map[string]any)
				if options["include_usage"] != true {
					t.Fatalf("stream_options.include_usage = %v, want true", options["include_usage"])
				}
			},
		},
	}

	for _, tc := range cases {
		forceUpstreamStreaming(tc.payload)
		if got := tc.payload["stream"]; got != tc.wantStream {
			t.Errorf("%s: payload[\"stream\"] = %v, want %v", tc.name, got, tc.wantStream)
		}
		if tc.checkOptions != nil {
			tc.checkOptions(t, tc.payload)
		}
	}
}

// ---------------------------------------------------------------------------
// chatCompletionAccumulator
// ---------------------------------------------------------------------------

func TestChatCompletionAccumulatorEndToEnd(t *testing.T) {
	acc := newChatCompletionAccumulator()

	chunks := [][]byte{
		// 1. role-only delta
		accumulatorChunk(`{"index":0,"delta":{"role":"assistant"},"finish_reason":null}`),
		// 2-3. content deltas
		accumulatorChunk(`{"index":0,"delta":{"content":"Hello"},"finish_reason":null}`),
		accumulatorChunk(`{"index":0,"delta":{"content":" world"},"finish_reason":null}`),
		// 4. reasoning delta
		accumulatorChunk(`{"index":0,"delta":{"reasoning_content":"thinking hard"},"finish_reason":null}`),
		// 5-7. one tool call split across three chunks, plus a second tool call
		accumulatorChunk(`{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_abc","type":"function","function":{"name":"bash","arguments":""}}]},"finish_reason":null}`),
		accumulatorChunk(`{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"cmd\":"}}]},"finish_reason":null}`),
		accumulatorChunk(`{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"ls -la\"}"}},{"index":1,"id":"call_def","type":"function","function":{"name":"edit","arguments":"{}"}}]},"finish_reason":null}`),
		// 8. finish chunk
		accumulatorChunk(`{"index":0,"delta":{},"finish_reason":"tool_calls"}`),
		// 9. usage-only chunk with empty choices
		[]byte(`{"id":"chatcmpl-123","object":"chat.completion.chunk","created":1700000000,"model":"test-model","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`),
	}

	for i, chunk := range chunks {
		if err := acc.ingest(chunk); err != nil {
			t.Fatalf("ingest chunk %d: unexpected error: %v", i+1, err)
		}
	}

	final := acc.finalize()

	if final["id"] != "chatcmpl-123" {
		t.Errorf("final[\"id\"] = %v, want chatcmpl-123", final["id"])
	}
	if final["object"] != "chat.completion" {
		t.Errorf("final[\"object\"] = %v, want chat.completion", final["object"])
	}
	if final["model"] != "test-model" {
		t.Errorf("final[\"model\"] = %v, want test-model", final["model"])
	}
	assertJSONNumber(t, `final["created"]`, final["created"], 1700000000)

	message := finalizedMessage(t, final)
	if message["role"] != "assistant" {
		t.Errorf("message.role = %v, want assistant", message["role"])
	}
	if message["content"] != "Hello world" {
		t.Errorf("message.content = %q, want %q", message["content"], "Hello world")
	}
	if message["reasoning_content"] != "thinking hard" {
		t.Errorf("message.reasoning_content = %q, want %q", message["reasoning_content"], "thinking hard")
	}

	if got := messageToolCallCount(t, message); got != 2 {
		t.Fatalf("message.tool_calls count = %d, want 2 (one merged across 3 chunks, one single)", got)
	}

	bashCall := findToolCallByFunctionName(t, message, "bash")
	if bashCall["id"] != "call_abc" {
		t.Errorf("bash tool_call id = %v, want call_abc", bashCall["id"])
	}
	if bashCall["type"] != "function" {
		t.Errorf("bash tool_call type = %v, want function", bashCall["type"])
	}
	bashFn := bashCall["function"].(map[string]any)
	if bashFn["arguments"] != `{"cmd":"ls -la"}` {
		t.Errorf("bash arguments = %q, want %q", bashFn["arguments"], `{"cmd":"ls -la"}`)
	}

	editCall := findToolCallByFunctionName(t, message, "edit")
	if editCall["id"] != "call_def" {
		t.Errorf("edit tool_call id = %v, want call_def", editCall["id"])
	}
	editFn := editCall["function"].(map[string]any)
	if editFn["arguments"] != "{}" {
		t.Errorf("edit arguments = %q, want %q", editFn["arguments"], "{}")
	}

	choice := finalizedChoice0(t, final)
	if choice["finish_reason"] != "tool_calls" {
		t.Errorf("choices[0].finish_reason = %v, want tool_calls", choice["finish_reason"])
	}

	usage, ok := final["usage"].(map[string]any)
	if !ok {
		t.Fatalf("final[\"usage\"] = %v (%T), want map", final["usage"], final["usage"])
	}
	assertJSONNumber(t, `usage["prompt_tokens"]`, usage["prompt_tokens"], 10)
	assertJSONNumber(t, `usage["completion_tokens"]`, usage["completion_tokens"], 5)
	assertJSONNumber(t, `usage["total_tokens"]`, usage["total_tokens"], 15)
}

func TestChatCompletionAccumulatorOmitsEmptyFieldsAndDefaultsFinishReason(t *testing.T) {
	acc := newChatCompletionAccumulator()
	chunks := [][]byte{
		accumulatorChunk(`{"index":0,"delta":{"role":"assistant"},"finish_reason":null}`),
		accumulatorChunk(`{"index":0,"delta":{"content":"Hi"},"finish_reason":null}`),
	}
	for i, chunk := range chunks {
		if err := acc.ingest(chunk); err != nil {
			t.Fatalf("ingest chunk %d: unexpected error: %v", i+1, err)
		}
	}

	final := acc.finalize()
	message := finalizedMessage(t, final)

	if _, present := message["reasoning_content"]; present {
		t.Errorf("message.reasoning_content present (%v), want omitted when empty", message["reasoning_content"])
	}
	if _, present := final["usage"]; present {
		t.Errorf("final[\"usage\"] present (%v), want omitted when no chunk carried usage", final["usage"])
	}
	if message["content"] != "Hi" {
		t.Errorf("message.content = %q, want %q", message["content"], "Hi")
	}
	choice := finalizedChoice0(t, final)
	if choice["finish_reason"] != "stop" {
		t.Errorf("choices[0].finish_reason = %v, want default %q", choice["finish_reason"], "stop")
	}
}

func TestChatCompletionAccumulatorIngestEdgeCases(t *testing.T) {
	acc := newChatCompletionAccumulator()
	for _, tc := range []struct {
		name  string
		input []byte
	}{
		{"empty input", []byte{}},
		{"whitespace input", []byte("  \n\t ")},
		{"done sentinel", []byte("[DONE]")},
	} {
		if err := acc.ingest(tc.input); err != nil {
			t.Errorf("%s: ingest returned error %v, want nil", tc.name, err)
		}
	}

	if err := acc.ingest([]byte("{not valid json")); err == nil {
		t.Error("ingest of invalid JSON returned nil error, want non-nil")
	}
}

func TestIsBlankUpstreamStream(t *testing.T) {
	roleChunk := accumulatorChunk(`{"index":0,"delta":{"role":"assistant"},"finish_reason":null}`)
	contentChunk := accumulatorChunk(`{"index":0,"delta":{"content":"Hello"},"finish_reason":null}`)
	reasoningChunk := accumulatorChunk(`{"index":0,"delta":{"reasoning_content":"hmm"},"finish_reason":null}`)
	toolCallChunk := accumulatorChunk(`{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"bash","arguments":""}}]},"finish_reason":null}`)

	cases := []struct {
		name   string
		chunks int
		ingest [][]byte
		want   bool
	}{
		{"nothing ingested", 0, nil, true},
		{"only role delta", 1, [][]byte{roleChunk}, true},
		{"only blank sentinels", 2, [][]byte{[]byte("[DONE]"), []byte(" ")}, true},
		{"content delta seen", 2, [][]byte{roleChunk, contentChunk}, false},
		{"reasoning delta seen", 1, [][]byte{reasoningChunk}, false},
		{"tool call delta seen", 1, [][]byte{toolCallChunk}, false},
	}

	for _, tc := range cases {
		acc := newChatCompletionAccumulator()
		for i, chunk := range tc.ingest {
			if err := acc.ingest(chunk); err != nil {
				t.Fatalf("%s: ingest %d: unexpected error: %v", tc.name, i+1, err)
			}
		}
		if got := isBlankUpstreamStream(tc.chunks, acc); got != tc.want {
			t.Errorf("%s: isBlankUpstreamStream(%d, acc) = %v, want %v", tc.name, tc.chunks, got, tc.want)
		}
	}

	// Blankness must be observable through finalize: empty content, no
	// reasoning_content, no tool_calls for a blank (role-only) stream.
	blankAcc := newChatCompletionAccumulator()
	if err := blankAcc.ingest(roleChunk); err != nil {
		t.Fatalf("ingest role chunk: %v", err)
	}
	blankFinal := blankAcc.finalize()
	blankMessage := finalizedMessage(t, blankFinal)
	if blankMessage["content"] != "" {
		t.Errorf("blank finalize content = %q, want empty", blankMessage["content"])
	}
	if _, present := blankMessage["reasoning_content"]; present {
		t.Errorf("blank finalize has reasoning_content %v, want omitted", blankMessage["reasoning_content"])
	}
	if got := messageToolCallCount(t, blankMessage); got != 0 {
		t.Errorf("blank finalize tool_calls count = %d, want 0", got)
	}
}

// ---------------------------------------------------------------------------
// Harness prompt rewrites
// ---------------------------------------------------------------------------

func TestRewriteHarnessPrompts(t *testing.T) {
	cases := []struct {
		name     string
		messages []any
		check    func(t *testing.T, payload map[string]any)
	}{
		{
			name: "all harness markers replaced in system message",
			messages: []any{
				map[string]any{
					"role":    "system",
					"content": "You are Claude Code, Anthropic's official CLI. cc_version=1.2.3",
				},
			},
			check: func(t *testing.T, payload map[string]any) {
				message := payload["messages"].([]any)[0].(map[string]any)
				want := "You are a coding assistant, a command line interface. cli_version=1.2.3"
				if message["content"] != want {
					t.Fatalf("system content = %q, want %q", message["content"], want)
				}
			},
		},
		{
			name: "partial replacement cc_version only",
			messages: []any{
				map[string]any{"role": "system", "content": "build cc_version=9 plus text"},
			},
			check: func(t *testing.T, payload map[string]any) {
				message := payload["messages"].([]any)[0].(map[string]any)
				want := "build cli_version=9 plus text"
				if message["content"] != want {
					t.Fatalf("system content = %q, want %q", message["content"], want)
				}
			},
		},
		{
			name: "user message untouched",
			messages: []any{
				map[string]any{
					"role":    "user",
					"content": "You are Claude Code, Anthropic's official CLI. cc_version=1.2.3",
				},
			},
			check: func(t *testing.T, payload map[string]any) {
				message := payload["messages"].([]any)[0].(map[string]any)
				want := "You are Claude Code, Anthropic's official CLI. cc_version=1.2.3"
				if message["content"] != want {
					t.Fatalf("user content = %q, want unchanged %q", message["content"], want)
				}
			},
		},
		{
			name: "non-string system content untouched",
			messages: []any{
				map[string]any{
					"role": "system",
					"content": []any{
						map[string]any{"type": "text", "text": "You are Claude Code, Anthropic's official CLI"},
					},
				},
			},
			check: func(t *testing.T, payload map[string]any) {
				message := payload["messages"].([]any)[0].(map[string]any)
				if _, isString := message["content"].(string); isString {
					t.Fatalf("system content became a string %q, want untouched non-string", message["content"])
				}
			},
		},
		{
			name: "unrelated system content untouched",
			messages: []any{
				map[string]any{"role": "system", "content": "Be helpful and concise."},
			},
			check: func(t *testing.T, payload map[string]any) {
				message := payload["messages"].([]any)[0].(map[string]any)
				if message["content"] != "Be helpful and concise." {
					t.Fatalf("system content = %q, want unchanged", message["content"])
				}
			},
		},
		{
			name: "mixed messages: only system rewritten",
			messages: []any{
				map[string]any{"role": "system", "content": "You are Claude Code"},
				map[string]any{"role": "user", "content": "You are Claude Code says the system"},
			},
			check: func(t *testing.T, payload map[string]any) {
				messages := payload["messages"].([]any)
				system := messages[0].(map[string]any)
				user := messages[1].(map[string]any)
				if system["content"] != "You are a coding assistant" {
					t.Fatalf("system content = %q, want rewritten", system["content"])
				}
				if user["content"] != "You are Claude Code says the system" {
					t.Fatalf("user content = %q, want unchanged", user["content"])
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := map[string]any{"model": "test-model", "messages": tc.messages}
			rewriteHarnessPrompts(payload)
			tc.check(t, payload)
		})
	}
}

// ---------------------------------------------------------------------------
// SSE chunk rewriting for pass-through streaming
// ---------------------------------------------------------------------------

func TestRewriteUpstreamChunkPassthrough(t *testing.T) {
	cases := []struct {
		name  string
		input []byte
	}{
		{"done sentinel", []byte("[DONE]")},
		{"invalid json", []byte(`{"choices":`)},
		{"garbage", []byte("not json at all")},
	}
	for _, tc := range cases {
		got := rewriteUpstreamChunk(tc.input)
		if !reflect.DeepEqual(got, tc.input) {
			t.Errorf("%s: rewriteUpstreamChunk(%q) = %q, want input unchanged", tc.name, tc.input, got)
		}
	}
}

func TestRewriteUpstreamChunkStripsCamouflage(t *testing.T) {
	input := []byte(`{"id":"c1","object":"chat.completion.chunk","model":"test-model","choices":[{"index":0,"delta":{"content":"Hi","tool_calls":[{"index":0,"id":"t1","type":"function","function":{"name":"mcp__bash","arguments":"{}"}},{"index":1,"id":"t2","type":"function","function":{"name":"decide","arguments":"{}"}}]},"finish_reason":null}]}`)

	output := rewriteUpstreamChunk(input)

	var chunk map[string]any
	if err := json.Unmarshal(output, &chunk); err != nil {
		t.Fatalf("rewritten chunk is not valid JSON: %v\noutput: %s", err, output)
	}
	if chunk["id"] != "c1" {
		t.Errorf("chunk id = %v, want c1", chunk["id"])
	}
	choices := chunk["choices"].([]any)
	choice := choices[0].(map[string]any)
	delta := choice["delta"].(map[string]any)
	if delta["content"] != "Hi" {
		t.Errorf("delta.content = %v, want Hi", delta["content"])
	}
	calls := delta["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("delta.tool_calls count = %d, want 1 (decide removed)", len(calls))
	}
	call := calls[0].(map[string]any)
	fn := call["function"].(map[string]any)
	if fn["name"] != "bash" {
		t.Errorf("remaining tool_call name = %v, want bash (mcp__ prefix stripped)", fn["name"])
	}
}

func TestRewriteUpstreamChunkPlainChunkContentPreserved(t *testing.T) {
	input := []byte(`{"id":"c2","object":"chat.completion.chunk","model":"test-model","choices":[{"index":0,"delta":{"content":"plain"},"finish_reason":null}]}`)

	output := rewriteUpstreamChunk(input)

	var chunk map[string]any
	if err := json.Unmarshal(output, &chunk); err != nil {
		t.Fatalf("rewritten chunk is not valid JSON: %v\noutput: %s", err, output)
	}
	choices := chunk["choices"].([]any)
	choice := choices[0].(map[string]any)
	delta := choice["delta"].(map[string]any)
	if delta["content"] != "plain" {
		t.Errorf("delta.content = %v, want plain", delta["content"])
	}
	if _, present := delta["tool_calls"]; present {
		t.Errorf("delta.tool_calls present (%v), want absent for plain chunk", delta["tool_calls"])
	}
}
