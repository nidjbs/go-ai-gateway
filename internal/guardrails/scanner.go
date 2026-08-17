package guardrails

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// ── injectionPatterns 是已知提示词注入攻击的正则规则库 ──
// 按类别分组，便于维护和审计。每条规则对应一种已知的注入模式。
//
// 评分策略：每条匹配的规则贡献 0.25 分，最高 1.0。
// 这意味着单条强规则不会直接触发 block，需要多条规则或强规则组合。

var injectionPatterns = []*regexp.Regexp{
	// ═══════════════════════════════════════════════════════
	// 类别 1：覆盖 / 忽略指令（最常见注入手法）
	// ═══════════════════════════════════════════════════════
	regexp.MustCompile(`(?i)ignore\s+(all\s+)?(previous|above|prior|earlier)\s+(instructions?|prompts?|rules?|commands?|guidelines?)`),
	regexp.MustCompile(`(?i)disregard\s+(all\s+)?(previous|above|prior|earlier)\s+(instructions?|prompts?|rules?|commands?)`),
	regexp.MustCompile(`(?i)forget\s+(everything|all|what\s+you\s+were\s+told|your\s+(instructions?|prompts?|rules?))`),
	regexp.MustCompile(`(?i)do\s+not\s+(follow|obey|adhere\s+to|consider)\s+(the\s+)?(previous|above|prior|system)\s+(instructions?|prompts?|rules?)`),
	regexp.MustCompile(`(?i)stop\s+(following|obeying|using)\s+(your\s+)?(previous|original|initial)\s+(instructions?|prompts?|rules?)`),
	regexp.MustCompile(`(?i)override\s+(all\s+)?(previous|prior|existing)\s+(instructions?|prompts?|rules?|constraints?)`),
	regexp.MustCompile(`(?i)bypass\s+(all\s+)?(safety|security|content|filter|restriction)?\s*(rules?|policies?|measures?)`),
	regexp.MustCompile(`(?i)skip\s+(all\s+)?(previous|above|prior)\s+(instructions?|steps?|rules?)`),

	// ═══════════════════════════════════════════════════════
	// 类别 2：角色覆盖（让 LLM 扮演无限制角色）
	// ═══════════════════════════════════════════════════════
	regexp.MustCompile(`(?i)you\s+are\s+now\s+(a|an|the)\s+\w+`),
	regexp.MustCompile(`(?i)from\s+now\s+on\s+you\s+(are|will|should|must)\b`),
	regexp.MustCompile(`(?i)pretend\s+(to\s+be|you\s+are|that\s+you\s+are)\b`),
	regexp.MustCompile(`(?i)act\s+as\s+(if\s+)?(you\s+are|a|an|the)\b`),
	regexp.MustCompile(`(?i)roleplay\s+as\s+(a|an|the)\b`),
	regexp.MustCompile(`(?i)assume\s+(the\s+)?(role|persona|identity|character)\s+of\b`),
	regexp.MustCompile(`(?i)new\s+(persona|role|identity|character)\s*[:：]`),

	// ═══════════════════════════════════════════════════════
	// 类别 3：新指令注入（直接替换系统指令）
	// ═══════════════════════════════════════════════════════
	regexp.MustCompile(`(?i)new\s+(instructions?|prompts?|rules?|commands?|guidelines?)\s*[:：]`),
	regexp.MustCompile(`(?i)updated?\s+(instructions?|prompts?|rules?|commands?)\s*[:：]`),
	regexp.MustCompile(`(?i)here\s+(is|are)\s+(the\s+)?(new|updated|real|actual)\s+(instructions?|prompts?|rules?)`),
	regexp.MustCompile(`(?i)instead\s+of\s+(the\s+)?(previous|original|above)\s*,?\s*(do|say|respond|act)`),
	regexp.MustCompile(`(?i)replace\s+(all\s+)?(previous|prior|existing)\s+(instructions?|prompts?)\s+with`),

	// ═══════════════════════════════════════════════════════
	// 类别 4：系统提示泄露（让 LLM 输出 system prompt）
	// ═══════════════════════════════════════════════════════
	regexp.MustCompile(`(?i)(reveal|output|print|repeat|show|leak|disclose|share|transcribe|recite|copy|dump|spit\s+out)\s+(me\s+)?(your|the)\s+(system|initial|original|full|complete|hidden)\s+(prompt|instructions?|message|context)`),
	regexp.MustCompile(`(?i)what\s+(are|were)\s+(your|the)\s+(system|initial|original|full)\s+(prompt|instructions?|message|context)`),
	regexp.MustCompile(`(?i)(your|the)\s+(system|initial|original)\s+(prompt|instructions?|message|context)\s+(is|was|please)`),

	// ═══════════════════════════════════════════════════════
	// 类别 5：Jailbreak / DAN / 无限制模式
	// ═══════════════════════════════════════════════════════
	regexp.MustCompile(`(?i)\bDAN\b(\s*(mode|prompt|jailbreak))?`),
	regexp.MustCompile(`(?i)developer\s*mode`),
	regexp.MustCompile(`(?i)\bjailbreak\b`),
	regexp.MustCompile(`(?i)no\s+(restrictions?|limits?|rules?|boundaries?|constraints?)`),
	regexp.MustCompile(`(?i)unrestricted\s+mode`),
	regexp.MustCompile(`(?i)uncensored\s+mode`),
	regexp.MustCompile(`(?i)unfiltered\s+mode`),
	regexp.MustCompile(`(?i)do\s+anything\s+now`),
	regexp.MustCompile(`(?i)without\s+(any\s+)?(restrictions?|limits?|rules?|boundaries?|constraints?|filters?)`),

	// ═══════════════════════════════════════════════════════
	// 类别 6：编码 / 混淆绕过
	// ═══════════════════════════════════════════════════════
	regexp.MustCompile(`(?i)base64\s+(encode|decode)`),
	regexp.MustCompile(`(?i)\brot13\b`),
	regexp.MustCompile(`(?i)decode\s+(this|the\s+following|below)`),
	regexp.MustCompile(`(?i)treat\s+(this|the\s+following|everything\s+after)\s+as\s+(a\s+)?(new\s+)?(instructions?|commands?|prompts?)`),
	regexp.MustCompile(`(?i)the\s+following\s+is\s+(a|an)\s+(safe|harmless|benign)\s+(instructions?|commands?)`),

	// ═══════════════════════════════════════════════════════
	// 类别 7：分隔符欺骗（伪造 system message 边界）
	// ═══════════════════════════════════════════════════════
	regexp.MustCompile(`(?i)---+\s*(system|instructions?|prompts?)\s*---+`),
	regexp.MustCompile(`(?i)###+\s*(system|instructions?|prompts?)\s*###+`),
	regexp.MustCompile(`(?i)\[system\s*(prompt|instructions?|message)\]`),
	regexp.MustCompile(`(?i)\{\{\s*(system|instructions?|prompts?)\s*\}\}`),
	regexp.MustCompile(`(?i)"role"\s*:\s*"system"`),
	regexp.MustCompile(`(?i)human\s*:\s*(please\s+)?(ignore|forget|disregard)`),
	regexp.MustCompile(`(?i)assistant\s*:\s*(I\s+will\s+now|from\s+now\s+on)`),

	// ═══════════════════════════════════════════════════════
	// 类别 8：中文 — 覆盖 / 忽略
	// ═══════════════════════════════════════════════════════
	regexp.MustCompile(`忽略(之前|上面|先前|所有|一切)的(指令|提示|规则|要求|设定)`),
	regexp.MustCompile(`忘记(之前|上面|所有|一切)(的)?(指令|提示|规则|要求|设定)`),
	regexp.MustCompile(`不要(遵守|遵循|理会|考虑)(之前|上面|系统)(的)?(指令|提示|规则|要求)`),
	regexp.MustCompile(`跳过(之前|上面|所有)(的)?(指令|提示|规则|步骤)`),
	regexp.MustCompile(`无视(之前|上面|所有|任何)(的)?(指令|提示|规则|限制|约束)`),
	regexp.MustCompile(`覆盖(之前|上面|所有|原有)(的)?(指令|提示|规则|设定|配置)`),
	regexp.MustCompile(`绕过(所有|一切|系统)(的)?(安全|限制|规则|过滤|检测|防护)`),
	regexp.MustCompile(`解除(所有|一切)(的)?(限制|约束|规则|过滤)`),

	// ═══════════════════════════════════════════════════════
	// 类别 9：中文 — 角色覆盖
	// ═══════════════════════════════════════════════════════
	regexp.MustCompile(`你现在(是|扮演|担任|变成了)`),
	regexp.MustCompile(`从现在开始你(是|扮演|担任|变成了)`),
	regexp.MustCompile(`请你扮演(一个|一名|一位)`),
	regexp.MustCompile(`假装你是(一个|一名|一位)`),
	regexp.MustCompile(`假设你是(一个|一名|一位)`),
	regexp.MustCompile(`你的新(身份|角色|任务是)`),
	regexp.MustCompile(`切换(到|为)(管理员|开发者|测试|调试|root|superuser)(模式|角色|身份)`),

	// ═══════════════════════════════════════════════════════
	// 类别 10：中文 — 新指令注入
	// ═══════════════════════════════════════════════════════
	regexp.MustCompile(`新的(指令|提示|规则|命令|要求)[：:]`),
	regexp.MustCompile(`更新(后的)?(指令|提示|规则|命令)[：:]`),
	regexp.MustCompile(`以下是(新的|真正的|实际的)(指令|提示|规则|命令)`),
	regexp.MustCompile(`用(以下|下面)(内容|指令|提示)(替代|替换|覆盖)`),

	// ═══════════════════════════════════════════════════════
	// 类别 11：中文 — 系统提示泄露
	// ═══════════════════════════════════════════════════════
	regexp.MustCompile(`(输出|显示|重复|告诉|泄露|说出|复述|念出|copy|dump)你的(系统|初始|原始|完整|隐藏)(提示|指令|设定|配置|消息|上下文)`),
	regexp.MustCompile(`你的(系统|初始|原始)(提示|指令|设定)(是|是什么)`),

	// ═══════════════════════════════════════════════════════
	// 类别 12：中文 — Jailbreak / 越狱
	// ═══════════════════════════════════════════════════════
	regexp.MustCompile(`越狱`),
	regexp.MustCompile(`开发者模式`),
	regexp.MustCompile(`无限制模式`),
	regexp.MustCompile(`无限制(的)?回答`),
	regexp.MustCompile(`不受(任何)?(限制|约束|规则|管控)`),
	regexp.MustCompile(`没有(任何)?(限制|约束|规则|底线|过滤)`),
	regexp.MustCompile(`突破(所有)?(限制|约束|规则|过滤|安全)`),
	regexp.MustCompile(`开启(完全|最高|管理员|root)(权限|模式)`),
	regexp.MustCompile(`关闭(所有)?(安全|过滤|审核|防护|限制)`),

	// ═══════════════════════════════════════════════════════
	// 类别 13：中文 — 编码 / 混淆
	// ═══════════════════════════════════════════════════════
	regexp.MustCompile(`base64(编码|解码)`),
	regexp.MustCompile(`rot13(编码|解码)`),
	regexp.MustCompile(`解码(以下内容|下面的|这段)`),
	regexp.MustCompile(`(下面|以下)(内容|文字)(实际上|真实)(是|为)(指令|命令|提示)`),
	regexp.MustCompile(`这段(文字|内容|话)(是|代表|表示)(安全|无害|正常的)(指令|命令)`),

	// ═══════════════════════════════════════════════════════
	// 类别 14：日语
	// ═══════════════════════════════════════════════════════
	regexp.MustCompile(`指示を無視`),
	regexp.MustCompile(`システムプロンプトを出力`),
	regexp.MustCompile(`役割を変更`),
	regexp.MustCompile(`以下の指示に従って`),
	regexp.MustCompile(`無制限モード`),

	// ═══════════════════════════════════════════════════════
	// 类别 15：俄语
	// ═══════════════════════════════════════════════════════
	regexp.MustCompile(`игнорируй\s+(все\s+)?(предыдущие|вышеуказанные)\s+(инструкции|правила|подсказки)`),
	regexp.MustCompile(`выведи\s+(свой|системный)\s+(промпт|инструкции|подсказку)`),
	regexp.MustCompile(`забудь\s+(все\s+)?(инструкции|правила|подсказки)`),
	regexp.MustCompile(`ты\s+теперь`),
	regexp.MustCompile(`режим\s+разработчика`),
	regexp.MustCompile(`обход\s+(всех\s+)?(ограничений|правил|фильтров)`),
}

