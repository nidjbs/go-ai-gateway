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

// distillPrompt instructs the model to turn a conversation transcript (plus an
// optional user-stated goal) into a structured reusable command file that binds
// the goal and spells out an explicit execution flow.
const distillPrompt = `你是命令沉淀助手。用户提供了一段多轮对话记录(可能包含工具调用),以及可选的"目的/目标"。请提炼成一个"可复用命令"文件,使之后每次独立调用都能可靠完成任务。

输出格式(只输出这个文件本身,不要前缀/解释/代码块):
---
name: <英文/数字/._- 组成的命令名,沿用给定的名字>
description: <一句话职责,贴合本次给定的目的/目标>
tools: [read_file, write_file]   # 仅当对话中实际使用了工具;否则留空 []
schedule: ""                     # 如 "0 9 * * 1" 或 "@every 24h";留空表示不自动执行
---

<系统提示正文>

正文要求:
1. 若提供了"目的/目标",description 与正文必须围绕该目标撰写,不要自行扩大范围或泛化;未提供时按对话体现的实际职责概括。
2. 正文开头写一段编号的"整体执行流程":逐条描述每次独立调用时输入→处理→产出的步骤,以及何时调用工具。正文要能指导一次全新调用从零完成任务。
3. 只描述可复用的职责/规则/流程,不要复述对话里的具体内容或数据。
4. tools 必须忠实反映对话中用到的工具,不要凭空添加。
5. 简洁、用中文。`

var skillNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func cmdRepl(args []string) int {
	cfg, code := loadCLIConfig()
	if cfg == nil {
		return code
	}
	fs := flag.NewFlagSet("repl", flag.ContinueOnError)
	aliasOf := modelFlags(fs, cfg)
	systemShort := fs.String("system", "", "initial system prompt (name, file, or raw text)")
	promptShort := fs.String("p", "", "alias for --system")
	promptLong := fs.String("prompt", "", "alias for --system")
	file := fs.String("f", "", "file whose content seeds the conversation")
	resume := fs.String("resume", "", "resume a previous session by id")
	noStream := fs.Bool("no-stream", false, "non-streaming output")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	alias := aliasOf()
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
		systemPrompt := strings.TrimSpace(system)
		if systemPrompt == "" {
			systemPrompt = defaultAgentPrompt
		}
		emit(sess, SessionEvent{Type: evSystemContext, Role: "system", Content: systemPrompt})
		if rules := loadAgentRules(); rules != "" {
			emit(sess, SessionEvent{Type: evSystemContext, Role: "system", Content: rules})
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
		case line == "/exit" || line == "exit" || line == "quit" || line == "退出":
			endSession()
			return 0
		case strings.HasPrefix(line, "/compact"):
			before := len(sess.Messages())
			forceCompact(sess, window)
			after := len(sess.Messages())
			fmt.Fprintf(os.Stderr, "gw: 已压缩上下文 %d → %d 条\n", before, after)
		case strings.HasPrefix(line, "/clipboard"):
			// Clipboard content is sensitive: recall goes through the LOCAL model
			// only, never into the remote agent's context or the session log.
			q := strings.TrimSpace(strings.TrimSpace(strings.TrimPrefix(line, "/clipboard")))
			q = strings.TrimSpace(strings.TrimPrefix(q, "recall"))
			if q == "" {
				fmt.Fprintln(os.Stderr, "gw: 用法 /clipboard recall <描述> (本地模型召回,不经远端)")
				continue
			}
			text, err := recallClipboard(cfg, q)
			if err != nil {
				fmt.Fprintln(os.Stderr, "gw:", err)
				continue
			}
			fmt.Println(text)
			if copyToPasteboard(text) {
				fmt.Fprintln(os.Stderr, "gw: 已复制到剪贴板")
			}
		case strings.HasPrefix(line, "/remember"):
			// Explicit user note: no agent confirmation needed. Notes may later
			// reach the agent model via recall_notes, so /remember is deliberate.
			text := strings.TrimSpace(strings.TrimPrefix(line, "/remember"))
			if text == "" {
				fmt.Fprintln(os.Stderr, "gw: 用法 /remember <要记住的内容>")
				continue
			}
			num, err := notesAppend(text)
			if err != nil {
				fmt.Fprintln(os.Stderr, "gw:", err)
				continue
			}
			fmt.Printf("已记住 #%d: %s\n", num, strings.ReplaceAll(text, "\n", " "))
		case strings.HasPrefix(line, "/save"):
			if err := saveFlow(cfg, alias, sc, line, sess.FullTranscript()); err != nil {
				fmt.Fprintln(os.Stderr, "gw:", err)
			}
		default:
			emit(sess, SessionEvent{Type: evUserMessage, Role: "user", Content: line})
			var out io.Writer
			if !noStream {
				out = os.Stdout
			}
			_, reply, err := agentReply(cfg, alias, sess.Messages(), policy, sess, memoryTools(agentTools()), out)
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

// saveFlow handles /save: parses a name plus an optional one-line goal, distills
// a draft command anchored on that goal, previews it, and writes only after the
// user confirms. A plain text answer redrafts toward that new goal; n/empty
// aborts without writing.
func saveFlow(cfg *Config, alias string, sc *bufio.Scanner, line string, history []Message) error {
	name, goal := splitSaveNameGoal(strings.TrimPrefix(line, "/save"))
	if !skillNameRe.MatchString(name) {
		return fmt.Errorf("/save 需要合法的命令名(字母/数字/._- 组成,如 /save weekly-report)")
	}
	if goal == "" {
		goal = readSaveLine(sc, "gw: 用一句话说明该命令的作用与目标(回车=按对话概括): ")
	}
	for {
		cmd, err := distillCommand(cfg, alias, name, goal, history)
		if err != nil {
			return err
		}
		raw, err := cmd.marshal()
		if err != nil {
			return err
		}
		fmt.Printf("将保存为 %s,草稿如下:\n%s\n", commandPath(promptsDir(), name), raw)
		ans := readSaveLine(sc, "gw: 确认? [y]保存 [n]取消 [输入新目的句重新提炼]: ")
		switch strings.ToLower(ans) {
		case "y", "yes":
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
		case "", "n", "no", "cancel":
			fmt.Println("已取消,未写入。")
			return nil
		default:
			goal = strings.TrimSpace(ans) // redraft toward the new goal
		}
	}
}

// readSaveLine prints prompt to stderr and reads one trimmed reply line from sc.
// On EOF/read error it returns "".
func readSaveLine(sc *bufio.Scanner, prompt string) string {
	fmt.Fprint(os.Stderr, prompt)
	if sc.Scan() {
		return strings.TrimSpace(sc.Text())
	}
	return ""
}

// splitSaveNameGoal splits the text after "/save" into the command name (first
// word) and an optional goal (everything after it).
func splitSaveNameGoal(rest string) (name, goal string) {
	rest = strings.TrimSpace(rest)
	if i := strings.IndexAny(rest, " \t"); i >= 0 {
		return rest[:i], strings.TrimSpace(rest[i:])
	}
	return rest, ""
}

// distillCommand validates the request, then asks the model to turn the
// transcript plus an optional user-stated goal into a draft reusable command.
// It does not write anything; callers preview and confirm before persisting.
func distillCommand(cfg *Config, alias, name, goal string, history []Message) (*Command, error) {
	if !skillNameRe.MatchString(name) {
		return nil, fmt.Errorf("/save 需要合法的命令名(字母/数字/._- 组成,如 /save weekly-report)")
	}
	if !hasAssistantReply(history) {
		return nil, fmt.Errorf("还没有可沉淀的对话(至少需要一轮完整的问答)")
	}
	msgs := []Message{{Role: "system", Content: distillPrompt}}
	if goal = strings.TrimSpace(goal); goal != "" {
		msgs = append(msgs, Message{Role: "user", Content: "目的/目标: " + goal})
	}
	msgs = append(msgs, Message{Role: "user", Content: renderTranscript(history)})
	out, err := NewClient(cfg).Chat(context.Background(), alias, msgs)
	if err != nil {
		return nil, err
	}
	return parseCommandOutput(name, out)
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
