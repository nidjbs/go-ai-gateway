package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
)

// distillPrompt instructs the model to turn a conversation transcript into a
// structured reusable command file (YAML frontmatter + system prompt body) that
// is saved under promptsDir().
const distillPrompt = `你是命令沉淀助手。用户提供了一段多轮对话记录(可能包含工具调用),请提炼成一个"可复用命令"文件。

输出格式(只输出这个文件本身,不要前缀/解释/代码块):
---
name: <英文/数字/._- 组成的命令名,如 weekly-report>
description: <一句话职责>
tools: [read_file, write_file]   # 仅当对话中实际使用了工具;否则留空 []
schedule: ""                     # 如 "0 9 * * 1" 或 "@every 24h";留空表示不自动执行
---

<系统提示正文:命令职责、处理输入的方式、执行步骤、工具使用时机(如用到)、产出格式>

要求:
1. 正文只描述可复用的职责/规则/流程,不要复述对话里的具体内容或数据。
2. tools 必须忠实反映对话中用到的工具,不要凭空添加。
3. 简洁、用中文,面向之后每次独立调用都能完成任务。`

var skillNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func cmdRepl(args []string) int {
	cfg, code := loadCLIConfig()
	if cfg == nil {
		return code
	}
	fs := flag.NewFlagSet("repl", flag.ContinueOnError)
	alias := modelFlags(fs, cfg)
	systemShort := fs.String("system", "", "initial system prompt (name, file, or raw text)")
	promptShort := fs.String("p", "", "alias for --system")
	promptLong := fs.String("prompt", "", "alias for --system")
	file := fs.String("f", "", "file whose content seeds the conversation")
	resume := fs.String("resume", "", "resume a previous session by id")
	noStream := fs.Bool("no-stream", false, "non-streaming output")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(os.Stderr, "gw: repl takes no positional args")
		return 2
	}
	system, err := systemPromptFor(firstNonEmpty(*systemShort, *promptShort, *promptLong), promptsDir())
	if err != nil {
		fmt.Fprintln(os.Stderr, "gw:", err)
		return 1
	}
	seed := ""
	if *file != "" {
		data, err := os.ReadFile(*file)
		if err != nil {
			fmt.Fprintln(os.Stderr, "gw:", err)
			return 1
		}
		seed = strings.TrimSpace(string(data))
	}
	var sess *Session
	if *resume != "" {
		sess, err = loadSession(sessionsDir(), *resume)
		if err != nil {
			fmt.Fprintf(os.Stderr, "gw: 无法恢复会话 %q: %v\n", *resume, err)
			return 1
		}
	} else {
		sess, err = StartSession(sessionsDir())
		if err != nil {
			fmt.Fprintln(os.Stderr, "gw: sessionlog:", err)
			return 1
		}
	}
	return replLoop(cfg, alias, system, seed, *noStream, os.Stdin, sess)
}

