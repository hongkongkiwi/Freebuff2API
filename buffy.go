package main

import "strings"

// buffySystemPromptOpening is the byte-level prefix the free-tier backend
// requires on the first system message; requests without it are rejected with
// 403 free_mode_cli_required.
const buffySystemPromptOpening = "You are Buffy, the strategic coding assistant."

// ensureBuffySystemPrompt guarantees the upstream payload's first system
// message opens with the Buffy prompt. Clients without a system message get
// one prepended; clients whose system prompt opens differently get the
// opening line prepended to their existing content.
func ensureBuffySystemPrompt(payload map[string]any) {
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

		switch content := message["content"].(type) {
		case string:
			if strings.HasPrefix(content, buffySystemPromptOpening) {
				return
			}
			if strings.TrimSpace(content) == "" {
				message["content"] = buffySystemPromptOpening
			} else {
				message["content"] = buffySystemPromptOpening + "\n\n" + content
			}
			return
		case []any:
			if len(content) > 0 {
				if part, ok := content[0].(map[string]any); ok {
					if text, ok := part["text"].(string); ok && strings.HasPrefix(text, buffySystemPromptOpening) {
						return
					}
				}
			}
			prepended := make([]any, 0, len(content)+1)
			prepended = append(prepended, map[string]any{"type": "text", "text": buffySystemPromptOpening})
			prepended = append(prepended, content...)
			message["content"] = prepended
			return
		default:
			return
		}
	}

	// No system message at all: prepend one.
	systemMessage := map[string]any{"role": "system", "content": buffySystemPromptOpening}
	payload["messages"] = append([]any{systemMessage}, messages...)
}
