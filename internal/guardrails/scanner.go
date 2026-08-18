package guardrails

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

var injectionPatterns = []*regexp.Regexp{
	// Category 1: Override / ignore instructions
	regexp.MustCompile(`(?i)ignore\s+(all\s+)?(previous|above|prior|earlier)\s+(instructions?|prompts?|rules?|commands?|guidelines?)`),
	regexp.MustCompile(`(?i)disregard\s+(all\s+)?(previous|above|prior|earlier)\s+(instructions?|prompts?|rules?|commands?)`),
	regexp.MustCompile(`(?i)forget\s+(everything|all|what\s+you\s+were\s+told|your\s+(instructions?|prompts?|rules?))`),
	regexp.MustCompile(`(?i)do\s+not\s+(follow|obey|adhere\s+to|consider)\s+(the\s+)?(previous|above|prior|system)\s+(instructions?|prompts?|rules?)`),
	regexp.MustCompile(`(?i)stop\s+(following|obeying|using)\s+(your\s+)?(previous|original|initial)\s+(instructions?|prompts?|rules?)`),
	regexp.MustCompile(`(?i)override\s+(all\s+)?(previous|prior|existing)\s+(instructions?|prompts?|rules?|constraints?)`),
	regexp.MustCompile(`(?i)bypass\s+(all\s+)?(safety|security|content|filter|restriction)?\s*(rules?|policies?|measures?)`),
	regexp.MustCompile(`(?i)skip\s+(all\s+)?(previous|above|prior)\s+(instructions?|steps?|rules?)`),

	// Category 2: Role override
	regexp.MustCompile(`(?i)you\s+are\s+now\s+(a|an|the)\s+\w+`),
	regexp.MustCompile(`(?i)from\s+now\s+on\s+you\s+(are|will|should|must)\b`),
	regexp.MustCompile(`(?i)pretend\s+(to\s+be|you\s+are|that\s+you\s+are)\b`),
	regexp.MustCompile(`(?i)act\s+as\s+(if\s+)?(you\s+are|a|an|the)\b`),
	regexp.MustCompile(`(?i)roleplay\s+as\s+(a|an|the)\b`),
	regexp.MustCompile(`(?i)assume\s+(the\s+)?(role|persona|identity|character)\s+of\b`),
	regexp.MustCompile(`(?i)new\s+(persona|role|identity|character)\s*[:：]`),

	// Category 3: New instructions injection
	regexp.MustCompile(`(?i)new\s+(instructions?|prompts?|rules?|commands?|guidelines?)\s*[:：]`),
	regexp.MustCompile(`(?i)updated?\s+(instructions?|prompts?|rules?|commands?)\s*[:：]`),
	regexp.MustCompile(`(?i)here\s+(is|are)\s+(the\s+)?(new|updated|real|actual)\s+(instructions?|prompts?|rules?)`),
	regexp.MustCompile(`(?i)instead\s+of\s+(the\s+)?(previous|original|above)\s*,?\s*(do|say|respond|act)`),
	regexp.MustCompile(`(?i)replace\s+(all\s+)?(previous|prior|existing)\s+(instructions?|prompts?)\s+with`),

	// Category 4: System prompt leakage
	regexp.MustCompile(`(?i)(reveal|output|print|repeat|show|leak|disclose|share|transcribe|recite|copy|dump|spit\s+out)\s+(me\s+)?(your|the)\s+(system|initial|original|full|complete|hidden)\s+(prompt|instructions?|message|context)`),
	regexp.MustCompile(`(?i)what\s+(are|were)\s+(your|the)\s+(system|initial|original|full)\s+(prompt|instructions?|message|context)`),
	regexp.MustCompile(`(?i)(your|the)\s+(system|initial|original)\s+(prompt|instructions?|message|context)\s+(is|was|please)`),

	// Category 5: Jailbreak / DAN / unrestricted mode
	regexp.MustCompile(`(?i)\bDAN\b(\s*(mode|prompt|jailbreak))?`),
	regexp.MustCompile(`(?i)developer\s*mode`),
	regexp.MustCompile(`(?i)\bjailbreak\b`),
	regexp.MustCompile(`(?i)no\s+(restrictions?|limits?|rules?|boundaries?|constraints?)`),
	regexp.MustCompile(`(?i)unrestricted\s+mode`),
	regexp.MustCompile(`(?i)uncensored\s+mode`),
	regexp.MustCompile(`(?i)unfiltered\s+mode`),
	regexp.MustCompile(`(?i)do\s+anything\s+now`),
	regexp.MustCompile(`(?i)without\s+(any\s+)?(restrictions?|limits?|rules?|boundaries?|constraints?|filters?)`),

	// Category 6: Encoding / obfuscation
	regexp.MustCompile(`(?i)base64\s+(encode|decode)`),
	regexp.MustCompile(`(?i)\brot13\b`),
	regexp.MustCompile(`(?i)decode\s+(this|the\s+following|below)`),
	regexp.MustCompile(`(?i)treat\s+(this|the\s+following|everything\s+after)\s+as\s+(a\s+)?(new\s+)?(instructions?|commands?|prompts?)`),
	regexp.MustCompile(`(?i)the\s+following\s+is\s+(a|an)\s+(safe|harmless|benign)\s+(instructions?|commands?)`),

	// Category 7: Delimiter spoofing
	regexp.MustCompile(`(?i)---+\s*(system|instructions?|prompts?)\s*---+`),
	regexp.MustCompile(`(?i)###+\s*(system|instructions?|prompts?)\s*###+`),
	regexp.MustCompile(`(?i)\[system\s*(prompt|instructions?|message)\]`),
	regexp.MustCompile(`(?i)\{\{\s*(system|instructions?|prompts?)\s*\}\}`),
	regexp.MustCompile(`(?i)"role"\s*:\s*"system"`),
	regexp.MustCompile(`(?i)human\s*:\s*(please\s+)?(ignore|forget|disregard)`),
	regexp.MustCompile(`(?i)assistant\s*:\s*(I\s+will\s+now|from\s+now\s+on)`),

	// Category 8: Chinese - override / ignore
	regexp.MustCompile(`忽略(之前|上面|先前|所有|一切)的(指令|提示|规则|要求|设定)`),
	regexp.MustCompile(`忘记(之前|上面|所有|一切)(的)?(指令|提示|规则|要求|设定)`),
	regexp.MustCompile(`不要(遵守|遵循|理会|考虑)(之前|上面|系统)(的)?(指令|提示|规则|要求)`),
	regexp.MustCompile(`跳过(之前|上面|所有)(的)?(指令|提示|规则|步骤)`),
	regexp.MustCompile(`无视(之前|上面|所有|任何)(的)?(指令|提示|规则|限制|约束)`),
	regexp.MustCompile(`覆盖(之前|上面|所有|原有)(的)?(指令|提示|规则|设定|配置)`),
	regexp.MustCompile(`绕过(所有|一切|系统)(的)?(安全|限制|规则|过滤|检测|防护)`),
	regexp.MustCompile(`解除(所有|一切)(的)?(限制|约束|规则|过滤)`),

	// Category 9: Chinese - role override
	regexp.MustCompile(`你现在(是|扮演|担任|变成了)`),
	regexp.MustCompile(`从现在开始你(是|扮演|担任|变成了)`),
	regexp.MustCompile(`请你扮演(一个|一名|一位)`),
	regexp.MustCompile(`假装你是(一个|一名|一位)`),
	regexp.MustCompile(`假设你是(一个|一名|一位)`),
	regexp.MustCompile(`你的新(身份|角色|任务是)`),
	regexp.MustCompile(`切换(到|为)(管理员|开发者|测试|调试|root|superuser)(模式|角色|身份)`),

	// Category 10: Chinese - new instructions injection
	regexp.MustCompile(`新的(指令|提示|规则|命令|要求)[：:]`),
	regexp.MustCompile(`更新(后的)?(指令|提示|规则|命令)[：:]`),
	regexp.MustCompile(`以下是(新的|真正的|实际的)(指令|提示|规则|命令)`),
	regexp.MustCompile(`用(以下|下面)(内容|指令|提示)(替代|替换|覆盖)`),

	// Category 11: Chinese - system prompt leakage
	regexp.MustCompile(`(输出|显示|重复|告诉|泄露|说出|复述|念出|copy|dump)你的(系统|初始|原始|完整|隐藏)(提示|指令|设定|配置|消息|上下文)`),
	regexp.MustCompile(`你的(系统|初始|原始)(提示|指令|设定)(是|是什么)`),

	// Category 12: Chinese - jailbreak
	regexp.MustCompile(`越狱`),
	regexp.MustCompile(`开发者模式`),
	regexp.MustCompile(`无限制模式`),
	regexp.MustCompile(`无限制(的)?回答`),
	regexp.MustCompile(`不受(任何)?(限制|约束|规则|管控)`),
	regexp.MustCompile(`没有(任何)?(限制|约束|规则|底线|过滤)`),
	regexp.MustCompile(`突破(所有)?(限制|约束|规则|过滤|安全)`),
	regexp.MustCompile(`开启(完全|最高|管理员|root)(权限|模式)`),
	regexp.MustCompile(`关闭(所有)?(安全|过滤|审核|防护|限制)`),

	// Category 13: Chinese - encoding / obfuscation
	regexp.MustCompile(`base64(编码|解码)`),
	regexp.MustCompile(`rot13(编码|解码)`),
	regexp.MustCompile(`解码(以下内容|下面的|这段)`),
	regexp.MustCompile(`(下面|以下)(内容|文字)(实际上|真实)(是|为)(指令|命令|提示)`),
	regexp.MustCompile(`这段(文字|内容|话)(是|代表|表示)(安全|无害|正常的)(指令|命令)`),

	// Category 14: Japanese
	regexp.MustCompile(`指示を無視`),
	regexp.MustCompile(`システムプロンプトを出力`),
	regexp.MustCompile(`役割を変更`),
	regexp.MustCompile(`以下の指示に従って`),
	regexp.MustCompile(`無制限モード`),

	// Category 15: Russian
	regexp.MustCompile(`игнорируй\s+(все\s+)?(предыдущие|вышеуказанные)\s+(инструкции|правила|подсказки)`),
	regexp.MustCompile(`выведи\s+(свой|системный)\s+(промпт|инструкции|подсказку)`),
	regexp.MustCompile(`забудь\s+(все\s+)?(инструкции|правила|подсказки)`),
	regexp.MustCompile(`ты\s+теперь`),
	regexp.MustCompile(`режим\s+разработчика`),
	regexp.MustCompile(`обход\s+(всех\s+)?(ограничений|правил|фильтров)`),
}

