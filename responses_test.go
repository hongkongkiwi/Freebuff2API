package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func readCloserFromString(s string) io.ReadCloser {
	return io.NopCloser(strings.NewReader(s))
}

func readCloserFromReader(r io.Reader) io.ReadCloser {
	return io.NopCloser(r)
}

func TestConvertResponsesRequestStringInput(t *testing.T) {
	payload, model, stream, err := convertResponsesRequestToChat([]byte(`{
		"model": "z-ai/glm-5.2",
		"instructions": "Be terse.",
		"input": "Hello there",
		"max_output_tokens": 256,
		"temperature": 0.3
	}`))
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if model != "z-ai/glm-5.2" || stream {
		t.Fatalf("model=%q stream=%v", model, stream)
	}
	messages := payload["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("messages = %v, want system + user", messages)
	}
	system := messages[0].(map[string]any)
	if system["role"] != "system" || system["content"] != "Be terse." {
		t.Fatalf("system message = %v", system)
	}
	user := messages[1].(map[string]any)
	if user["role"] != "user" || user["content"] != "Hello there" {
		t.Fatalf("user message = %v", user)
	}
	if payload["max_tokens"] != 256 {
		t.Fatalf("max_tokens = %v, want 256", payload["max_tokens"])
	}
}

func TestConvertResponsesRequestItemTypes(t *testing.T) {
	payload, _, _, err := convertResponsesRequestToChat([]byte(`{
		"model": "m",
		"input": [
			{"type": "message", "role": "user", "content": [
				{"type": "input_text", "text": "list files"},
				{"type": "input_text", "text": " now"}
			]},
			{"type": "function_call", "call_id": "call_1", "name": "bash", "arguments": "{\"cmd\":\"ls\"}"},
			{"type": "function_call_output", "call_id": "call_1", "output": "file.txt"},
			{"type": "reasoning", "summary": []},
			{"type": "message", "role": "developer", "content": "extra system"}
		],
		"tools": [
			{"type": "function", "name": "bash", "description": "run", "parameters": {"type": "object"}},
			{"type": "web_search"}
		],
		"tool_choice": {"type": "function", "name": "bash"}
	}`))
	if err != nil {
		t.Fatalf("convert: %v", err)
	}

	messages := payload["messages"].([]any)
	if len(messages) != 4 {
		t.Fatalf("messages = %d entries: %v", len(messages), messages)
	}
	if messages[0].(map[string]any)["content"] != "list files now" {
		t.Fatalf("user message = %v", messages[0])
	}
	// assistant function_call message
	callMsg := messages[1].(map[string]any)
	calls := callMsg["tool_calls"].([]any)
	call := calls[0].(map[string]any)
	if call["id"] != "call_1" || call["type"] != "function" {
		t.Fatalf("tool call = %v", call)
	}
	fn := call["function"].(map[string]any)
	if fn["name"] != "bash" || fn["arguments"] != `{"cmd":"ls"}` {
		t.Fatalf("tool fn = %v", fn)
	}
	// tool result
	toolMsg := messages[2].(map[string]any)
	if toolMsg["role"] != "tool" || toolMsg["tool_call_id"] != "call_1" || toolMsg["content"] != "file.txt" {
		t.Fatalf("tool message = %v", toolMsg)
	}
	// developer → system (reasoning item skipped entirely)
	devMsg := messages[3].(map[string]any)
	if devMsg["role"] != "system" || devMsg["content"] != "extra system" {
		t.Fatalf("developer message = %v", devMsg)
	}

	tools := payload["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("tools = %v", tools)
	}
	nested := tools[0].(map[string]any)["function"].(map[string]any)
	if nested["name"] != "bash" {
		t.Fatalf("nested tool = %v", nested)
	}
	if tools[1].(map[string]any)["type"] != "web_search" {
		t.Fatalf("builtin tool = %v", tools[1])
	}

	choice := payload["tool_choice"].(map[string]any)
	if choice["type"] != "function" || choice["function"].(map[string]any)["name"] != "bash" {
		t.Fatalf("tool_choice = %v", choice)
	}
}

func TestConvertChatCompletionToResponses(t *testing.T) {
	final := map[string]any{
		"id":      "chatcmpl-abc",
		"model":   "z-ai/glm-5.2",
		"created": 1700000000.0,
		"choices": []any{map[string]any{
			"index": 0,
			"message": map[string]any{
				"role":    "assistant",
				"content": "done",
				"tool_calls": []any{map[string]any{
					"index": 0, "id": "call_x", "type": "function",
					"function": map[string]any{"name": "bash", "arguments": "{}"},
				}},
			},
			"finish_reason": "tool_calls",
		}},
		"usage": map[string]any{"prompt_tokens": 12, "completion_tokens": 3, "total_tokens": 15},
	}

	out := convertChatCompletionToResponses(final)
	if out["object"] != "response" || out["status"] != "completed" {
		t.Fatalf("response header = %v", out)
	}
	if !strings.HasPrefix(out["id"].(string), "resp_") {
		t.Fatalf("id = %v", out["id"])
	}
	output := out["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("output = %v", output)
	}
	message := output[0].(map[string]any)
	if message["type"] != "message" {
		t.Fatalf("first item = %v", message)
	}
	text := message["content"].([]any)[0].(map[string]any)
	if text["text"] != "done" || text["type"] != "output_text" {
		t.Fatalf("text part = %v", text)
	}
	call := output[1].(map[string]any)
	if call["type"] != "function_call" || call["call_id"] != "call_x" || call["name"] != "bash" {
		t.Fatalf("function call item = %v", call)
	}
	usage := out["usage"].(map[string]any)
	if usage["input_tokens"] != 12 || usage["output_tokens"] != 3 || usage["total_tokens"] != 15 {
		t.Fatalf("usage = %v", usage)
	}
}

