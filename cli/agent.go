package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// maxAgentTurns caps tool-calling loops to bound runaway chains.
const maxAgentTurns = 8

// agentReply runs the tool-calling loop until the model stops: send a turn,
// execute any requested tools, feed results back, repeat. It appends assistant
// and tool messages to a copy of history and returns the full updated history
// plus the final assistant text. Session events are emitted as it goes. tools
// controls which tools the model may call (empty = none).
func agentReply(cfg *Config, alias string, history []Message, policy *FilePolicy, log *Session, tools []ToolSpec) ([]Message, string, error) {
	msgs := append([]Message(nil), history...)
	ctx := context.Background()
	client := NewClient(cfg)
	for turn := 1; turn <= maxAgentTurns; turn++ {
		reqID := newEventID()
		start := time.Now()
		res, err := client.AgentTurn(ctx, reqID, alias, msgs, tools)
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
		emit(log, SessionEvent{Type: evAssistantMessage, Turn: turn, RequestID: reqID, Model: alias, Content: res.Content})
		if len(res.ToolCalls) == 0 {
			return msgs, res.Content, nil
		}
		for _, call := range res.ToolCalls {
			path := toolArgPath(call.Function.Arguments)
			emit(log, SessionEvent{Type: evToolCall, Turn: turn, RequestID: reqID, ToolName: call.Function.Name, ToolCallID: call.ID, Path: path})
			fmt.Fprintf(os.Stderr, "gw: [工具] %s %s\n", call.Function.Name, path)
			result, terr := policy.DispatchTool(call)
			if terr != nil {
				result = "错误: " + terr.Error()
				fmt.Fprintln(os.Stderr, "gw:", terr)
			}
			emit(log, SessionEvent{
				Type: evToolResult, Turn: turn, RequestID: reqID,
				ToolName: call.Function.Name, ToolCallID: call.ID, Path: path,
				Allowed: terr == nil, Content: result,
			})
			msgs = append(msgs, Message{Role: "tool", ToolCallID: call.ID, Content: result})
		}
	}
	emit(log, SessionEvent{Type: evAgentError, Model: alias, Message: "超过工具调用轮数上限"})
	return msgs, "", fmt.Errorf("agent 超过 %d 轮工具调用上限", maxAgentTurns)
}

// emit writes a session event, no-op when logging is disabled.
func emit(log *Session, ev SessionEvent) {
	if log != nil {
		log.Emit(ev)
	}
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
