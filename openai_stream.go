package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// errBlankUpstreamStream signals a 2xx upstream response whose SSE body
// produced no data payloads. Callers may retry it against a fresh session
// as long as nothing has been written to the client yet.
var errBlankUpstreamStream = errors.New("upstream stream produced no data")

// Tool camouflage: the free-tier backend expects tool calls that look like
// they come from the official CLI's MCP toolset, so client tools are prefixed
// on the wire and a well-known bookkeeping tool is appended to satisfy the
// upstream toolset signature check.
const mcpToolPrefix = "mcp__"

const signatureToolName = "decide"

func toWireToolName(name string) string {
	if name == "" {
		return name
	}
	return mcpToolPrefix + name
}

func fromWireToolName(name string) string {
	return strings.TrimPrefix(name, mcpToolPrefix)
}

func isSignatureToolName(name string) bool {
	return name == signatureToolName
}

// signatureTool returns the bookkeeping tool appended to client toolsets. Its
// shape must match the upstream toolset signature check exactly.
func signatureTool() map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        signatureToolName,
			"description": "Records the routing decision taken for the current step. Bookkeeping only.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"decision": map[string]any{
						"type":        "string",
						"description": "Short label for the decision taken.",
					},
				},
				"required": []any{"decision"},
			},
		},
	}
}

// camouflageToolsForUpstream renames client tools to their wire names, appends
// the signature tool when missing, and rewrites function references in
// tool_choice. It is idempotent.
func camouflageToolsForUpstream(payload map[string]any) {
	tools, ok := payload["tools"].([]any)
	if !ok || len(tools) == 0 {
		return
	}

	hasSignature := false
	for _, raw := range tools {
		tool, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := tool["function"].(map[string]any)
		if !ok {
			continue
		}
		name, _ := fn["name"].(string)
		if isSignatureToolName(name) {
			hasSignature = true
			continue
		}
		if name == "" || strings.HasPrefix(name, mcpToolPrefix) {
			continue
		}
		fn["name"] = toWireToolName(name)
	}
	if !hasSignature {
		payload["tools"] = append(tools, signatureTool())
	}

	if choice, ok := payload["tool_choice"].(map[string]any); ok {
		if fn, ok := choice["function"].(map[string]any); ok {
			if name, ok := fn["name"].(string); ok && name != "" && !isSignatureToolName(name) && !strings.HasPrefix(name, mcpToolPrefix) {
				fn["name"] = toWireToolName(name)
			}
		}
	}
}

// stripCamouflageFromPayload removes the signature tool from response tool
// calls and strips wire-name prefixes so clients only ever see their own tool
// names. Applies to both message.tool_calls and delta.tool_calls.
func stripCamouflageFromPayload(payload map[string]any) {
	choices, ok := payload["choices"].([]any)
	if !ok {
		return
	}
	for _, rawChoice := range choices {
		choice, ok := rawChoice.(map[string]any)
		if !ok {
			continue
		}
		for _, field := range []string{"message", "delta"} {
			container, ok := choice[field].(map[string]any)
			if !ok {
				continue
			}
			calls, ok := container["tool_calls"].([]any)
			if !ok {
				continue
			}
			kept := make([]any, 0, len(calls))
			for _, rawCall := range calls {
				call, ok := rawCall.(map[string]any)
				if !ok {
					kept = append(kept, rawCall)
					continue
				}
				fn, ok := call["function"].(map[string]any)
				if !ok {
					kept = append(kept, call)
					continue
				}
				name, _ := fn["name"].(string)
				if isSignatureToolName(name) {
					continue
				}
				fn["name"] = fromWireToolName(name)
				kept = append(kept, call)
			}
			if len(kept) == 0 {
				delete(container, "tool_calls")
			} else {
				container["tool_calls"] = kept
			}
		}
	}
}

// forceUpstreamStreaming pins stream:true plus usage reporting on the upstream
// payload; the free tier is unreliable for non-streamed completions, so
// non-stream clients get a reassembled response instead.
func forceUpstreamStreaming(payload map[string]any) {
	payload["stream"] = true
	options, ok := payload["stream_options"].(map[string]any)
	if !ok {
		options = make(map[string]any)
		payload["stream_options"] = options
	}
	options["include_usage"] = true
}

// harnessPromptReplacements disguises well-known coding-harness markers in
// system prompts, which the upstream otherwise flags.
var harnessPromptReplacements = [][2]string{
	{"You are Claude Code", "You are a coding assistant"},
	{"Anthropic's official CLI", "a command line interface"},
	{"cc_version=", "cli_version="},
}

// rewriteHarnessPrompts applies the harness replacements to string content of
// system messages only.
func rewriteHarnessPrompts(payload map[string]any) {
	messages, ok := payload["messages"].([]any)
	if !ok {
		return
	}
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if stringValue(message["role"]) != "system" {
			continue
		}
		content, ok := message["content"].(string)
		if !ok {
			continue
		}
		for _, pair := range harnessPromptReplacements {
			content = strings.ReplaceAll(content, pair[0], pair[1])
		}
		message["content"] = content
	}
}

