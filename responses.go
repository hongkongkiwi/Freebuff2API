package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// convertResponsesRequestToChat maps an OpenAI Responses API request body onto
// a chat-completions payload. Returns the payload, requested model, and
// whether the client asked for a stream.
func convertResponsesRequestToChat(body []byte) (map[string]any, string, bool, error) {
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, "", false, fmt.Errorf("request body must be valid JSON")
	}

	modelName := strings.TrimSpace(stringValue(root["model"]))
	stream := boolValue(root["stream"])
	out := map[string]any{
		"model":    modelName,
		"messages": []any{},
		"stream":   stream,
	}

	messages := make([]any, 0, 8)
	if instructions := strings.TrimSpace(stringValue(root["instructions"])); instructions != "" {
		messages = append(messages, map[string]any{"role": "system", "content": instructions})
	}

	switch input := root["input"].(type) {
	case string:
		if strings.TrimSpace(input) != "" {
			messages = append(messages, map[string]any{"role": "user", "content": input})
		}
	case []any:
		for _, rawItem := range input {
			item := mapValue(rawItem)
			if item == nil {
				continue
			}
			converted := convertResponsesInputItem(item)
			if converted == nil {
				continue
			}
			messages = append(messages, converted)
		}
	case nil:
		// No input at all: allowed (instructions-only requests).
	default:
		return nil, "", false, fmt.Errorf("input must be a string or an array")
	}

	out["messages"] = messages

	if maxTokens, ok := intValue(root["max_output_tokens"]); ok && maxTokens > 0 {
		out["max_tokens"] = maxTokens
	}
	if temperature, ok := floatValue(root["temperature"]); ok {
		out["temperature"] = temperature
	}
	if topP, ok := floatValue(root["top_p"]); ok {
		out["top_p"] = topP
	}

	if rawTools := sliceValue(root["tools"]); len(rawTools) > 0 {
		tools := make([]any, 0, len(rawTools))
		for _, rawTool := range rawTools {
			tool := mapValue(rawTool)
			if tool == nil {
				continue
			}
			converted := convertResponsesToolToChat(tool)
			if converted != nil {
				tools = append(tools, converted)
			}
		}
		if len(tools) > 0 {
			out["tools"] = tools
		}
	}

	switch choice := root["tool_choice"].(type) {
	case string:
		if choice == "auto" || choice == "none" || choice == "required" {
			out["tool_choice"] = choice
		}
	case map[string]any:
		if name := strings.TrimSpace(stringValue(choice["name"])); name != "" {
			out["tool_choice"] = map[string]any{
				"type":     "function",
				"function": map[string]any{"name": name},
			}
		}
	}

	return out, modelName, stream, nil
}

// convertResponsesInputItem converts one Responses input item into zero or one
// chat message (function_call items merge into the assistant message they
// belong to, so a single item yields a single message here).
func convertResponsesInputItem(item map[string]any) map[string]any {
	switch strings.ToLower(strings.TrimSpace(stringValue(item["type"]))) {
	case "message", "":
		role := strings.ToLower(strings.TrimSpace(stringValue(item["role"])))
		switch role {
		case "developer":
			role = "system"
		case "":
			role = "user"
		}
		content := convertResponsesContentToOpenAI(item["content"])
		if content == "" {
			return nil
		}
		return map[string]any{"role": role, "content": content}

	case "function_call":
		name := strings.TrimSpace(stringValue(item["name"]))
		if name == "" {
			return nil
		}
		return map[string]any{
			"role":    "assistant",
			"content": "",
			"tool_calls": []any{map[string]any{
				"id":   sanitizeClaudeToolID(stringValue(item["call_id"])),
				"type": "function",
				"function": map[string]any{
					"name":      name,
					"arguments": marshalJSONObject(item["arguments"]),
				},
			}},
		}

	case "function_call_output":
		callID := strings.TrimSpace(stringValue(item["call_id"]))
		if callID == "" {
			return nil
		}
		return map[string]any{
			"role":         "tool",
			"tool_call_id": sanitizeClaudeToolID(callID),
			"content":      convertResponsesOutputToText(item["output"]),
		}

	default:
		// reasoning and other item types carry no upstream-usable content.
		return nil
	}
}

