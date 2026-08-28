package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestParseSSEJoinsDeltas(t *testing.T) {
	input := "data: {\"choices\":[{\"delta\":{\"content\":\"he\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"llo\"}}]}\n\n" +
		"data: [DONE]\n\n"
	var buf bytes.Buffer
	err := parseSSE(strings.NewReader(input), func(d string) { buf.WriteString(d) }, nil)
	if err != nil {
		t.Fatal(err)
	}
	if buf.String() != "hello" {
		t.Fatalf("got %q, want hello", buf.String())
	}
}

func TestParseSSESurfacesErrorFrame(t *testing.T) {
	input := "data: {\"error\":{\"message\":\"boom\",\"code\":\"x\"}}\n\n"
	err := parseSSE(strings.NewReader(input), func(string) {}, nil)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v, want boom", err)
	}
}

func TestParseSSESkipsNonDataLines(t *testing.T) {
	input := "event: foo\ndata: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n"
	var buf bytes.Buffer
	err := parseSSE(strings.NewReader(input), func(d string) { buf.WriteString(d) }, nil)
	if err != nil {
		t.Fatal(err)
	}
	if buf.String() != "a" {
		t.Fatalf("got %q, want a", buf.String())
	}
}