func TestWriteResponsesStreamingEvents(t *testing.T) {
	body := "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Hi\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"model\":\"m\",\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":1,\"total_tokens\":6}}\n\n" +
		"data: [DONE]\n\n"

	rec := httptest.NewRecorder()
	resp := &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}
	resp.Body.Close()
	resp.Body = readCloserFromString(body)
	if err := writeResponsesStreamingResponse(rec, resp, "test-model"); err != nil {
		t.Fatalf("writeResponsesStreamingResponse: %v", err)
	}

	var eventNames []string
	var completed map[string]any
	for _, raw := range strings.Split(rec.Body.String(), "\n\n") {
		var eventType string
		for _, line := range strings.Split(raw, "\n") {
			if strings.HasPrefix(line, "event: ") {
				eventType = strings.TrimPrefix(line, "event: ")
			}
		}
		if eventType == "" {
			continue
		}
		eventNames = append(eventNames, eventType)
		if eventType == "response.completed" {
			dataLine := ""
			for _, line := range strings.Split(raw, "\n") {
				if strings.HasPrefix(line, "data: ") {
					dataLine = strings.TrimPrefix(line, "data: ")
				}
			}
			if err := json.Unmarshal([]byte(dataLine), &completed); err != nil {
				t.Fatalf("decode completed event: %v", err)
			}
		}
	}

	want := []string{
		"response.created",
		"response.output_item.added",
		"response.output_text.delta",
		"response.output_text.done",
		"response.output_item.done",
		"response.completed",
	}
	if !reflect.DeepEqual(eventNames, want) {
		t.Fatalf("events = %v, want %v", eventNames, want)
	}
	response := completed["response"].(map[string]any)
	if response["status"] != "completed" {
		t.Fatalf("completed response = %v", response)
	}
	usage := response["usage"].(map[string]any)
	assertJSONNumber(t, `usage["input_tokens"]`, usage["input_tokens"], 5)
	assertJSONNumber(t, `usage["output_tokens"]`, usage["output_tokens"], 1)
	assertJSONNumber(t, `usage["total_tokens"]`, usage["total_tokens"], 6)
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type = %q", ct)
	}
}

func TestWriteResponsesStreamingBlankStream(t *testing.T) {
	rec := httptest.NewRecorder()
	resp := &http.Response{StatusCode: http.StatusOK, Body: readCloserFromString("data: [DONE]\n\n")}
	err := writeResponsesStreamingResponse(rec, resp, "m")
	if err != errBlankUpstreamStream {
		t.Fatalf("err = %v, want errBlankUpstreamStream", err)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("bytes written for blank stream: %q", rec.Body.String())
	}
}

func TestResponsesStreamingStripsCamouflage(t *testing.T) {
	body := "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"t1\",\"type\":\"function\",\"function\":{\"name\":\"mcp__bash\",\"arguments\":\"{\\\"a\\\":1}\"}}]},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":1,\"id\":\"t2\",\"type\":\"function\",\"function\":{\"name\":\"decide\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n\n"

	rec := httptest.NewRecorder()
	resp := &http.Response{StatusCode: http.StatusOK, Body: readCloserFromString(body)}
	if err := writeResponsesStreamingResponseCtx(rec, resp, "m", camouflageCtx{strip: true}); err != nil {
		t.Fatalf("write: %v", err)
	}

	completedRaw := ""
	for _, frame := range strings.Split(rec.Body.String(), "\n\n") {
		if strings.HasPrefix(frame, "event: response.completed") {
			for _, line := range strings.Split(frame, "\n") {
				if strings.HasPrefix(line, "data: ") {
					completedRaw = strings.TrimPrefix(line, "data: ")
				}
			}
		}
	}
	if completedRaw == "" {
		t.Fatalf("response.completed event missing:\n%s", rec.Body.String())
	}
	var event struct {
		Response struct {
			Output []map[string]any `json:"output"`
		} `json:"response"`
	}
	if err := json.Unmarshal([]byte(completedRaw), &event); err != nil {
		t.Fatalf("decode completed: %v", err)
	}
	var functionCalls []string
	for _, item := range event.Response.Output {
		if item["type"] == "function_call" {
			functionCalls = append(functionCalls, item["name"].(string))
		}
	}
	if len(functionCalls) != 1 || functionCalls[0] != "bash" {
		t.Fatalf("function calls = %v, want exactly [bash] (mcp__ stripped, decide dropped)", functionCalls)
	}
}

func TestResponsesFailedEventOnMidStreamError(t *testing.T) {
	reader := &failAfterBodyReader{
		data: []byte("data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"partial\"},\"finish_reason\":null}]}\n\n"),
	}
	rec := httptest.NewRecorder()
	resp := &http.Response{StatusCode: http.StatusOK, Body: readCloserFromReader(reader)}
	err := writeResponsesStreamingResponseCtx(rec, resp, "m", camouflageCtx{strip: true})
	if err == nil {
		t.Fatal("expected an error from a mid-stream read failure")
	}

	var lastEvent string
	for _, frame := range strings.Split(rec.Body.String(), "\n\n") {
		if strings.HasPrefix(frame, "event: ") {
			lastEvent = strings.TrimSpace(strings.TrimPrefix(strings.SplitN(frame, "\n", 2)[0], "event: "))
		}
	}
	if lastEvent != "response.failed" {
		t.Fatalf("last event = %q, want response.failed; body:\n%s", lastEvent, rec.Body.String())
	}
}