// scorePerRule 是每条匹配规则贡献的分数。
// 设计原则：单条规则不足以触发 block（需要 ≥4 条规则同时命中），
// 避免误杀正常用户输入中偶尔出现的关键词。
const scorePerRule = 0.25

// maxUserMessageLength 是单条 user message 的异常长度阈值。
// 超过此长度的消息额外加分（可能是长文本注入 payload）。
const maxUserMessageLength = 2000

// lengthScoreBonus 是超长消息的额外加分。
const lengthScoreBonus = 0.1

// ScanResult 是一次扫描的结果。
type ScanResult struct {
	Score   float64  // 0.0 ~ 1.0，越高越可能是注入
	Matched []string // 匹配到的规则表达式（用于审计）
	Action  string   // "allow" | "flag" | "block"
}

// Scanner 是注入检测的入口。
type Scanner struct {
	patterns []*regexp.Regexp
}

// NewScanner 创建一个新的 Scanner，预编译所有正则规则。
func NewScanner() *Scanner {
	return &Scanner{patterns: injectionPatterns}
}

// ScanMessages 扫描消息列表中的注入意图。
// 只扫描 role=user 和 role=tool 的消息（system 是内部控制的，不需要扫描）。
// threshold 是触发 block 的分数阈值（0.0 ~ 1.0）。
func (s *Scanner) ScanMessages(messages []Message, threshold float64) ScanResult {
	var matched []string
	score := 0.0

	for _, msg := range messages {
		// 只扫描外部输入的消息
		if msg.Role != "user" && msg.Role != "tool" {
			continue
		}

		content := msg.Content

		// 正则匹配
		for _, pattern := range s.patterns {
			if pattern.MatchString(content) {
				matched = append(matched, pattern.String())
				score += scorePerRule
			}
		}

		// 启发式：单条 user message 异常长
		if msg.Role == "user" && utf8.RuneCountInString(content) > maxUserMessageLength {
			score += lengthScoreBonus
		}
	}

	// 分数上限 1.0
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

// ScanText 扫描任意文本（用于出站检测）。
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

// RuleCount 返回当前加载的规则总数（用于监控和测试）。
func (s *Scanner) RuleCount() int {
	return len(s.patterns)
}

// ── 辅助函数 ──

// extractText 从消息 content 字段提取纯文本。
// content 可能是字符串，也可能是富文本数组（如 [{type: "text", text: "..."}]）。
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
