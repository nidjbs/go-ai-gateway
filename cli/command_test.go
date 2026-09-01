package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestParseCommandFrontmatter(t *testing.T) {
	data := []byte("---\nname: weekly-report\ndescription: 周报\ntools: [read_file, write_file]\nschedule: \"0 9 * * 1\"\n---\n\n你是周报助手。\n")
	cmd, err := parseCommand(data)
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Name != "weekly-report" || cmd.Description != "周报" || cmd.Schedule != "0 9 * * 1" {
		t.Fatalf("cmd = %+v", cmd)
	}
	if !reflect.DeepEqual(cmd.Tools, []string{"read_file", "write_file"}) {
		t.Fatalf("tools = %v", cmd.Tools)
	}
	if cmd.Body != "你是周报助手。" {
		t.Fatalf("body = %q", cmd.Body)
	}
}

func TestParseCommandNoFrontmatter(t *testing.T) {
	cmd, err := parseCommand([]byte("纯文本正文"))
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Body != "纯文本正文" {
		t.Fatalf("body = %q", cmd.Body)
	}
	if cmd.Tools != nil || cmd.Schedule != "" {
		t.Fatalf("unexpected metadata: %+v", cmd)
	}
}

func TestParseCommandUnclosedFrontmatter(t *testing.T) {
	if _, err := parseCommand([]byte("---\nname: x\nbody without close")); err == nil {
		t.Fatal("unclosed frontmatter must fail")
	}
}

func TestWriteCommandRoundtrip(t *testing.T) {
	dir := t.TempDir()
	cmd := &Command{Name: "report", Description: "周报", Tools: []string{"read_file"}, Schedule: "@every 24h", Body: "正文"}
	if err := writeCommand(dir, "report", cmd); err != nil {
		t.Fatal(err)
	}
	got, err := loadCommand(filepath.Join(dir, "report.md"), "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "report" || got.Schedule != "@every 24h" || got.Body != "正文" {
		t.Fatalf("roundtrip = %+v", got)
	}
	if !reflect.DeepEqual(got.Tools, []string{"read_file"}) {
		t.Fatalf("tools = %v", got.Tools)
	}
}

func TestSelectTools(t *testing.T) {
	if got := selectTools([]string{"read_file", "write_file"}); len(got) != 2 {
		t.Fatalf("all tools = %d", len(got))
	}
	if got := selectTools([]string{"read_file"}); len(got) != 1 || got[0].Function.Name != "read_file" {
		t.Fatalf("one = %v", got)
	}
	if got := selectTools([]string{"bogus"}); got != nil {
		t.Fatal("unknown tools should be filtered out")
	}
	if got := selectTools(nil); got != nil {
		t.Fatal("empty tools should be nil")
	}
}

func TestLoadCommandBuiltin(t *testing.T) {
	cmd, err := loadCommand("trans", "")
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Name != "trans" || cmd.Body == "" {
		t.Fatalf("builtin = %+v", cmd)
	}
}

func TestParseCommandOutput(t *testing.T) {
	cases := []struct {
		name string
		in   string
		body string
	}{
		{"well-formed", "---\nname: x\ndescription: d\n---\n\n正文", "正文"},
		{"code-fenced", "```\n---\nname: x\n---\n\n正文\n```", "正文"},
		{"unclosed-frontmatter", "---\nname: x\ndescription: d\n\n正文", "正文"},
		{"plain-body", "直接正文内容", "直接正文内容"},
	}
	for _, tc := range cases {
		cmd, err := parseCommandOutput(tc.name, tc.in)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if cmd.Name != tc.name || cmd.Body != tc.body {
			t.Fatalf("%s: got %+v, body want %q", tc.name, cmd, tc.body)
		}
	}
}

func TestSystemPromptForStripsFrontmatter(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "y.md"), []byte("---\nname: y\n---\n\nbody here\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	body, err := systemPromptFor("y", dir)
	if err != nil {
		t.Fatal(err)
	}
	if body != "body here" {
		t.Fatalf("body = %q", body)
	}
}
