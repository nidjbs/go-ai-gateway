package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// runChat sends one single-turn chat to the gateway, streaming to stdout by
// default. Returns the process exit code.
func runChat(cfg *Config, alias, systemPrompt, userContent string, noStream bool) int {
	client := NewClient(cfg)
	ctx := context.Background()
	messages := make([]Message, 0, 2)
	if strings.TrimSpace(systemPrompt) != "" {
		messages = append(messages, Message{Role: "system", Content: systemPrompt})
	}
	messages = append(messages, Message{Role: "user", Content: userContent})
	if noStream {
		out, err := client.Chat(ctx, alias, messages)
		if err != nil {
			fmt.Fprintln(os.Stderr, "gw:", err)
			return 1
		}
		fmt.Println(out)
		return 0
	}
	if err := client.ChatStream(ctx, alias, messages, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "gw:", err)
		return 1
	}
	fmt.Println()
	return 0
}

// modelFlags binds both -m and --model and returns a resolver for the effective
// alias. Call the resolver after fs.Parse so the flag values are populated.
func modelFlags(fs *flag.FlagSet, cfg *Config) func() string {
	m := fs.String("m", "", "调用模型名(gateway 别名,可用 gw models 查看)")
	long := fs.String("model", "", "调用模型名(gateway 别名,可用 gw models 查看)")
	return func() string { return firstNonEmpty(*m, *long, cfg.DefaultAlias) }
}

// loadCLIConfig loads the CLI config, printing a failure and exiting on error.
func loadCLIConfig() (*Config, int) {
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "gw:", err)
		return nil, 1
	}
	return cfg, 0
}

// readInput reads from -f file, positional args, or stdin.
func readInput(fs *flag.FlagSet, file string) (string, error) {
	if file != "" && file != "-" {
		data, err := os.ReadFile(file)
		return strings.TrimSpace(string(data)), err
	}
	if fs.NArg() > 0 {
		return strings.Join(fs.Args(), " "), nil
	}
	data, err := io.ReadAll(os.Stdin)
	return strings.TrimSpace(string(data)), err
}

func cmdAsk(args []string) int {
	cfg, code := loadCLIConfig()
	if cfg == nil {
		return code
	}
	fs := flag.NewFlagSet("ask", flag.ContinueOnError)
	aliasOf := modelFlags(fs, cfg)
	promptShort := fs.String("p", "", "prompt name, file, or raw prompt text")
	promptLong := fs.String("prompt", "", "prompt name, file, or raw prompt text")
	noStream := fs.Bool("no-stream", false, "non-streaming output")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	alias := aliasOf()
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "gw: ask requires a question")
		return 2
	}
	system, err := systemPromptFor(firstNonEmpty(*promptShort, *promptLong), promptsDir())
	if err != nil {
		fmt.Fprintln(os.Stderr, "gw:", err)
		return 1
	}
	return runChat(cfg, alias, system, strings.Join(fs.Args(), " "), *noStream)
}

func cmdTrans(args []string) int {
	cfg, code := loadCLIConfig()
	if cfg == nil {
		return code
	}
	fs := flag.NewFlagSet("trans", flag.ContinueOnError)
	aliasOf := modelFlags(fs, cfg)
	to := fs.String("t", "", "target language (default: auto-detect, 中文↔English)")
	noStream := fs.Bool("no-stream", false, "non-streaming output")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	alias := aliasOf()
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "gw: trans requires text to translate")
		return 2
	}
	// no -t: auto-detect source; 中文→English, otherwise 简体中文
	lang := *to
	if lang == "" {
		lang = "根据输入语言自动互译:如果输入是中文则翻译成英文,否则翻译成简体中文,"
	} else {
		lang = "把用户输入翻译成" + lang + ","
	}
	system := builtinPrompt("trans", lang)
	return runChat(cfg, alias, system, strings.Join(fs.Args(), " "), *noStream)
}

func cmdSummarize(args []string) int {
	cfg, code := loadCLIConfig()
	if cfg == nil {
		return code
	}
	fs := flag.NewFlagSet("summarize", flag.ContinueOnError)
	aliasOf := modelFlags(fs, cfg)
	file := fs.String("f", "", "file to summarize (- = stdin)")
	noStream := fs.Bool("no-stream", false, "non-streaming output")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	alias := aliasOf()
	content, err := readInput(fs, *file)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gw:", err)
		return 1
	}
	if content == "" {
		fmt.Fprintln(os.Stderr, "gw: no input to summarize")
		return 2
	}
	return runChat(cfg, alias, builtinPrompt("summarize"), content, *noStream)
}

func cmdExplain(args []string) int {
	cfg, code := loadCLIConfig()
	if cfg == nil {
		return code
	}
	fs := flag.NewFlagSet("explain", flag.ContinueOnError)
	aliasOf := modelFlags(fs, cfg)
	file := fs.String("f", "", "file to explain (- = stdin)")
	noStream := fs.Bool("no-stream", false, "non-streaming output")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	alias := aliasOf()
	content, err := readInput(fs, *file)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gw:", err)
		return 1
	}
	if content == "" {
		fmt.Fprintln(os.Stderr, "gw: no input to explain")
		return 2
	}
	return runChat(cfg, alias, builtinPrompt("explain"), content, *noStream)
}

func cmdCommit(args []string) int {
	cfg, code := loadCLIConfig()
	if cfg == nil {
		return code
	}
	fs := flag.NewFlagSet("commit", flag.ContinueOnError)
	aliasOf := modelFlags(fs, cfg)
	file := fs.String("f", "", "diff file to use instead of `git diff`")
	noStream := fs.Bool("no-stream", false, "non-streaming output")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	alias := aliasOf()
	content, err := readInput(fs, *file)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gw:", err)
		return 1
	}
	if content == "" {
		content = gitDiff()
	}
	if content == "" {
		fmt.Fprintln(os.Stderr, "gw: no staged or unstaged changes to describe")
		return 2
	}
	return runChat(cfg, alias, builtinPrompt("commit"), content, *noStream)
}

// gitDiff returns the staged diff, falling back to the unstaged diff.
func gitDiff() string {
	for _, arg := range []string{"--cached", ""} {
		args := []string{"diff"}
		if arg != "" {
			args = append(args, arg)
		}
		out, err := exec.Command("git", args...).Output()
		if err == nil && len(out) > 0 {
			return strings.TrimSpace(string(out))
		}
	}
	return ""
}