func convertResponsesContentToOpenAI(content any) string {
	switch typed := content.(type) {
	case string:
		return typed
	case []any:
		var builder strings.Builder
		for _, rawPart := range typed {
			part := mapValue(rawPart)
			if part == nil {
				continue
			}
			switch strings.ToLower(strings.TrimSpace(stringValue(part["type"]))) {
			case "input_text", "output_text", "summary_text", "text":
				builder.WriteString(stringValue(part["text"]))
			case "refusal":
				builder.WriteString(stringValue(part["refusal"]))
			}
		}
		return builder.String()
	default:
		return ""
	}
}

func convertResponsesOutputToText(output any) string {
	switch typed := output.(type) {
	case string:
		return typed
	case []any:
		var builder strings.Builder
		for _, rawPart := range typed {
			part := mapValue(rawPart)
			if part == nil {
				continue
			}
			if strings.EqualFold(stringValue(part["type"]), "output_text") {
				builder.WriteString(stringValue(part["text"]))
			}
		}
		return builder.String()
	case nil:
		return ""
	default:
		encoded, _ := json.Marshal(typed)
		return string(encoded)
	}
}

// convertResponsesToolToChat maps a flat Responses tool definition onto the
// nested chat-completions shape. Non-function tool types pass through with
// their type preserved.
func convertResponsesToolToChat(tool map[string]any) map[string]any {
	toolType := strings.ToLower(strings.TrimSpace(stringValue(tool["type"])))
	if toolType != "" && toolType != "function" {
		return cloneMap(tool)
	}
	function := map[string]any{
		"name": stringValue(tool["name"]),
	}
	if description, ok := tool["description"]; ok {
		function["description"] = description
	}
	if parameters, ok := tool["parameters"]; ok {
		function["parameters"] = parameters
	}
	return map[string]any{"type": "function", "function": function}
}

// convertChatCompletionToResponses builds a Responses API response object from
// a reassembled chat completion.
func convertChatCompletionToResponses(final map[string]any) map[string]any {
	chatID := stringValue(final["id"])
	responseID := "resp_" + strings.TrimPrefix(chatID, "chatcmpl-")

	output := make([]any, 0, 2)
	choices := sliceValue(final["choices"])
	if len(choices) > 0 {
		choice := mapValue(choices[0])
		message := mapValue(choice["message"])
		if text := stringValue(message["content"]); text != "" {
			output = append(output, map[string]any{
				"id":     "msg_" + uuid.NewString(),
				"type":   "message",
				"role":   "assistant",
				"status": "completed",
				"content": []any{map[string]any{
					"type":        "output_text",
					"text":        text,
					"annotations": []any{},
				}},
			})
		}
		for _, rawCall := range sliceValue(message["tool_calls"]) {
			call := mapValue(rawCall)
			function := mapValue(call["function"])
			output = append(output, map[string]any{
				"id":        "fc_" + uuid.NewString(),
				"type":      "function_call",
				"status":    "completed",
				"call_id":   stringValue(call["id"]),
				"name":      stringValue(function["name"]),
				"arguments": stringValue(function["arguments"]),
			})
		}
	}

	// Claude-style decide-only upstream replies leave no output items; emit
	// an empty assistant message so the response is never empty.
	if len(output) == 0 {
		output = append(output, map[string]any{
			"id":     "msg_" + uuid.NewString(),
			"type":   "message",
			"role":   "assistant",
			"status": "completed",
			"content": []any{map[string]any{
				"type":        "output_text",
				"text":        "",
				"annotations": []any{},
			}},
		})
	}

	response := map[string]any{
		"id":                  responseID,
		"object":              "response",
		"created_at":          time.Now().Unix(),
		"status":              "completed",
		"model":               stringValue(final["model"]),
		"output":              output,
		"parallel_tool_calls": true,
		"tool_choice":         "auto",
		"tools":               []any{},
	}

	if usage := mapValue(final["usage"]); usage != nil {
		inputTokens, _ := intValue(usage["prompt_tokens"])
		outputTokens, _ := intValue(usage["completion_tokens"])
		totalTokens, _ := intValue(usage["total_tokens"])
		response["usage"] = map[string]any{
			"input_tokens":  inputTokens,
			"output_tokens": outputTokens,
			"total_tokens":  totalTokens,
		}
	}

	return response
}

// responsesEvent is one SSE event in the Responses API stream.
type responsesEvent struct {
	Name    string
	Payload map[string]any
}

