package main

import (
	"strconv"
)

// Tool-result pruning budgets: a result is pruned when it exceeds the threshold,
// keeping head + omission marker + tail, bounded. The full original always stays
// in the session log.
const (
	toolResultThreshold = 8192 // prune when content exceeds this
	toolResultHead      = 4096 // keep this many head chars
	toolResultTail      = 1024 // keep this many tail chars
)

// trimToolResult prunes an oversized tool result to head + marker + tail, so the
// end of a large file (often the most relevant part) survives. It only prunes
// when clearly over budget, so trimming never makes the result longer.
func trimToolResult(s string) string {
	if len(s) <= toolResultThreshold {
		return s
	}
	omitted := len(s) - toolResultHead - toolResultTail
	marker := "\n… [中间省略 " + strconv.Itoa(omitted) + " 字节]\n"
	head := toolResultHead
	if head+len(marker)+toolResultTail > toolResultThreshold {
		head = toolResultThreshold - len(marker) - toolResultTail
	}
	return s[:head] + marker + s[len(s)-toolResultTail:]
}

// maybePruneToolResult replaces an over-budget tool result on the surface with a
// pruned version via a replace event (the original stays in the log). Called
// right after the tool result is emitted, so the surface never carries a full
// oversized result into later turns.
func maybePruneToolResult(sess *Session, seq int64, result string) {
	if trimmed := trimToolResult(result); trimmed != result {
		sess.EmitTrim(seq, trimmed)
	}
}

// maybeCompact applies the round-based sliding-window policy: when the surface
// is nearly full (remaining below triggerPercent of capacity), the oldest
// messages slide out via a context.compact event. The log keeps every original
// event. capacity <= 0 disables compression.
func maybeCompact(sess *Session, capacity, triggerPercent int) {
	if capacity <= 0 {
		return
	}
	surface := sess.surfaceEvents()
	highWater := int(float64(capacity) * (1 - float64(triggerPercent)/100))
	if len(surface) <= highWater {
		return
	}
	keep := max(int(float64(capacity)*0.6), 1)
	drop := max(len(surface)-keep, 0)
	if drop > 0 {
		shadow := make([]int64, 0, drop)
		for i := range drop {
			shadow = append(shadow, surface[i].Seq)
		}
		sess.Compact(shadow)
	}
}
