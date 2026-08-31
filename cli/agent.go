package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"
)

// maxAgentTurns caps tool-calling loops to bound runaway chains.
const maxAgentTurns = 20

// agentReply runs the tool-calling loop until the model stops: send a turn,
// execute any requested tools, feed results back, repeat. It appends assistant
// and tool messages to a copy of history and returns the full updated history
// plus the final assistant text. Session events are emitted as it goes. tools
// controls which tools the model may call (empty = none). When w is non-nil,
// assistant text is streamed to w as it arrives (REPL); otherwise the caller
// prints the returned text itself.
func agentReply(cfg *Config, alias string, history []Message, policy *FilePolicy, log *Session, tools []ToolSpec, w io.Writer) ([]Message, string, error) {
	msgs := append([]Message(nil), history...)
	ctx := context.Background()
	client := NewClient(cfg)
	for turn := 1; turn <= maxAgentTurns; turn++ {
		reqID := newEventID()
		start := time.Now()
		var res *AgentResult
		var err error
		if w != nil {
			res, err = client.AgentTurnStream(ctx, reqID, alias, msgs, tools, w)
		} else {
			res, err = client.AgentTurn(ctx, reqID, alias, msgs, tools)
		}
		if err != nil {
			emit(log, SessionEvent{Type: evAgentError, Turn: turn, RequestID: reqID, Model: alias, Message: err.Error()})
			return msgs, "", err
		}
		emit(log, SessionEvent{
			Type: evModelRequest, Turn: turn, RequestID: reqID, Model: alias,
			InputTokens: res.InputTokens, OutputTokens: res.OutputTokens,
			DurationMS: time.Since(start).Milliseconds(),
		})
		msgs = append(msgs, Message{Role: "assistant", Content: res.Content, ToolCalls: res.ToolCalls})
		emit(log, SessionEvent{Type: evAssistantMessage, Role: "assistant", Turn: turn, RequestID: reqID, Model: alias, Content: res.Content, ToolCalls: res.ToolCalls})
		if len(res.ToolCalls) == 0 {
			return msgs, res.Content, nil
		}
		for _, call := range res.ToolCalls {
			path := toolArgPath(call.Function.Arguments)
			emit(log, SessionEvent{Type: evToolCall, Turn: turn, RequestID: reqID, ToolName: call.Function.Name, ToolCallID: call.ID, Arguments: call.Function.Arguments, Path: path})
			fmt.Fprintf(os.Stderr, "gw: [工具] %s %s\n", call.Function.Name, path)
			result, terr := policy.DispatchTool(call)
			if terr != nil {
				result = "错误: " + terr.Error()
				fmt.Fprintln(os.Stderr, "gw:", terr)
			}
			seq := emit(log, SessionEvent{
				Type: evToolResult, Role: "tool", Turn: turn, RequestID: reqID,
				ToolName: call.Function.Name, ToolCallID: call.ID, Path: path,
				Allowed: terr == nil, Content: result,
			})
			// The full result is logged; the surface immediately shows a pruned
			// version if oversized, so later turns never carry a full dump.
			maybePruneToolResult(log, seq, result)
			// In-loop the model still sees the full result to finish the turn.
			msgs = append(msgs, Message{Role: "tool", ToolCallID: call.ID, Content: result})
		}
	}
	emit(log, SessionEvent{Type: evAgentError, Model: alias, Message: "超过工具调用轮数上限"})
	return msgs, "", fmt.Errorf("agent 超过 %d 轮工具调用上限", maxAgentTurns)
}

// emit writes a session event, no-op when logging is disabled; returns the
// assigned seq (0 when logging is off).
func emit(log *Session, ev SessionEvent) int64 {
	if log != nil {
		return log.Emit(ev)
	}
	return 0
}

// toolArgPath extracts the path argument from a tool call's JSON arguments.
func toolArgPath(args string) string {
	var a struct {
		Path string `json:"path"`
	}
	if json.Unmarshal([]byte(args), &a) == nil {
		return a.Path
	}
	return ""
}
