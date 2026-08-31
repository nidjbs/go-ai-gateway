package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

// cmdRun executes a saved command agentically: the command body is the system
// prompt and declared tools (if any) are available to the model.
func cmdRun(args []string) int {
	cfg, code := loadCLIConfig()
	if cfg == nil {
		return code
	}
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	alias := modelFlags(fs, cfg)
	file := fs.String("f", "", "read input from file (- = stdin)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "gw: usage: gw run <command> [input...]")
		return 2
	}
	name := fs.Arg(0)
	cmd, err := loadSavedCommand(name)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gw:", err)
		return 1
	}
	userContent := ""
	if fs.NArg() > 1 {
		userContent = strings.Join(fs.Args()[1:], " ")
	}
	if *file != "" {
		content, err := readRunInput(*file)
		if err != nil {
			fmt.Fprintln(os.Stderr, "gw:", err)
			return 1
		}
		userContent = content
	}
	return runCommand(cfg, alias, cmd, userContent)
}

// loadSavedCommand loads a persisted command by name (or a direct file path).
func loadSavedCommand(name string) (*Command, error) {
	if strings.Contains(name, string(os.PathSeparator)) {
		data, err := os.ReadFile(name)
		if err != nil {
			return nil, err
		}
		cmd, err := parseCommand(data)
		if err != nil {
			return nil, err
		}
		return cmd, nil
	}
	data, err := os.ReadFile(commandPath(promptsDir(), name))
	if err != nil {
		return nil, fmt.Errorf("命令 %q 不存在(先用 /save 沉淀,或传入文件路径)", name)
	}
	cmd, err := parseCommand(data)
	if err != nil {
		return nil, err
	}
	if cmd.Name == "" {
		cmd.Name = name
	}
	return cmd, nil
}

// readRunInput reads a -f file or stdin for gw run.
func readRunInput(file string) (string, error) {
	if file != "-" {
		data, err := os.ReadFile(file)
		return strings.TrimSpace(string(data)), err
	}
	data, err := io.ReadAll(os.Stdin)
	return strings.TrimSpace(string(data)), err
}

// runCommand runs one agentic command turn, logging a one-shot session. It is
// shared by gw run and the scheduler daemon.
func runCommand(cfg *Config, alias string, cmd *Command, userContent string) int {
	policy, err := newFilePolicy(cfg.FileRoots, cfg.WriteConfirm)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gw:", err)
		return 1
	}
	log, err := StartSession(sessionsDir())
	if err != nil {
		fmt.Fprintln(os.Stderr, "gw: sessionlog:", err)
		return 1
	}
	defer log.Close()
	emit(log, SessionEvent{Type: evSessionStarted, Model: alias})
	history := make([]Message, 0, 2)
	if cmd.Body != "" {
		history = append(history, Message{Role: "system", Content: cmd.Body})
	}
	if userContent != "" {
		history = append(history, Message{Role: "user", Content: userContent})
		emit(log, SessionEvent{Type: evUserMessage, Content: userContent})
	}
	_, reply, err := agentReply(cfg, alias, history, policy, log, selectTools(cmd.Tools))
	emit(log, SessionEvent{Type: evSessionEnded, Model: alias})
	if err != nil {
		fmt.Fprintln(os.Stderr, "gw:", err)
		return 1
	}
	if reply != "" {
		fmt.Println(reply)
	}
	return 0
}
