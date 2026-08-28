package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuiltinPromptFillsPlaceholder(t *testing.T) {
	p := builtinPrompt("trans", "英文")
	if !strings.Contains(p, "英文") {
		t.Fatalf("trans prompt must mention target language: %q", p)
	}
	if builtinPrompt("nope") != "" {
		t.Fatal("unknown builtin must return empty")
	}
}

func TestSystemPromptForCustomFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "legal.md")
	if err := os.WriteFile(path, []byte("你是一个法律助手"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := systemPromptFor("legal", dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != "你是一个法律助手" {
		t.Fatalf("got %q", got)
	}
}

func TestSystemPromptForBuiltinName(t *testing.T) {
	got, err := systemPromptFor("summarize", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "摘要") {
		t.Fatalf("builtin lookup must return the template: %q", got)
	}
}

func TestSystemPromptForRawText(t *testing.T) {
	got, err := systemPromptFor("  用俳句风格回答  ", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got != "用俳句风格回答" {
		t.Fatalf("raw text must be trimmed as-is: %q", got)
	}
}
