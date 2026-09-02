package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Clipboard watcher + history, with LOCAL-model semantic recall. Clipboard
// content is sensitive: the remote (agent) model never sees it — recall goes
// through a dedicated local-model alias configured as clipboard_local_alias.

const (
	clipboardPollInterval   = 2 * time.Second
	clipboardMaxEntries     = 500
	recallClipboardLimit    = 100 // entries handed to the local model at once
	recallClipboardEntryCap = 800 // per-entry chars sent to the local model
)

// clipboardEntry is one line of the clipboard history JSONL.
type clipboardEntry struct {
	Time string `json:"time"`
	Text string `json:"text"`
}

// clipboardItem is a history entry with its 1-based line number.
type clipboardItem struct {
	line  int
	entry clipboardEntry
}

// clipboardPath returns <state>/clipboard.jsonl.
func clipboardPath() string {
	return filepath.Join(gwStateDir(), "clipboard.jsonl")
}

// readClipboardItems parses the history file into items in file order.
func readClipboardItems() ([]clipboardItem, error) {
	data, err := os.ReadFile(clipboardPath())
	if err != nil {
		return nil, err
	}
	var items []clipboardItem
	for i, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var e clipboardEntry
		if json.Unmarshal([]byte(line), &e) == nil && e.Text != "" {
			items = append(items, clipboardItem{line: i + 1, entry: e})
		}
	}
	return items, nil
}

// captureClipboard appends the current pasteboard (if non-empty and changed
// from the last entry) to the history, keeping it bounded.
func captureClipboard() (bool, error) {
	out, err := exec.Command("pbpaste").Output()
	if err != nil {
		return false, nil // no pasteboard available / empty
	}
	text := strings.TrimSpace(string(out))
	if text == "" {
		return false, nil
	}
	last, _ := lastClipboardEntry()
	if last != nil && last.Text == text {
		return false, nil // unchanged
	}
	entry := clipboardEntry{Time: time.Now().Format(time.RFC3339), Text: text}
	data, _ := json.Marshal(entry)
	f, err := os.OpenFile(clipboardPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return false, err
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		f.Close()
		return false, err
	}
	f.Close()
	trimClipboardHistory()
	return true, nil
}

// lastClipboardEntry returns the most recent history entry, or nil.
func lastClipboardEntry() (*clipboardEntry, error) {
	f, err := os.Open(clipboardPath())
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var last *clipboardEntry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e clipboardEntry
		if json.Unmarshal([]byte(line), &e) == nil && e.Text != "" {
			last = &e
		}
	}
	return last, nil
}

// trimClipboardHistory keeps only the newest clipboardMaxEntries lines.
func trimClipboardHistory() {
	data, err := os.ReadFile(clipboardPath())
	if err != nil {
		return
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) <= clipboardMaxEntries {
		return
	}
	keep := lines[len(lines)-clipboardMaxEntries:]
	_ = os.WriteFile(clipboardPath(), []byte(strings.Join(keep, "\n")+"\n"), 0o600)
}

// leetNormalize maps common leet substitutions so "password" matches "P@ssw0rd".
func leetNormalize(s string) string {
	return strings.NewReplacer(
		"0", "o", "1", "i", "3", "e", "4", "a", "5", "s", "7", "t", "@", "a", "$", "s", "!", "i",
	).Replace(strings.ToLower(s))
}

func ngramSet(s string, n int) map[string]bool {
	out := make(map[string]bool)
	if s == "" {
		return out
	}
	if len(s) < n {
		out[s] = true
		return out
	}
	for i := 0; i+n <= len(s); i++ {
		out[s[i:i+n]] = true
	}
	return out
}

