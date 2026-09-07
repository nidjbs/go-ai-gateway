package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Cross-session notes: explicit memory the user (or an agent, after TTY
// confirmation) writes. Unlike clipboard content, notes are meant to reach the
// agent model when it recalls them — see the recall_notes tool.

const (
	notesMaxEntries = 500
	notesMaxText    = 4096 // per-note byte cap
	notesFindLimit  = 5
)

// noteEntry is one note line of the JSONL file.
type noteEntry struct {
	Time string `json:"time"`
	Text string `json:"text"`
}

// noteItem is a note with its 1-based file line number.
type noteItem struct {
	line  int
	entry noteEntry
}

// notesPath returns <state>/notes.jsonl.
func notesPath() string {
	return filepath.Join(gwStateDir(), "notes.jsonl")
}

// readNotes parses the notes file into items in file order.
func readNotes() ([]noteItem, error) {
	data, err := os.ReadFile(notesPath())
	if err != nil {
		return nil, err
	}
	var items []noteItem
	for i, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var e noteEntry
		if json.Unmarshal([]byte(line), &e) == nil && e.Text != "" {
			items = append(items, noteItem{line: i + 1, entry: e})
		}
	}
	return items, nil
}

// notesAppend writes a note, trimming old ones past notesMaxEntries, and
// returns its (current) file line number.
func notesAppend(text string) (int, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return 0, fmt.Errorf("笔记内容为空")
	}
	if len(text) > notesMaxText {
		return 0, fmt.Errorf("笔记超过 %d 字节", notesMaxText)
	}
	items, _ := readNotes()
	if len(items) >= notesMaxEntries {
		notesKeep(notesMaxEntries - 1)
		items, _ = readNotes()
	}
	entry := noteEntry{Time: time.Now().Format(time.RFC3339), Text: text}
	data, _ := json.Marshal(entry)
	if err := os.MkdirAll(gwStateDir(), 0o700); err != nil {
		return 0, err
	}
	f, err := os.OpenFile(notesPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		f.Close()
		return 0, err
	}
	f.Close()
	return len(items) + 1, nil
}

// notesKeep keeps only the newest max lines of the notes file.
func notesKeep(max int) {
	data, err := os.ReadFile(notesPath())
	if err != nil {
		return
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) <= max {
		return
	}
	keep := lines[len(lines)-max:]
	_ = os.WriteFile(notesPath(), []byte(strings.Join(keep, "\n")+"\n"), 0o600)
}

// notesForget removes the note at 1-based file line and returns it.
func notesForget(line int) (noteEntry, error) {
	items, err := readNotes()
	if err != nil {
		if os.IsNotExist(err) {
			return noteEntry{}, fmt.Errorf("没有笔记(第 %d 行不存在)", line)
		}
		return noteEntry{}, err
	}
	var removed noteEntry
	var kept []string
	found := false
	for _, it := range items {
		if it.line == line && !found {
			removed = it.entry
			found = true
			continue
		}
		data, _ := json.Marshal(it.entry)
		kept = append(kept, string(data))
	}
	if !found {
		return noteEntry{}, fmt.Errorf("第 %d 行不存在", line)
	}
	if len(kept) == 0 {
		_ = os.Remove(notesPath())
	} else {
		_ = os.WriteFile(notesPath(), []byte(strings.Join(kept, "\n")+"\n"), 0o600)
	}
	return removed, nil
}

// noteScore ranks how related a note is to a query (same fuzzy matcher as the
// clipboard recall, so memory works without a model).
func noteScore(query, text string) float64 {
	return clipboardScore(query, text)
}

// findNotes returns the most similar notes to query by fuzzy score, newest
// first on ties, prefixed with their file line numbers.
func findNotes(query string, limit int) (string, error) {
	if limit <= 0 {
		limit = notesFindLimit
	}
	items, err := readNotes()
	if err != nil {
		if os.IsNotExist(err) {
			return "(还没有笔记)", nil
		}
		return "", err
	}
	if len(items) == 0 {
		return "(还没有笔记)", nil
	}
	type scored struct {
		item  noteItem
		score float64
	}
	all := make([]scored, 0, len(items))
	for _, it := range items {
		all = append(all, scored{item: it, score: noteScore(query, it.entry.Text)})
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
		fmt.Fprintf(&b, "#%d  %s\n%s\n\n", s.item.line, s.item.entry.Time, s.item.entry.Text)
	}
	if count == 0 {
		return "(没有匹配的笔记)", nil
	}
	return strings.TrimSpace(b.String()), nil
}

