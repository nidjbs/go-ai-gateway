package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// cmdClipboard manages the clipboard history watcher and the recall tool's data.
func cmdClipboard(args []string) int {
	if len(args) == 0 {
		return clipboardList()
	}
	switch args[0] {
	case "list":
		return clipboardList()
	case "find":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "gw: usage: gw clipboard find <query>")
			return 2
		}
		out, err := findClipboard(strings.Join(args[1:], " "), 5)
		if err != nil {
			fmt.Fprintln(os.Stderr, "gw:", err)
			return 1
		}
		fmt.Println(out)
		return 0
	case "recall":
		// Semantic recall via the LOCAL model alias only (never the remote
		// agent model), so clipboard content stays on this machine.
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "gw: usage: gw clipboard recall <描述>")
			return 2
		}
		cfg, code := loadCLIConfig()
		if cfg == nil {
			return code
		}
		text, err := recallClipboard(cfg, strings.Join(args[1:], " "))
		if err != nil {
			fmt.Fprintln(os.Stderr, "gw:", err)
			return 1
		}
		fmt.Println(text)
		if copyToPasteboard(text) {
			fmt.Println("(已复制到剪贴板)")
		}
		return 0
	case "start":
		return clipboardStart()
	case "stop":
		return clipboardStop()
	case "clear":
		return clipboardClear()
	case "watch":
		return clipboardDaemon()
	default:
		fmt.Fprintf(os.Stderr, "gw: unknown clipboard subcommand %q\n", args[0])
		return 2
	}
}

// clipboardList prints the newest entries (default 10) plus the watcher status.
func clipboardList() int {
	logPath, pidPath := clipboardStatePaths()
	status := "stopped"
	if pid, running := pidAlive(pidPath); running {
		status = fmt.Sprintf("running (pid %d)", pid)
	}
	fmt.Printf("clipboard watcher: %s (history: %s, log: %s)\n", status, clipboardPath(), logPath)
	f, err := os.Open(clipboardPath())
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Println("no clipboard history yet")
			return 0
		}
		fmt.Fprintln(os.Stderr, "gw:", err)
		return 1
	}
	defer f.Close()
	var entries []clipboardEntry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e clipboardEntry
		if json.Unmarshal([]byte(line), &e) == nil {
			entries = append(entries, e)
		}
	}
	start := len(entries) - 10
	if start < 0 {
		start = 0
	}
	for _, e := range entries[start:] {
		text := e.Text
		if len(text) > 80 {
			text = text[:80] + "…"
		}
		fmt.Printf("%s  %s\n", e.Time, strings.ReplaceAll(text, "\n", " "))
	}
	return 0
}

func clipboardStart() int {
	_, pidPath := clipboardStatePaths()
	if pid, running := pidAlive(pidPath); running {
		fmt.Fprintf(os.Stderr, "gw: clipboard watcher already running (pid %d)\n", pid)
		return 1
	}
	if err := startClipboardWatch(); err != nil {
		fmt.Fprintln(os.Stderr, "gw:", err)
		return 1
	}
	logPath, _ := clipboardStatePaths()
	pid, _ := pidAlive(pidPath)
	fmt.Printf("clipboard watcher started (pid %d, log: %s)\n", pid, logPath)
	return 0
}

func clipboardStop() int {
	_, pidPath := clipboardStatePaths()
	pid, running := pidAlive(pidPath)
	if !running {
		fmt.Println("no clipboard watcher running")
		return 0
	}
	if proc, err := os.FindProcess(pid); err == nil {
		if err := proc.Signal(os.Interrupt); err != nil {
			_ = proc.Kill()
		}
	}
	_ = os.Remove(pidPath)
	fmt.Printf("clipboard watcher stopped (pid %d)\n", pid)
	return 0
}

func clipboardClear() int {
	_ = os.Remove(clipboardPath())
	fmt.Println("clipboard history cleared")
	return 0
}