// Each matched rule contributes 0.25 to the score; 4+ matches trigger a block.
// This avoids false positives on normal inputs containing occasional keywords.
const scorePerRule = 0.25

// maxUserMessageLength is the character threshold for triggering the length bonus.
const maxUserMessageLength = 2000

// lengthScoreBonus is added once for messages exceeding maxUserMessageLength.
const lengthScoreBonus = 0.1

// ScanResult is the result of a single scan.
type ScanResult struct {
	Score   float64
	Matched []string
	Action  string
}

// Scanner is the injection-detection entry point.
type Scanner struct {
	patterns []*regexp.Regexp
}

// NewScanner creates a new Scanner with all patterns precompiled.
func NewScanner() *Scanner {
	return &Scanner{patterns: injectionPatterns}
}

// ScanMessages scans messages for injection intent. Only user and tool messages
// are scanned; threshold is the score (0.0–1.0) above which a block is triggered.
func (s *Scanner) ScanMessages(messages []Message, threshold float64) ScanResult {
	var matched []string
	score := 0.0

	for _, msg := range messages {
		if msg.Role != "user" && msg.Role != "tool" {
			continue
		}

		content := msg.Content
		for _, pattern := range s.patterns {
			if pattern.MatchString(content) {
				matched = append(matched, pattern.String())
				score += scorePerRule
			}
		}

		if msg.Role == "user" && utf8.RuneCountInString(content) > maxUserMessageLength {
			score += lengthScoreBonus
		}
	}

	if score > 1.0 {
		score = 1.0
	}

	action := "allow"
	if score >= threshold {
		action = "block"
	} else if score > 0 {
		action = "flag"
	}

	return ScanResult{
		Score:   score,
		Matched: matched,
		Action:  action,
	}
}

// ScanText scans arbitrary text for injection patterns.
func (s *Scanner) ScanText(text string, threshold float64) ScanResult {
	var matched []string
	score := 0.0

	for _, pattern := range s.patterns {
		if pattern.MatchString(text) {
			matched = append(matched, pattern.String())
			score += scorePerRule
		}
	}

	if score > 1.0 {
		score = 1.0
	}

	action := "allow"
	if score >= threshold {
		action = "block"
	} else if score > 0 {
		action = "flag"
	}

	return ScanResult{
		Score:   score,
		Matched: matched,
		Action:  action,
	}
}

// RuleCount returns the number of loaded rules.
func (s *Scanner) RuleCount() int {
	return len(s.patterns)
}

// extractText extracts plain text from a message content field, which may be
// a string or a rich-text array (e.g. [{"type":"text","text":"..."}]).
func extractText(v interface{}) string {
	if v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	case []interface{}:
		parts := make([]string, 0, len(t))
		for _, item := range t {
			if m, ok := item.(map[string]interface{}); ok {
				if text, ok := m["text"].(string); ok {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, " ")
	}
	return ""
}
