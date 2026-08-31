package main

import (
	"fmt"
)

// builtinPrompts maps a quick command to its system-prompt template. The "%s"
// placeholder is filled per invocation (e.g. the target language for trans).
var builtinPrompts = map[string]string{
	"trans":     "你是一个专业的翻译引擎。%s只输出译文,不要解释或额外说明。",
	"summarize": "你是一个专业的摘要助手。请用简洁的中文总结用户提供的内容,保留关键信息,输出为要点列表。",
	"explain":   "你是一个耐心的讲解者。请用中文清晰、结构化地解释用户提供的内容,必要时分步骤说明。",
	"commit":    "你是一个 Git 提交信息助手。请根据用户提供的 diff 生成一条简洁的 Conventional Commits 提交信息(subject 一行,必要时加 body),只输出提交信息本身,不要用代码块包裹。",
}

// builtinPrompt returns the rendered system prompt for a builtin command.
func builtinPrompt(name string, args ...any) string {
	tpl, ok := builtinPrompts[name]
	if !ok {
		return ""
	}
	return fmt.Sprintf(tpl, args...)
}

// systemPromptFor resolves a --prompt argument to a system prompt, reusing
// loadCommand so frontmatter metadata is stripped and only the body is used.
func systemPromptFor(arg, promptsDir string) (string, error) {
	if arg == "" {
		return "", nil
	}
	cmd, err := loadCommand(arg, promptsDir)
	if err != nil {
		return "", err
	}
	return cmd.Body, nil
}