// replLoop runs the interactive session event-sourced from sess; in is
// parameterized for tests. The loop is agentic: every user line may trigger
// file tool calls, and the model context is projected from the session log.
// Assistant text streams to stdout unless noStream is set.
func replLoop(cfg *Config, alias, system, seed string, noStream bool, in io.Reader, sess *Session) int {
	policy, err := newFilePolicy(cfg.FileRoots, cfg.WriteConfirm)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gw:", err)
		return 1
	}
	defer sess.Close()
	window := 20
	if cfg.ContextWindow != nil {
		window = *cfg.ContextWindow
	}
	trigger := 20
	if cfg.ContextTrigger != nil {
		trigger = *cfg.ContextTrigger
	}
	fresh := len(sess.events) == 0
	if fresh {
		emit(sess, SessionEvent{Type: evSessionStarted, Model: alias})
		if strings.TrimSpace(system) != "" {
			emit(sess, SessionEvent{Type: evSystemContext, Role: "system", Content: system})
		}
		if seed != "" {
			emit(sess, SessionEvent{Type: evUserMessage, Role: "user", Content: seed})
		}
		fmt.Fprintln(os.Stderr, "多轮 agent 会话已开始(可读写文件)。/exit 退出,/save <name> 沉淀为可复用命令。")
	} else {
		fmt.Fprintf(os.Stderr, "已恢复会话 %s。/exit 退出,/save <name> 沉淀为可复用命令。\n", sess.ID)
	}
	endSession := func() { emit(sess, SessionEvent{Type: evSessionEnded, Model: alias}) }

	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for {
		fmt.Fprint(os.Stderr, "gw> ")
		if !sc.Scan() {
			break // EOF (Ctrl-D) or read error
		}
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		switch {
		case line == "/exit" || line == "exit" || line == "quit":
			endSession()
			return 0
		case strings.HasPrefix(line, "/save"):
			if err := saveSession(cfg, alias, strings.TrimSpace(strings.TrimPrefix(line, "/save")), sess.FullTranscript()); err != nil {
				fmt.Fprintln(os.Stderr, "gw:", err)
			}
		default:
			emit(sess, SessionEvent{Type: evUserMessage, Role: "user", Content: line})
			var out io.Writer
			if !noStream {
				out = os.Stdout
			}
			_, reply, err := agentReply(cfg, alias, sess.Messages(), policy, sess, agentTools(), out)
			if err != nil {
				fmt.Fprintln(os.Stderr, "gw:", err)
				continue
			}
			maybeCompact(sess, window, trigger)
			if reply != "" {
				if noStream {
					fmt.Println(reply)
				} else {
					fmt.Println() // newline after streamed content
				}
			}
		}
	}
	endSession()
	if err := sc.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "gw:", err)
		return 1
	}
	return 0
}

// saveSession distills the conversation into a structured reusable command and
// persists it as promptsDir()/<name>.md (frontmatter + system prompt body).
func saveSession(cfg *Config, alias, name string, history []Message) error {
	if !skillNameRe.MatchString(name) {
		return fmt.Errorf("/save 需要合法的命令名(字母/数字/._- 组成,如 /save weekly-report)")
	}
	if !hasAssistantReply(history) {
		return fmt.Errorf("还没有可沉淀的对话(至少需要一轮完整的问答)")
	}
	msgs := []Message{
		{Role: "system", Content: distillPrompt},
		{Role: "user", Content: renderTranscript(history)},
	}
	out, err := NewClient(cfg).Chat(context.Background(), alias, msgs)
	if err != nil {
		return err
	}
	cmd, err := parseCommand([]byte(out))
	if err != nil {
		return err
	}
	if cmd.Body == "" {
		return fmt.Errorf("沉淀结果为空")
	}
	dir := promptsDir()
	if err := writeCommand(dir, name, cmd); err != nil {
		return err
	}
	fmt.Printf("saved command → %s\n", commandPath(dir, name))
	if len(cmd.Tools) > 0 {
		fmt.Printf("复用: gw run %s \"输入\"\n", name)
	} else {
		fmt.Printf("复用: gw ask --prompt %s \"输入\"\n", name)
	}
	return nil
}

func hasAssistantReply(history []Message) bool {
	for _, m := range history {
		if m.Role == "assistant" && strings.TrimSpace(m.Content) != "" {
			return true
		}
	}
	return false
}

// renderTranscript serializes the history for the distillation request: tool
// calls render as their own lines so the distiller can decide which tools to
// declare, and tool results are labeled to keep the transcript readable.
func renderTranscript(history []Message) string {
	var b strings.Builder
	for _, m := range history {
		label := "用户"
		switch m.Role {
		case "system":
			label = "系统指令"
		case "assistant":
			if m.Content != "" {
				label = "助手"
			} else if len(m.ToolCalls) > 0 {
				label = "工具调用"
			} else {
				continue
			}
		case "tool":
			label = "工具结果"
		}
		if label == "工具调用" {
			for _, tc := range m.ToolCalls {
				b.WriteString("[工具调用] " + tc.Function.Name + " " + toolArgPath(tc.Function.Arguments) + "\n\n")
			}
			continue
		}
		b.WriteString("[" + label + "] " + m.Content + "\n\n")
	}
	return strings.TrimSpace(b.String())
}
