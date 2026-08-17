package guardrails

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
)

const (
	// canaryPrefix 是所有 canary token 的前缀，用于在响应中快速定位。
	// 使用 HTML 注释包裹后嵌入 system message 的隐藏字段，LLM 不会主动复述。
	canaryPrefix = "GW_CANARY_"
	canaryTag    = "<!-- "
	canarySuffix = " -->"
)

// CanaryToken 表示一个注入到请求中的追踪 token，用于检测系统提示泄露。
type CanaryToken struct {
	Token     string // 完整 token，如 GW_CANARY_a1b2c3d4
	Hidden    string // HTML 注释形式，如 <!-- GW_CANARY_a1b2c3d4 -->
	FieldName string // 注入到的字段名，如 system_canary
}

// NewCanaryToken 生成一个新的 canary token。
func NewCanaryToken() CanaryToken {
	b := make([]byte, 8)
	rand.Read(b)
	token := canaryPrefix + hex.EncodeToString(b)
	return CanaryToken{
		Token:     token,
		Hidden:    canaryTag + token + canarySuffix,
		FieldName: "system_canary",
	}
}

// CheckCanary 检查响应文本中是否包含 canary token（完整 token 或 HTML 注释形式均可）。
// 如果包含，说明 system prompt 被 LLM 原样输出，发生了泄露。
func CheckCanary(response string, token CanaryToken) bool {
	if token.Token == "" {
		return false
	}
	// 快速路径：先检查前缀，避免全量 strings.Contains
	if !strings.Contains(response, canaryPrefix) {
		return false
	}
	// 精确匹配：完整 token 或 HTML 注释形式
	return strings.Contains(response, token.Token) ||
		strings.Contains(response, token.Hidden)
}

// InjectIntoMessages 将 canary token 注入到 messages 的隐藏字段中。
// 如果存在 system message，在其 content 末尾追加 HTML 注释形式的 token；
// 如果不存在，在消息列表末尾追加一条隐藏的 system message。
// 返回注入后的 messages 和 canary token。
func InjectIntoMessages(messages []Message) ([]Message, CanaryToken) {
	canary := NewCanaryToken()

	// 尝试在现有 system message 中注入
	for i, msg := range messages {
		if msg.Role == "system" {
			content := msg.Content
			if !strings.HasSuffix(content, canarySuffix) {
				messages[i].Content = content + "\n" + canary.Hidden
			}
			return messages, canary
		}
	}

	// 没有 system message：追加一条隐藏的 system message
	hidden := Message{
		Role:    "system",
		Content: "<!-- internal -->\n" + canary.Hidden,
	}
	messages = append(messages, hidden)
	return messages, canary
}
