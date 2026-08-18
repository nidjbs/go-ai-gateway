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
