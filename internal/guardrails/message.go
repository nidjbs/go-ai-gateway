package guardrails

import (
	"encoding/json"
	"strings"
)

// Message is the internal chat-message representation used by guardrails.
type Message struct {
	Role    string // "system" | "user" | "assistant" | "tool"
	Content string // plain-text content
}

// MessagesFromChatRequest parses the messages array out of a raw Chat Completion request body.
func MessagesFromChatRequest(raw []byte) ([]Message, bool) {
	var req struct {
		Messages json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, false
	}
	return MessagesFromOpenAI(req.Messages)
}

// MessagesFromResponsesRequest parses user-controlled text out of a raw
// Responses API request body. input may be a plain string or an array of
// input items; only user/assistant message text and function_call_output
// (tool output) are surfaced to the scanner. Model-authored function_call
// items are skipped since their arguments are not an injection surface.
func MessagesFromResponsesRequest(raw []byte) ([]Message, bool) {
	var req struct {
		Input json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, false
	}

	// Plain string input.
	var text string
	if err := json.Unmarshal(req.Input, &text); err == nil {
		if strings.TrimSpace(text) == "" {
			return nil, true
		}
		return []Message{{Role: "user", Content: text}}, true
	}

	var items []struct {
		Type    string          `json:"type"`
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
		Output  string          `json:"output"`
	}
	if err := json.Unmarshal(req.Input, &items); err != nil {
		return nil, false
	}

	out := make([]Message, 0, len(items))
	for _, item := range items {
		switch item.Type {
		case "message":
			role := item.Role
			if role == "" {
				role = "user"
			}
			if content := responsesContent(item.Content); content != "" {
				out = append(out, Message{Role: role, Content: content})
			}
		case "function_call_output":
			if strings.TrimSpace(item.Output) != "" {
				out = append(out, Message{Role: "tool", Content: item.Output})
			}
		}
	}
	return out, true
}

// responsesContent extracts plain text from a Responses message content
// field, which may be a string or an array of typed content parts
// (input_text / output_text / text / refusal).
func responsesContent(raw json.RawMessage) string {
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	var sb strings.Builder
	for _, p := range parts {
		if p.Text != "" {
			sb.WriteString(p.Text)
			sb.WriteByte(' ')
		}
	}
	return strings.TrimSpace(sb.String())
}

// MessagesFromOpenAI parses messages from an OpenAI-format JSON messages array.
func MessagesFromOpenAI(raw []byte) ([]Message, bool) {
	var msgs []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &msgs); err != nil {
		return nil, false
	}

	out := make([]Message, 0, len(msgs))
	for _, m := range msgs {
		content := ""
		if err := json.Unmarshal(m.Content, &content); err == nil {
			out = append(out, Message{Role: m.Role, Content: content})
			continue
		}
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(m.Content, &parts); err == nil {
			for _, p := range parts {
				if p.Type == "text" {
					content += p.Text + " "
				}
			}
			out = append(out, Message{Role: m.Role, Content: strings.TrimSpace(content)})
			continue
		}
	}
	return out, true
}
