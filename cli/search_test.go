package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func newSearchPolicy(t *testing.T) (*FilePolicy, string) {
	t.Helper()
	root := t.TempDir()
	p, err := newFilePolicy([]string{root}, "never")
	if err != nil {
		t.Fatal(err)
	}
	return p, root
}

func writeSearchFixture(t *testing.T, root string) {
	t.Helper()
	files := map[string]string{
		"a.go":        "package a\nfunc X() { println(\"hello world\") }\n",
		"note.txt":    "secret token abc123\n",
		"sub/b.go":    "package b\n// TODO fix\nfunc Y() {}\n",
		".git/x.go":   "func hidden() {}\n",
		".venv/y.txt": "virtual env file\n",
	}
	for rel, content := range files {
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFindFiles(t *testing.T) {
	p, root := newSearchPolicy(t)
	writeSearchFixture(t, root)
	got, err := p.findFiles("*.go", root)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"a.go", "sub/b.go"} {
		if !strings.Contains(got, want) {
			t.Fatalf("find_files 缺少 %q:\n%s", want, got)
		}
	}
	// .git 和隐藏目录里的文件不得出现
	if strings.Contains(got, ".git") || strings.Contains(got, ".venv") {
		t.Fatalf("find_files 不应进入隐藏目录:\n%s", got)
	}
	// 无匹配
	empty, err := p.findFiles("*.dmg", root)
	if err != nil || empty != "(未找到匹配的文件)" {
		t.Fatalf("find_files 空结果 = %q, %v", empty, err)
	}
}

func TestFindFilesOutsideRoot(t *testing.T) {
	p, _ := newSearchPolicy(t)
	if _, err := p.findFiles("*.go", os.TempDir()); err == nil || !strings.Contains(err.Error(), "不在允许") {
		t.Fatalf("find_files 越界应拒绝, got %v", err)
	}
}

func TestSearchText(t *testing.T) {
	p, root := newSearchPolicy(t)
	writeSearchFixture(t, root)
	got, err := p.searchText("hello", root, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "a.go:2:") || !strings.Contains(got, "hello world") {
		t.Fatalf("search_text 子串:\n%s", got)
	}
	// 正则
	got, err = p.searchText(`TODO|secret`, root, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "sub/b.go") || !strings.Contains(got, "note.txt") {
		t.Fatalf("search_text 正则:\n%s", got)
	}
	// 二进制与 .git 内容不可达
	if err := os.WriteFile(filepath.Join(root, "bin.dat"), []byte{0x00, 0x01, 'h', 'i'}, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = p.searchText("hidden", root, false)
	if err != nil {
		t.Fatal(err)
	}
	if got != "(未找到匹配的内容)" {
		t.Fatalf("search_text 不应命中隐藏/二进制: %q", got)
	}
}

func TestTailFile(t *testing.T) {
	p, root := newSearchPolicy(t)
	log := filepath.Join(root, "app.log")
	// 大到超过 tailReadChunk(64KB),必须走"只读尾部"分支
	var big strings.Builder
	for i := 1; i <= 20000; i++ {
		big.WriteString("line-" + strconv.Itoa(i) + "\n")
	}
	if err := os.WriteFile(log, []byte(big.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := p.tailFile(log, 3)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "line-19998") || !strings.Contains(got, "line-20000") || strings.Contains(got, "\nline-1\n") {
		t.Fatalf("tail_file 应只含末尾行:\n%s", got)
	}
	if !strings.Contains(got, "仅返回末尾") {
		t.Fatalf("tail_file 大文件应有截断标记:\n%s", got)
	}
	// 目录不可 tail
	if _, err := p.tailFile(root, 10); err == nil {
		t.Fatal("tail_file 目录应失败")
	}
}