// ngramJaccard measures character n-gram overlap as a fuzzy similarity.
func ngramJaccard(a, b string) float64 {
	as, bs := ngramSet(a, 2), ngramSet(b, 2)
	if len(as) == 0 || len(bs) == 0 {
		return 0
	}
	inter := 0
	for g := range as {
		if bs[g] {
			inter++
		}
	}
	union := len(as) + len(bs) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// clipboardScore ranks how related an entry is to the query: exact substring,
// then leet-normalized substring (passwords/tokens), then fuzzy n-gram overlap.
func clipboardScore(query, text string) float64 {
	q := strings.ToLower(query)
	t := strings.ToLower(text)
	if strings.Contains(t, q) {
		return 1.0
	}
	nq, nt := leetNormalize(q), leetNormalize(t)
	if strings.Contains(nt, nq) {
		return 0.9
	}
	return ngramJaccard(nq, nt)
}

// findClipboard returns the full text of entries most similar to the query by
// fuzzy scoring. Purely local (no model); a quick substring / leet / fuzzy hit.
func findClipboard(query string, limit int) (string, error) {
	if limit <= 0 {
		limit = 5
	}
	items, err := readClipboardItems()
	if err != nil {
		if os.IsNotExist(err) {
			return "(剪贴板历史为空)", nil
		}
		return "", err
	}
	if len(items) == 0 {
		return "(剪贴板历史为空)", nil
	}
	type scored struct {
		item  clipboardItem
		score float64
	}
	all := make([]scored, 0, len(items))
	for _, it := range items {
		all = append(all, scored{item: it, score: clipboardScore(query, it.entry.Text)})
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].score != all[j].score {
			return all[i].score > all[j].score
		}
		return all[i].item.entry.Time > all[j].item.entry.Time // newest first
	})
	var b strings.Builder
	count := 0
	for _, s := range all {
		if s.score < 0.25 {
			continue
		}
		if count >= limit {
			break
		}
		count++
		fmt.Fprintf(&b, "=== %s ===\n%s\n\n", s.item.entry.Time, s.item.entry.Text)
	}
	if count == 0 {
		return "(没有匹配的剪贴板历史)", nil
	}
	return strings.TrimSpace(b.String()), nil
}

// recallClipboard does semantic recall via the configured LOCAL model alias, so
// clipboard content never reaches a remote provider. The local model identifies
// the best entry by number; the full original is then read from the history file.
func recallClipboard(cfg *Config, query string) (string, error) {
	alias := cfg.ClipboardLocalAlias
	if alias == "" {
		return "", fmt.Errorf("未配置 clipboard_local_alias(指向本地模型的别名, 用于剪贴板语义召回)")
	}
	items, err := readClipboardItems()
	if err != nil {
		if os.IsNotExist(err) {
			return "(剪贴板历史为空)", nil
		}
		return "", err
	}
	if len(items) == 0 {
		return "(剪贴板历史为空)", nil
	}
	var list strings.Builder
	start := max(len(items)-recallClipboardLimit, 0)
	for i := start; i < len(items); i++ {
		text := items[i].entry.Text
		if len(text) > recallClipboardEntryCap {
			text = text[:recallClipboardEntryCap] + "…"
		}
		fmt.Fprintf(&list, "#%d %s\n", items[i].line, text)
	}
	msgs := []Message{
		{Role: "system", Content: "你是本地剪贴板召回助手。用户给出一个描述，下面是从剪贴板历史抽出的条目(#编号 内容)。找出与描述语义最相关的一条，只输出它的编号(一个整数)，不要输出内容或解释。"},
		{Role: "user", Content: "描述: " + query + "\n\n" + list.String()},
	}
	out, err := NewClient(cfg).Chat(context.Background(), alias, msgs)
	if err != nil {
		return "", err
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil || n < 1 {
		return "", fmt.Errorf("本地模型未返回有效条目编号: %q", strings.TrimSpace(out))
	}
	for _, it := range items {
		if it.line == n {
			return it.entry.Text, nil
		}
	}
	return "", fmt.Errorf("本地模型返回的编号 %d 超出范围", n)
}

// copyToPasteboard writes text back to the system pasteboard via pbcopy.
func copyToPasteboard(text string) bool {
	cmd := exec.Command("pbcopy")
	cmd.Stdin = strings.NewReader(text)
	return cmd.Run() == nil
}

// clipboardStatePaths returns the watcher daemon's log and pid files.
func clipboardStatePaths() (logPath, pidPath string) {
	base := gwStateDir()
	return filepath.Join(base, "clipboard.log"), filepath.Join(base, "clipboard.pid")
}

// clipboardDaemon is the resident watcher loop started by `gw clipboard start`.
func clipboardDaemon() int {
	ticker := time.NewTicker(clipboardPollInterval)
	defer ticker.Stop()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	fmt.Println("clipboard watcher running")
	for {
		if _, err := captureClipboard(); err != nil {
			fmt.Fprintln(os.Stderr, "clipboard:", err)
		}
		select {
		case <-ticker.C:
		case <-sig:
			fmt.Println("clipboard watcher stopped")
			return 0
		}
	}
}

// startClipboardWatch launches the watcher in the background, writing pid + log.
func startClipboardWatch() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	logPath, pidPath := clipboardStatePaths()
	logF, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer logF.Close()
	cmd := exec.Command(exe, "clipboard", "watch")
	cmd.Stdout = logF
	cmd.Stderr = logF
	if err := cmd.Start(); err != nil {
		return err
	}
	return os.WriteFile(pidPath, []byte(strconv.Itoa(cmd.Process.Pid)), 0o600)
}