// rewriteUpstreamChunk strips response-side camouflage from one SSE data
// payload. [DONE] and unparseable payloads pass through unchanged.
func rewriteUpstreamChunk(data []byte) []byte {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("[DONE]")) {
		return data
	}
	var payload map[string]any
	if err := json.Unmarshal(trimmed, &payload); err != nil {
		return data
	}
	stripCamouflageFromPayload(payload)
	rewritten, err := json.Marshal(payload)
	if err != nil {
		return data
	}
	return rewritten
}

// consumeOpenAIStream reads an upstream SSE body and invokes onData for every
// data payload. It returns the number of data payloads seen.
func consumeOpenAIStream(body io.Reader, onData func(payload []byte) error) (int, error) {
	reader := bufio.NewReader(body)
	count := 0
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			trimmed := bytes.TrimSpace(line)
			if len(trimmed) > 0 && !bytes.HasPrefix(trimmed, []byte(":")) && bytes.HasPrefix(trimmed, []byte("data:")) {
				payload := bytes.TrimSpace(trimmed[5:])
				if len(payload) > 0 {
					count++
					if onData != nil {
						if err := onData(payload); err != nil {
							return count, err
						}
					}
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				return count, nil
			}
			return count, err
		}
	}
}

type accumulatorToolCall struct {
	id        string
	name      string
	arguments strings.Builder
}

type accumulatorChoice struct {
	role         string
	content      strings.Builder
	reasoning    strings.Builder
	finishReason string
	toolOrder    []int
	toolCalls    map[int]*accumulatorToolCall
}

// chatCompletionAccumulator merges a streamed chat.completion.chunk sequence
// into a single non-streaming chat.completion object.
type chatCompletionAccumulator struct {
	id          string
	model       string
	created     any
	usage       map[string]any
	choiceOrder []int
	choices     map[int]*accumulatorChoice
}

func newChatCompletionAccumulator() *chatCompletionAccumulator {
	return &chatCompletionAccumulator{
		choices: make(map[int]*accumulatorChoice),
	}
}

func (a *chatCompletionAccumulator) choiceFor(index int) *accumulatorChoice {
	choice, ok := a.choices[index]
	if !ok {
		choice = &accumulatorChoice{toolCalls: make(map[int]*accumulatorToolCall)}
		a.choices[index] = choice
		a.choiceOrder = append(a.choiceOrder, index)
	}
	return choice
}