// remember stores a note after interactive confirmation. An agent-initiated
// write follows the same confirm policy as file writes: never mode auto-allows,
// otherwise a TTY yes is required and non-interactive runs are denied.
func (p *FilePolicy) remember(text string) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", fmt.Errorf("remember 缺少 text")
	}
	if !p.confirmNote(text) {
		return "", fmt.Errorf("笔记写入被拒绝(需要交互确认)")
	}
	line, err := notesAppend(text)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("已记住笔记 #%d", line), nil
}

// recallNotes returns notes matching query by fuzzy score, or the newest notes
// when the query is empty. Notes are meant to be model-visible, so this runs on
// the remote agent side by design.
func (p *FilePolicy) recallNotes(query string) (string, error) {
	if strings.TrimSpace(query) == "" {
		return listNotes()
	}
	return findNotes(query, 0)
}

// confirmNote prompts for approval of an agent-initiated note write, showing the
// note text. Reuses the policy's injected confirm / mode, like confirmAction.
func (p *FilePolicy) confirmNote(text string) bool {
	if p.confirm != nil {
		return p.confirm("remember:" + text)
	}
	if p.mode == "never" {
		return true
	}
	if !isTTY(os.Stdin) {
		return false
	}
	preview := text
	if len(preview) > 500 {
		preview = preview[:500] + "…"
	}
	fmt.Fprintf(os.Stderr, "gw: agent 想记住这条笔记? [y/N]\n%s\n> ", preview)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return false
	}
	line = strings.ToLower(strings.TrimSpace(line))
	return line == "y" || line == "yes"
}

var toolRememberNotes = ToolSpec{
	Type: "function",
	Function: ToolSpecFunction{
		Name:        "remember",
		Description: "Store an explicit short note (cross-session memory). Requires interactive confirmation before it is written. Use for facts you want available in future sessions, like the user's preferences or environment details.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"text": map[string]any{"type": "string", "description": "The note text to remember."},
			},
			"required": []string{"text"},
		},
	},
}

var toolRecallNotes = ToolSpec{
	Type: "function",
	Function: ToolSpecFunction{
		Name:        "recall_notes",
		Description: "Recall explicit notes the user (or a prior session) wrote, matched to a query. With no query returns the most recent notes. Note content is shared with you on purpose.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string", "description": "What to look for (optional)."},
			},
		},
	},
}

// memoryTools filters an agent tool list so recall_notes is only advertised when
// the store has notes; remember stays available (it still confirms before write).
func memoryTools(tools []ToolSpec) []ToolSpec {
	if recallNotesAvailable() {
		return tools
	}
	var out []ToolSpec
	for _, t := range tools {
		if t.Function.Name != "recall_notes" {
			out = append(out, t)
		}
	}
	return out
}

// recallNotesAvailable reports whether any notes exist to recall.
func recallNotesAvailable() bool {
	items, err := readNotes()
	return err == nil && len(items) > 0
}

// listNotes renders notes newest first with line numbers.
func listNotes() (string, error) {
	items, err := readNotes()
	if err != nil {
		if os.IsNotExist(err) {
			return "(还没有笔记)", nil
		}
		return "", err
	}
	var b strings.Builder
	for i := len(items) - 1; i >= 0; i-- {
		it := items[i]
		text := strings.ReplaceAll(it.entry.Text, "\n", " ")
		if len(text) > 120 {
			text = text[:120] + "…"
		}
		fmt.Fprintf(&b, "%3d  %s  %s\n", it.line, it.entry.Time, text)
	}
	return strings.TrimRight(b.String(), "\n"), nil
}
