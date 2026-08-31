package main

import (
	"strconv"
)

// maxToolResultBytes caps how much of a tool result survives compression in the
// model context. The full result always lands in the session log.
const maxToolResultBytes = 4096

// trimToolResult cuts oversized tool results, keeping a head prefix + marker.
// It only trims when the input clearly exceeds head+marker, so trimming never
// makes the result longer.
func trimToolResult(s string) string {
	marker := "\n… [结果过长,已裁剪前 " + strconv.Itoa(maxToolResultBytes) + " 字节]"
	if len(s) <= maxToolResultBytes+len(marker) {
		return s
	}
	return s[:maxToolResultBytes] + marker
}

// contextCompressor keeps a sliding window of recent messages for the model
// request. Tool results stay full until the window is nearly full (remaining
// below triggerPercent of capacity); then large tool results are trimmed and
// the oldest messages slide out to a 60% low-water mark. The full transcript is
// kept separately for /save and session logs.
type contextCompressor struct {
	capacity     int // message capacity; <= 0 disables compression
	triggerRatio float64
	system       string
	window       []Message
}

func newContextCompressor(system string, capacity, triggerPercent int) *contextCompressor {
	triggerRatio := 0.2
	if triggerPercent > 0 {
		triggerRatio = float64(triggerPercent) / 100
	}
	return &contextCompressor{system: system, capacity: capacity, triggerRatio: triggerRatio}
}

// add appends messages; once the window is nearly full it trims large tool
// results and slides out the oldest, so compression is lazy, not per-message.
func (c *contextCompressor) add(msgs ...Message) {
	c.window = append(c.window, msgs...)
	if c.capacity <= 0 {
		return
	}
	highWater := int(float64(c.capacity) * (1 - c.triggerRatio))
	if len(c.window) > highWater {
		c.compress()
	}
}

// compress trims oversized tool results and keeps the newest lowWater messages.
func (c *contextCompressor) compress() {
	for i := range c.window {
		if c.window[i].Role == "tool" {
			c.window[i].Content = trimToolResult(c.window[i].Content)
		}
	}
	keep := int(float64(c.capacity) * 0.6)
	if keep < 1 {
		keep = 1
	}
	if len(c.window) > keep {
		c.window = append([]Message(nil), c.window[len(c.window)-keep:]...)
	}
}

// requestMessages returns the bounded messages for the model: system prompt
// (if any) then the recent window.
func (c *contextCompressor) requestMessages() []Message {
	msgs := make([]Message, 0, 1+len(c.window))
	if c.system != "" {
		msgs = append(msgs, Message{Role: "system", Content: c.system})
	}
	return append(msgs, c.window...)
}