func (a *chatCompletionAccumulator) ingest(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("[DONE]")) {
		return nil
	}

	var chunk struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Created any    `json:"created"`
		Choices []struct {
			Index int `json:"index"`
			Delta struct {
				Role             string                 `json:"role"`
				Content          string                 `json:"content"`
				ReasoningContent string                 `json:"reasoning_content"`
				ToolCalls        []openAIStreamToolCall `json:"tool_calls"`
			} `json:"delta"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(trimmed, &chunk); err != nil {
		return fmt.Errorf("decode upstream stream chunk: %w", err)
	}

	if chunk.ID != "" {
		a.id = chunk.ID
	}
	if chunk.Model != "" {
		a.model = chunk.Model
	}
	if chunk.Created != nil {
		a.created = chunk.Created
	}
	if chunk.Usage != nil {
		a.usage = chunk.Usage
	}

	for _, rawChoice := range chunk.Choices {
		choice := a.choiceFor(rawChoice.Index)
		if rawChoice.Delta.Role != "" {
			choice.role = rawChoice.Delta.Role
		}
		if rawChoice.Delta.Content != "" {
			choice.content.WriteString(rawChoice.Delta.Content)
		}
		if rawChoice.Delta.ReasoningContent != "" {
			choice.reasoning.WriteString(rawChoice.Delta.ReasoningContent)
		}
		for _, toolCall := range rawChoice.Delta.ToolCalls {
			tool, ok := choice.toolCalls[toolCall.Index]
			if !ok {
				tool = &accumulatorToolCall{}
				choice.toolCalls[toolCall.Index] = tool
				choice.toolOrder = append(choice.toolOrder, toolCall.Index)
			}
			if toolCall.ID != "" {
				tool.id = toolCall.ID
			}
			if toolCall.Function.Name != "" {
				tool.name = toolCall.Function.Name
			}
			if toolCall.Function.Arguments != "" {
				tool.arguments.WriteString(toolCall.Function.Arguments)
			}
		}
		if rawChoice.FinishReason != "" {
			choice.finishReason = rawChoice.FinishReason
		}
	}

	return nil
}

func (a *chatCompletionAccumulator) finalize() map[string]any {
	final := map[string]any{
		"object":  "chat.completion",
		"choices": []any{},
	}
	if a.id != "" {
		final["id"] = a.id
	}
	if a.model != "" {
		final["model"] = a.model
	}
	if a.created != nil {
		final["created"] = a.created
	}
	if a.usage != nil {
		final["usage"] = a.usage
	}

	choices := make([]any, 0, len(a.choiceOrder))
	for _, index := range a.choiceOrder {
		choice := a.choices[index]

		message := map[string]any{
			"role":    "assistant",
			"content": choice.content.String(),
		}
		if choice.role != "" {
			message["role"] = choice.role
		}
		if choice.reasoning.Len() > 0 {
			message["reasoning_content"] = choice.reasoning.String()
		}
		if len(choice.toolOrder) > 0 {
			calls := make([]any, 0, len(choice.toolOrder))
			for _, toolIndex := range choice.toolOrder {
				tool := choice.toolCalls[toolIndex]
				calls = append(calls, map[string]any{
					"index": toolIndex,
					"id":    tool.id,
					"type":  "function",
					"function": map[string]any{
						"name":      tool.name,
						"arguments": tool.arguments.String(),
					},
				})
			}
			message["tool_calls"] = calls
		}

		finishReason := choice.finishReason
		if finishReason == "" {
			finishReason = "stop"
		}
		choices = append(choices, map[string]any{
			"index":         index,
			"message":       message,
			"finish_reason": finishReason,
		})
	}
	final["choices"] = choices

	return final
}

// isBlankUpstreamStream reports whether a stream carried no observable output
// deltas (content, reasoning, or tool calls).
func isBlankUpstreamStream(chunks int, acc *chatCompletionAccumulator) bool {
	if chunks <= 0 || acc == nil || len(acc.choiceOrder) == 0 {
		return true
	}
	for _, index := range acc.choiceOrder {
		choice := acc.choices[index]
		if choice.content.Len() > 0 || choice.reasoning.Len() > 0 || len(choice.toolOrder) > 0 {
			return false
		}
	}
	return true
}

// promoteBlankOpenAIContent moves reasoning text into content when a stream
// produced reasoning only, so clients that ignore reasoning still see output.
func promoteBlankOpenAIContent(final map[string]any) {
	choices, ok := final["choices"].([]any)
	if !ok || len(choices) == 0 {
		return
	}
	choice, ok := choices[0].(map[string]any)
	if !ok {
		return
	}
	message, ok := choice["message"].(map[string]any)
	if !ok {
		return
	}
	if content, _ := message["content"].(string); strings.TrimSpace(content) != "" {
		return
	}
	reasoning, _ := message["reasoning_content"].(string)
	if strings.TrimSpace(reasoning) == "" {
		return
	}
	message["content"] = reasoning
	delete(message, "reasoning_content")
}

// reassembleOpenAIStreamResponse consumes a forced-stream upstream body and
// builds the non-streaming chat.completion object a client asked for.
func reassembleOpenAIStreamResponse(body io.Reader, stripCamouflage bool) (map[string]any, error) {
	acc := newChatCompletionAccumulator()
	chunks, err := consumeOpenAIStream(body, func(payload []byte) error {
		return acc.ingest(payload)
	})
	if err != nil {
		return nil, err
	}
	if isBlankUpstreamStream(chunks, acc) {
		return nil, errBlankUpstreamStream
	}

	final := acc.finalize()
	if stripCamouflage {
		stripCamouflageFromPayload(final)
	}
	promoteBlankOpenAIContent(final)
	return final, nil
}

// writeOpenAIStreamingResponse relays the upstream SSE stream to a streaming
// client, optionally rewriting tool camouflage per chunk. Response headers are
// not committed until the first data payload arrives so a fully blank upstream
// stream can still be retried.
func writeOpenAIStreamingResponse(w http.ResponseWriter, resp *http.Response, stripCamouflage bool) error {
	flusher, _ := w.(http.Flusher)
	reader := bufio.NewReader(resp.Body)
	committed := false
	payloadCount := 0

	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			trimmed := bytes.TrimSpace(line)
			if len(trimmed) > 0 && !bytes.HasPrefix(trimmed, []byte(":")) && bytes.HasPrefix(trimmed, []byte("data:")) {
				payload := bytes.TrimSpace(trimmed[5:])
				if len(payload) > 0 {
					if !committed {
						w.Header().Set("Content-Type", "text/event-stream")
						w.Header().Set("Cache-Control", "no-cache")
						w.WriteHeader(resp.StatusCode)
						committed = true
					}
					payloadCount++

					out := line
					if stripCamouflage {
						out = append([]byte("data: "), rewriteUpstreamChunk(payload)...)
						out = append(out, '\n', '\n')
					}
					if _, writeErr := w.Write(out); writeErr != nil {
						return writeErr
					}
					if flusher != nil {
						flusher.Flush()
					}
				}
			}
		}

		if err != nil {
			if err == io.EOF {
				if payloadCount == 0 && !committed {
					return errBlankUpstreamStream
				}
				return nil
			}
			if payloadCount == 0 && !committed {
				return errBlankUpstreamStream
			}
			return err
		}
	}
}
