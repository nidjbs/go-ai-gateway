package guardrails

import (
	"encoding/json"
	"strings"
)

// Message 是聊天消息的内部表示，用于 guardrails 检测。
// 只包含检测所需的字段，不依赖外部 provider 的消息结构。
type Message struct {
	Role    string // "system" | "user" | "assistant" | "tool"
	Content string // 纯文本内容
}

// MessagesFromChatRequest 从完整的 Chat Completion 请求 JSON 中解析出 messages。
// 这是中间件使用的入口，因为请求体包含 model、messages、metadata 等字段。
func MessagesFromChatRequest(raw []byte) ([]Message, bool) {
	var req struct {
		Messages json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, false
	}
	return MessagesFromOpenAI(req.Messages)
}

// MessagesFromOpenAI 从 OpenAI 格式的消息数组 JSON 解析出 Message 列表。
// 如果解析失败，返回 false。
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
		// content 可能是字符串
		if err := json.Unmarshal(m.Content, &content); err == nil {
			out = append(out, Message{Role: m.Role, Content: content})
			continue
		}
		// content 可能是数组 [{type: "text", text: "..."}]
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
		// 无法解析，跳过
	}
	return out, true
}
