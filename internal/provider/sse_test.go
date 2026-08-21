package provider

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestSSEParserParsesSimpleEvents(t *testing.T) {
	body := strings.Join([]string{
		"event: foo",
		"data: hello",
		"",
		"event: bar",
		`data: {"a":1}`,
		"",
	}, "\n")
	p := newSSEParser(strings.NewReader(body))
	first, err := p.Next()
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if first.Type != "foo" || string(first.Data) != "hello" {
		t.Fatalf("first = %+v", first)
	}
	second, err := p.Next()
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if second.Type != "bar" || string(second.Data) != `{"a":1}` {
		t.Fatalf("second = %+v", second)
	}
	if _, err := p.Next(); err != io.EOF {
		t.Fatalf("expected EOF, got %v", err)
	}
}

func TestSSEParserJoinsMultipleDataLines(t *testing.T) {
	body := "data: one\ndata: two\ndata: three\n\n"
	p := newSSEParser(strings.NewReader(body))
	evt, err := p.Next()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if string(evt.Data) != "one\ntwo\nthree" {
		t.Fatalf("data = %q", evt.Data)
	}
}

func TestSSEParserSkipsComments(t *testing.T) {
	body := ":heartbeat\nevent: ping\n: keepalive\n\n"
	p := newSSEParser(strings.NewReader(body))
	evt, err := p.Next()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if evt.Type != "ping" {
		t.Fatalf("type = %q", evt.Type)
	}
}

func TestSSEParserAcceptsCRLF(t *testing.T) {
	body := "event: x\r\ndata: a\r\n\r\n"
	p := newSSEParser(strings.NewReader(body))
	evt, err := p.Next()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if evt.Type != "x" || string(evt.Data) != "a" {
		t.Fatalf("event = %+v", evt)
	}
}

func TestSSEParserSplitsAcrossReads(t *testing.T) {
	// Feed the body in 1-byte chunks so the parser's buffered reads are
	// exercised across line and event boundaries.
	body := "data: ab\ndata: cd\n\n"
	p := newSSEParser(bufio.NewReaderSize(strings.NewReader(body), 2))
	evt, err := p.Next()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if string(evt.Data) != "ab\ncd" {
		t.Fatalf("data = %q", evt.Data)
	}
}

func TestSSEParserLineTooLarge(t *testing.T) {
	// sseMaxLine + 1 byte on a single line, terminated by \n.
	big := bytes.Repeat([]byte{'a'}, sseMaxLine+1)
	big = append(big, '\n')
	_, err := newSSEParser(bytes.NewReader(big)).Next()
	if !errors.Is(err, ErrSSELineTooLarge) {
		t.Fatalf("expected ErrSSELineTooLarge, got %v", err)
	}
}

func TestSSEParserEventTooLarge(t *testing.T) {
	// Many medium data lines joined by \n push past sseMaxEvent.
	// Each line must carry the "data:" field so the parser treats it as
	// additional event data rather than an unknown field.
	const chunk = 1024
	lines := (sseMaxEvent / chunk) + 2
	var buf bytes.Buffer
	for i := 0; i < lines; i++ {
		buf.WriteString("data: ")
		buf.Write(bytes.Repeat([]byte{'a'}, chunk))
		buf.WriteByte('\n')
	}
	buf.WriteByte('\n')
	_, err := newSSEParser(bytes.NewReader(buf.Bytes())).Next()
	if !errors.Is(err, ErrSSEEventTooLarge) {
		t.Fatalf("expected ErrSSEEventTooLarge, got %v", err)
	}
}

func TestSSEParserHandlesIDField(t *testing.T) {
	body := "id: 42\nevent: update\ndata: hi\n\n"
	p := newSSEParser(strings.NewReader(body))
	evt, err := p.Next()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if evt.ID != "42" || evt.Type != "update" || string(evt.Data) != "hi" {
		t.Fatalf("event = %+v", evt)
	}
}

func TestSSEParserHandlesDataWithoutEventField(t *testing.T) {
	// OpenAI-style: only a "data:" line, no "event:" field.
	body := "data: [DONE]\n\n"
	p := newSSEParser(strings.NewReader(body))
	evt, err := p.Next()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if string(evt.Data) != "[DONE]" {
		t.Fatalf("data = %q", evt.Data)
	}
}

func TestSSEParserIgnoresBlankLinesBetweenEvents(t *testing.T) {
	body := "\n\ndata: a\n\n\ndata: b\n\n"
	p := newSSEParser(strings.NewReader(body))
	first, err := p.Next()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if string(first.Data) != "a" {
		t.Fatalf("first = %q", first.Data)
	}
	second, err := p.Next()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if string(second.Data) != "b" {
		t.Fatalf("second = %q", second.Data)
	}
}

func TestFormatSSERoundTrip(t *testing.T) {
	in := sseEvent{Type: "message", Data: []byte("hello\nworld"), ID: "7"}
	r := formatSSE(in)
	p := newSSEParser(bytes.NewReader(r))
	got, err := p.Next()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got.Type != in.Type || string(got.Data) != string(in.Data) || got.ID != in.ID {
		t.Fatalf("round-trip mismatch: in=%+v out=%+v", in, got)
	}
}