// writeResponsesStreamingResponse converts a chat-completions SSE stream into
// Responses API events with response-side camouflage stripping enabled.
func writeResponsesStreamingResponse(w http.ResponseWriter, resp *http.Response, requestedModel string) error {
	return writeResponsesStreamingResponseCtx(w, resp, requestedModel, camouflageCtx{strip: true})
}

// writeResponsesStreamingResponseCtx converts a chat-completions SSE stream
// into Responses API events. Headers are committed with the first event so a
// blank upstream stream can still be retried.
func writeResponsesStreamingResponseCtx(w http.ResponseWriter, resp *http.Response, requestedModel string, cam camouflageCtx) error {
	flusher, _ := w.(http.Flusher)
	reader := bufio.NewReader(resp.Body)
	committed := false
	commitHeader := func() {
		if !committed {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.WriteHeader(resp.StatusCode)
			committed = true
		}
	}

	writeEvents := func(events []responsesEvent) error {
		if len(events) == 0 {
			return nil
		}
		commitHeader()
		for _, event := range events {
			payload, err := json.Marshal(event.Payload)
			if err != nil {
				continue
			}
			if _, err := io.WriteString(w, "event: "+event.Name+"\ndata: "+string(payload)+"\n\n"); err != nil {
				return err
			}
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	}

	responseID := "resp_" + uuid.NewString()
	messageItemID := "msg_" + uuid.NewString()
	var textContent strings.Builder
	var functionCalls []*responsesFunctionCallState
	payloadCount := 0
	var usage map[string]any

	// response.created is held until the first upstream payload arrives so a
	// fully blank upstream stream writes nothing and can be retried.
	createdEvent := []responsesEvent{{
		Name: "response.created",
		Payload: map[string]any{
			"type": "response.created",
			"response": map[string]any{
				"id": responseID, "object": "response", "status": "in_progress",
				"model": requestedModel, "output": []any{},
			},
		},
	}}

	messageItemAdded := false
	emitTextDelta := func(delta string) error {
		events := make([]responsesEvent, 0, 3)
		events = append(events, createdEvent...)
		createdEvent = nil
		if !messageItemAdded {
			events = append(events, responsesEvent{
				Name: "response.output_item.added",
				Payload: map[string]any{
					"type": "response.output_item.added", "output_index": 0,
					"item": map[string]any{
						"id": messageItemID, "type": "message", "role": "assistant",
						"status": "in_progress", "content": []any{},
					},
				},
			})
			messageItemAdded = true
		}
		events = append(events, responsesEvent{
			Name: "response.output_text.delta",
			Payload: map[string]any{
				"type": "response.output_text.delta", "item_id": messageItemID,
				"output_index": 0, "content_index": 0, "delta": delta,
			},
		})
		return writeEvents(events)
	}

	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			trimmed := bytes.TrimSpace(line)
			if len(trimmed) > 0 && !bytes.HasPrefix(trimmed, []byte(":")) && bytes.HasPrefix(trimmed, []byte("data:")) {
				payload := bytes.TrimSpace(trimmed[5:])
				if len(payload) > 0 && !bytes.Equal(payload, []byte("[DONE]")) {
					payloadCount++
					var chunk struct {
						Choices []struct {
							Delta struct {
								Content   string                 `json:"content"`
								ToolCalls []openAIStreamToolCall `json:"tool_calls"`
							} `json:"delta"`
						} `json:"choices"`
						Usage map[string]any `json:"usage"`
					}
					if jsonErr := json.Unmarshal(payload, &chunk); jsonErr == nil {
						if chunk.Usage != nil {
							usage = chunk.Usage
						}
						for _, choice := range chunk.Choices {
							if choice.Delta.Content != "" {
								if err := emitTextDelta(choice.Delta.Content); err != nil {
									return err
								}
								textContent.WriteString(choice.Delta.Content)
							}
							for _, toolCall := range choice.Delta.ToolCalls {
								for len(functionCalls) <= toolCall.Index {
									functionCalls = append(functionCalls, &responsesFunctionCallState{})
								}
								state := functionCalls[toolCall.Index]
								if state.Skipped {
									continue
								}
								if toolCall.ID != "" {
									state.CallID = toolCall.ID
								}
								if toolCall.Function.Name != "" {
									name, keep := cam.clientToolName(toolCall.Function.Name)
									if !keep {
										state.Skipped = true
										continue
									}
									state.Name = name
								}
								state.Arguments.WriteString(toolCall.Function.Arguments)
							}
						}
					}
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			if payloadCount == 0 && !committed {
				return errBlankUpstreamStream
			}
			if committed {
				// The client already saw events; close the stream out with a
				// terminal failure so it never hangs waiting for completion.
				_ = writeEvents([]responsesEvent{{
					Name: "response.failed",
					Payload: map[string]any{
						"type": "response.failed",
						"response": map[string]any{
							"id": responseID, "object": "response", "status": "failed",
							"model": requestedModel, "output": []any{},
							"error": map[string]any{"code": "upstream_error", "message": err.Error()},
						},
					},
				}})
			}
			return err
		}
	}

	if payloadCount == 0 {
		return errBlankUpstreamStream
	}

	output := make([]any, 0, 1+len(functionCalls))
	outputIndex := 0
	events := make([]responsesEvent, 0, 8)
	events = append(events, createdEvent...)
	createdEvent = nil

	// When nothing streamed, emit the whole message item now; when text
	// streamed, item.added already went out with the first delta and only the
	// done events remain.
	messageItem := map[string]any{
		"id": messageItemID, "type": "message", "role": "assistant",
		"status": "completed",
		"content": []any{map[string]any{
			"type": "output_text", "text": textContent.String(),
			"annotations": []any{},
		}},
	}
	textDoneEvent := responsesEvent{
		Name: "response.output_text.done",
		Payload: map[string]any{
			"type": "response.output_text.done", "item_id": messageItemID,
			"output_index": outputIndex, "content_index": 0, "text": textContent.String(),
		},
	}
	itemDoneEvent := responsesEvent{
		Name: "response.output_item.done",
		Payload: map[string]any{
			"type": "response.output_item.done", "output_index": outputIndex,
			"item": messageItem,
		},
	}
	if !messageItemAdded {
		events = append(events,
			responsesEvent{
				Name: "response.output_item.added",
				Payload: map[string]any{
					"type": "response.output_item.added", "output_index": outputIndex,
					"item": map[string]any{
						"id": messageItemID, "type": "message", "role": "assistant",
						"status": "in_progress", "content": []any{},
					},
				},
			},
			textDoneEvent,
			itemDoneEvent,
		)
	} else {
		events = append(events, textDoneEvent, itemDoneEvent)
	}
	output = append(output, messageItem)
	outputIndex++

	for _, call := range functionCalls {
		if call.Name == "" || call.Skipped {
			continue
		}
		itemID := "fc_" + uuid.NewString()
		args := call.Arguments.String()
		events = append(events,
			responsesEvent{
				Name: "response.output_item.added",
				Payload: map[string]any{
					"type": "response.output_item.added", "output_index": outputIndex,
					"item": map[string]any{
						"id": itemID, "type": "function_call", "status": "in_progress",
						"call_id": call.CallID, "name": call.Name, "arguments": "",
					},
				},
			},
			responsesEvent{
				Name: "response.function_call_arguments.done",
				Payload: map[string]any{
					"type": "response.function_call_arguments.done", "item_id": itemID,
					"output_index": outputIndex, "arguments": args,
				},
			},
			responsesEvent{
				Name: "response.output_item.done",
				Payload: map[string]any{
					"type": "response.output_item.done", "output_index": outputIndex,
					"item": map[string]any{
						"id": itemID, "type": "function_call", "status": "completed",
						"call_id": call.CallID, "name": call.Name, "arguments": args,
					},
				},
			},
		)
		output = append(output, map[string]any{
			"id": itemID, "type": "function_call", "status": "completed",
			"call_id": call.CallID, "name": call.Name, "arguments": args,
		})
		outputIndex++
	}

	completedResponse := map[string]any{
		"id": responseID, "object": "response", "status": "completed",
		"model": requestedModel, "output": output,
	}
	if usage != nil {
		inputTokens, _ := intValue(usage["prompt_tokens"])
		outputTokens, _ := intValue(usage["completion_tokens"])
		totalTokens, _ := intValue(usage["total_tokens"])
		completedResponse["usage"] = map[string]any{
			"input_tokens": inputTokens, "output_tokens": outputTokens,
			"total_tokens": totalTokens,
		}
	}
	events = append(events, responsesEvent{
		Name:    "response.completed",
		Payload: map[string]any{"type": "response.completed", "response": completedResponse},
	})

	return writeEvents(events)
}

type responsesFunctionCallState struct {
	CallID    string
	Name      string
	Skipped   bool
	Arguments strings.Builder
}
