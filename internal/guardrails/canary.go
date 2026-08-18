package guardrails

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
)

const (
	// canaryPrefix marks every canary token so it can be located quickly in responses.
	canaryPrefix = "GW_CANARY_"
	canaryTag    = "<!-- "
	canarySuffix = " -->"
)

// CanaryToken represents a tracking token injected into a request to detect system-prompt leakage.
type CanaryToken struct {
	Token     string // full token, e.g. GW_CANARY_a1b2c3d4
	Hidden    string // HTML-comment form, e.g. <!-- GW_CANARY_a1b2c3d4 -->
	FieldName string // injection field name, e.g. system_canary
}

// NewCanaryToken generates a new canary token.
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

// CheckCanary checks whether the response contains the canary token. A hit
// means the system prompt was echoed by the model and has leaked.
func CheckCanary(response string, token CanaryToken) bool {
	if token.Token == "" {
		return false
	}
	if !strings.Contains(response, canaryPrefix) {
		return false
	}
	return strings.Contains(response, token.Token) ||
		strings.Contains(response, token.Hidden)
}

// InjectIntoMessages embeds a canary token into the hidden system-message
// field. If a system message exists, the token is appended; otherwise a
// hidden system message is appended.
func InjectIntoMessages(messages []Message) ([]Message, CanaryToken) {
	canary := NewCanaryToken()

	for i, msg := range messages {
		if msg.Role == "system" {
			content := msg.Content
			if !strings.HasSuffix(content, canarySuffix) {
				messages[i].Content = content + "\n" + canary.Hidden
			}
			return messages, canary
		}
	}

	hidden := Message{
		Role:    "system",
		Content: "<!-- internal -->\n" + canary.Hidden,
	}
	messages = append(messages, hidden)
	return messages, canary
}
