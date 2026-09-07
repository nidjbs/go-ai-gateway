package main

import (
	"context"
	"testing"
)

func TestEvalFakeTextFIFO(t *testing.T) {
	f := newEvalFake([]StubStep{{Text: "一"}, {Text: "二"}}, "兜底")
	defer f.Close()
	c := NewClient(&Config{GatewayURL: f.URL(), APIKey: "sk-test"})
	msgs := []Message{{Role: "user", Content: "q"}}
	for i, want := range []string{"一", "二", "兜底"} {
		got, err := c.Chat(context.Background(), "chat", msgs)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("chat %d = %q, want %q", i, got, want)
		}
	}
}

func TestEvalFakeToolThenText(t *testing.T) {
	f := newEvalFake([]StubStep{
		{ToolCall: &StubTool{Name: "read_file", Arguments: map[string]any{"path": "a.txt"}}},
		{Text: "已读取"},
	}, "好的。")
	defer f.Close()
	c := NewClient(&Config{GatewayURL: f.URL(), APIKey: "sk-test"})
	res, err := c.AgentTurn(context.Background(), "", "chat",
		[]Message{{Role: "user", Content: "读文件"}}, []ToolSpec{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.ToolCalls) != 1 || res.ToolCalls[0].Function.Name != "read_file" {
		t.Fatalf("tool calls = %+v", res.ToolCalls)
	}
	if res.FinishReason != "tool_calls" {
		t.Fatalf("finish = %q", res.FinishReason)
	}
	got, err := c.Chat(context.Background(), "chat", []Message{
		{Role: "tool", ToolCallID: "call_0", Content: "file contents"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != "已读取" {
		t.Fatalf("second chat = %q", got)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func TestEvalFakeStreamText(t *testing.T) {
	f := newEvalFake([]StubStep{{Text: "流式文本"}}, "好的。")
	defer f.Close()
	c := NewClient(&Config{GatewayURL: f.URL(), APIKey: "sk-test"})
	var buf []byte
	res, err := c.AgentTurnStream(context.Background(), "", "chat",
		[]Message{{Role: "user", Content: "hi"}}, nil,
		writerFunc(func(p []byte) (int, error) { buf = append(buf, p...); return len(p), nil }))
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "流式文本" || string(buf) != "流式文本" {
		t.Fatalf("stream = %q / %q", res.Content, buf)
	}
}
