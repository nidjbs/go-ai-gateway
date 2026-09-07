package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// toolCall builds a ToolCall from a name + JSON arguments.
func toolCall(name, args string) ToolCall {
	return ToolCall{Function: ToolFunction{Name: name, Arguments: args}}
}

// setNotesState points GW_STATE_DIR at a temp dir and returns a cleanup.
func setNotesState(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("GW_STATE_DIR", dir)
}

func writeNote(t *testing.T, text string) int {
	t.Helper()
	line, err := notesAppend(text)
	if err != nil {
		t.Fatalf("notesAppend(%q): %v", text, err)
	}
	return line
}

func TestNotesAppendListForget(t *testing.T) {
	setNotesState(t)
	_, err := notesAppend("")
	if err == nil {
		t.Fatal("空笔记应拒绝")
	}
	l1 := writeNote(t, "deploy host is 10.0.0.7 on port 3000")
	writeNote(t, "user prefers Chinese replies")
	if l1 != 1 {
		t.Fatalf("首个笔记行号 = %d, want 1", l1)
	}
	out, err := listNotes()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "deploy host") || !strings.Contains(out, "Chinese replies") {
		t.Fatalf("listNotes 缺内容:\n%s", out)
	}
	// fuzzy find: substring 命中
	found, err := findNotes("deploy host", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(found, "#1") || !strings.Contains(found, "10.0.0.7") {
		t.Fatalf("findNotes:\n%s", found)
	}
	// 不存在的内容 → 空
	if got, err := findNotes("zzz-no-such-topic", 0); err != nil || got != "(没有匹配的笔记)" {
		t.Fatalf("findNotes 无匹配 = %q, %v", got, err)
	}
	// forget 第 2 行
	removed, err := notesForget(2)
	if err != nil || removed.Text == "" {
		t.Fatalf("notesForget(2): %+v, %v", removed, err)
	}
	got, _ := readNotes()
	if len(got) != 1 || got[0].line != 1 || got[0].entry.Text != "deploy host is 10.0.0.7 on port 3000" {
		t.Fatalf("forget 后 = %+v", got)
	}
	// 重复 forget 同一行 → 报错
	if _, err := notesForget(2); err == nil {
		t.Fatal("越界 forget 应报错")
	}
	// 忘光后文件删除
	if _, err := notesForget(1); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(notesPath()); !os.IsNotExist(err) {
		t.Fatalf("清空后应删除文件, err = %v", err)
	}
}

func TestNotesCap(t *testing.T) {
	setNotesState(t)
	for i := 0; i < notesMaxEntries+10; i++ {
		if _, err := notesAppend("note-" + strconv.Itoa(i)); err != nil {
			t.Fatal(err)
		}
	}
	items, err := readNotes()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != notesMaxEntries {
		t.Fatalf("笔记数 = %d, want %d", len(items), notesMaxEntries)
	}
	if items[0].entry.Text != "note-10" {
		t.Fatalf("应裁掉最旧 10 条, 首条 = %q", items[0].entry.Text)
	}
}

func TestNotesOverlong(t *testing.T) {
	setNotesState(t)
	long := strings.Repeat("x", notesMaxText+1)
	if _, err := notesAppend(long); err == nil || !strings.Contains(err.Error(), "超过") {
		t.Fatalf("超长笔记应拒绝, got %v", err)
	}
}

func TestMemoryRememberRecallDispatch(t *testing.T) {
	setNotesState(t)
	// never 模式 + confirm 注入 → remember 直接写入
	p, err := newFilePolicy([]string{t.TempDir()}, "never")
	if err != nil {
		t.Fatal(err)
	}
	out, err := p.DispatchTool(toolCall("remember", `{"text":"deploy on port 3000"}`))
	if err != nil || !strings.Contains(out, "#1") {
		t.Fatalf("remember = %q, %v", out, err)
	}
	data, _ := os.ReadFile(notesPath())
	if !strings.Contains(string(data), "port 3000") {
		t.Fatalf("notes file = %s", data)
	}
	// recall_notes 空 query → 最近笔记
	out, err = p.DispatchTool(toolCall("recall_notes", `{}`))
	if err != nil || !strings.Contains(out, "port 3000") {
		t.Fatalf("recall_notes empty = %q, %v", out, err)
	}
	// recall_notes 带 query → 子串命中
	out, err = p.DispatchTool(toolCall("recall_notes", `{"query":"on port"}`))
	if err != nil || !strings.Contains(out, "port 3000") {
		t.Fatalf("recall_notes query = %q, %v", out, err)
	}
}

func TestMemoryRememberDeniedNonTTY(t *testing.T) {
	setNotesState(t)
	// auto 模式 + 无注入, 测试内 os.Stdin 非 TTY → 拒绝且不落盘
	p, err := newFilePolicy([]string{t.TempDir()}, "auto")
	if err != nil {
		t.Fatal(err)
	}
	out, err := p.DispatchTool(toolCall("remember", `{"text":"sneaky note"}`))
	if err == nil || !strings.Contains(err.Error(), "被拒绝") {
		t.Fatalf("非 TTY remember 应拒绝, out=%q err=%v", out, err)
	}
	if _, statErr := os.Stat(notesPath()); !os.IsNotExist(statErr) {
		t.Fatalf("被拒后不应写文件, stat=%v", statErr)
	}
}

func TestMemoryToolsGating(t *testing.T) {
	setNotesState(t)
	// 空笔记库 → 不广告 recall_notes, remember 仍在
	tools := memoryTools(agentTools())
	if containsTool(tools, "recall_notes") {
		t.Fatal("空库不应广告 recall_notes")
	}
	if !containsTool(tools, "remember") || !containsTool(tools, "read_file") {
		t.Fatal("remember/read_file 应始终可用")
	}
	// 有笔记后 → 广告 recall_notes
	writeNote(t, "something to recall later")
	if !containsTool(memoryTools(agentTools()), "recall_notes") {
		t.Fatal("有笔记后应广告 recall_notes")
	}
	// selectTools 保留显式声明的 memory 工具
	if sel := selectTools([]string{"remember", "recall_notes"}); len(sel) != 2 {
		t.Fatalf("selectTools memory = %d, want 2", len(sel))
	}
}

func containsTool(tools []ToolSpec, name string) bool {
	for _, t := range tools {
		if t.Function.Name == name {
			return true
		}
	}
	return false
}

func TestNotesCommandDispatch(t *testing.T) {
	setNotesState(t)
	if want := filepath.Join(os.Getenv("GW_STATE_DIR"), "notes.jsonl"); notesPath() != want {
		t.Fatalf("notesPath = %s, want %s", notesPath(), want)
	}
	if code := cmdRemember([]string{"alpha", "beta"}); code != 0 {
		t.Fatalf("cmdRemember exit = %d", code)
	}
	out, err := findNotes("alpha", 0)
	if err != nil || !strings.Contains(out, "alpha beta") {
		t.Fatalf("remember 后 find: %q, %v", out, err)
	}
}
