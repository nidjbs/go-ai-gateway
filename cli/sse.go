package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// parseSSE reads an OpenAI-compatible SSE stream ("data: {json}\n\n"), invoking
// onDelta for each content chunk and onDone before a [DONE] marker or EOF.
func parseSSE(r io.Reader, onDelta func(string), onDone func()) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			if onDone != nil {
				onDone()
			}
			return nil
		}
		var ev struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
			Error *struct {
				Message string `json:"message"`
				Code    string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			continue // non-JSON frame; skip
		}
		if ev.Error != nil && ev.Error.Message != "" {
			return fmt.Errorf("stream error: %s", ev.Error.Message)
		}
		if len(ev.Choices) > 0 {
			onDelta(ev.Choices[0].Delta.Content)
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if onDone != nil {
		onDone()
	}
	return nil
}
