package main

import (
	"bufio"
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// distillPrompt instructs the model to turn a conversation transcript into a
// reusable command (a system prompt) that is saved under promptsDir().
const distillPrompt = `你是工作流沉淀助手。用户提供了一段多轮对话记录,请提炼成一个"可复用命令"的系统提示。

要求:
1. 只输出系统提示本身,不要前缀、解释或代码块。
2. 描述命令的职责、如何处理用户输入、产出什么格式。
3. 抽象可复用的步骤/规则/方法论,不要复述对话里的具体内容或数据。
4. 简洁,用中文,面向之后每次独立调用都能完成任务。`

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
	return replLoop(cfg, alias, system, seed, *noStream, os.Stdin)
}

// replLoop runs the interactive session; in is parameterized for tests.
func replLoop(cfg *Config, alias, system, seed string, noStream bool, in io.Reader) int {
	history := make([]Message, 0, 8)
	if strings.TrimSpace(system) != "" {
		history = append(history, Message{Role: "system", Content: system})
	}
	if seed != "" {
		history = append(history, Message{Role: "user", Content: seed})
	}

	fmt.Fprintln(os.Stderr, "多轮会话已开始。/exit 退出,/save <name> 沉淀为可复用命令。")
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
			return 0
		case strings.HasPrefix(line, "/save"):
			if err := saveSession(cfg, alias, strings.TrimSpace(strings.TrimPrefix(line, "/save")), history); err != nil {
				fmt.Fprintln(os.Stderr, "gw:", err)
			}
		default:
			history = append(history, Message{Role: "user", Content: line})
			reply, err := replTurn(cfg, alias, history, noStream)
			if err != nil {
				fmt.Fprintln(os.Stderr, "gw:", err)
				continue
			}
			history = append(history, Message{Role: "assistant", Content: reply})
		}
	}
	if err := sc.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "gw:", err)
		return 1
	}
	return 0
}

// replTurn sends the full history and returns the assistant's full reply,
// streaming it to stdout unless noStream is set.
func replTurn(cfg *Config, alias string, history []Message, noStream bool) (string, error) {
	client := NewClient(cfg)
	ctx := context.Background()
	if noStream {
		out, err := client.Chat(ctx, alias, history)
		if err != nil {
			return "", err
		}
		fmt.Println(out)
		return out, nil
	}
	var buf bytes.Buffer
	if err := client.ChatStream(ctx, alias, history, io.MultiWriter(os.Stdout, &buf)); err != nil {
		return "", err
	}
	fmt.Println()
	return buf.String(), nil
}

// saveSession distills the conversation into a reusable command (a system
// prompt) and persists it as promptsDir()/<name>.md.
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
	out = strings.TrimSpace(out)
	if out == "" {
		return fmt.Errorf("沉淀结果为空")
	}
	dir := promptsDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, name+".md")
	if err := os.WriteFile(path, []byte(out), 0o600); err != nil {
		return err
	}
	fmt.Printf("saved command → %s\n", path)
	fmt.Printf("复用: gw ask --prompt %s \"输入\"\n", name)
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

// renderTranscript serializes the history for the distillation request.
func renderTranscript(history []Message) string {
	var b strings.Builder
	for _, m := range history {
		label := "用户"
		switch m.Role {
		case "system":
			label = "系统指令"
		case "assistant":
			label = "助手"
		}
		b.WriteString("[" + label + "] " + m.Content + "\n\n")
	}
	return strings.TrimSpace(b.String())
}
