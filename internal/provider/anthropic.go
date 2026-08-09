package provider

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"example.com/light-llm-gateway/internal/routing"
)

const anthropicVersion = "2023-06-01"

type anthropicAdapter struct {
	client *Client
}

func (a *anthropicAdapter) Type() string { return "anthropic" }

func (a *anthropicAdapter) Supports(operation Operation) bool {
	return operation == ChatCompletions
}

func (a *anthropicAdapter) Validate(request Request) error {
	if request.Operation != ChatCompletions {
		return &RequestError{Message: "Anthropic providers support chat.completions only"}
	}
	_, err := anthropicRequest(request.Body, "model", false)
	return err
}

func (a *anthropicAdapter) Do(ctx context.Context, request Request, candidate routing.Candidate) (Result, error) {
	body, err := anthropicRequest(request.Body, candidate.Model, false)
	if err != nil {
		return Result{}, err
	}
	response, cancel, err := a.open(ctx, body, candidate, false)
	if err != nil {
		return Result{}, err
	}
	defer cancel()
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 16<<20))
	if err != nil {
		return Result{}, fmt.Errorf("read upstream response: %w", err)
	}
	if response.StatusCode >= http.StatusBadRequest {
		return Result{}, decodeHTTPError(response.StatusCode, data)
	}
	return anthropicResult(data)
}

func (a *anthropicAdapter) OpenStream(ctx context.Context, request Request, candidate routing.Candidate) (Stream, error) {
	body, err := anthropicRequest(request.Body, candidate.Model, true)
	if err != nil {
		return nil, err
	}
	response, cancel, err := a.open(ctx, body, candidate, true)
	if err != nil {
		return nil, err
	}
	if response.StatusCode >= http.StatusBadRequest {
		defer cancel()
		defer response.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		return nil, decodeHTTPError(response.StatusCode, data)
	}
	return &anthropicStream{scanner: bufio.NewScanner(response.Body), response: response, cancel: cancel}, nil
}

func (a *anthropicAdapter) open(ctx context.Context, body []byte, candidate routing.Candidate, stream bool) (*http.Response, context.CancelFunc, error) {
	req, cancel, err := a.client.newRequest(ctx, http.MethodPost, endpointURL(candidate.BaseURL, "messages"), body, candidate.Timeout)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", candidate.APIKey)
	req.Header.Set("Anthropic-Version", anthropicVersion)
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	response, err := a.client.HTTPClient.Do(req)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	return response, cancel, nil
}

type openAIRequest struct {
	Messages          []openAIMessage `json:"messages"`
	Tools             []openAITool    `json:"tools"`
	ToolChoice        json.RawMessage `json:"tool_choice"`
	MaxTokens         *int            `json:"max_tokens"`
	Temperature       *float64        `json:"temperature"`
	TopP              *float64        `json:"top_p"`
	Stream            bool            `json:"stream"`
	Metadata          json.RawMessage `json:"metadata"`
	ResponseFormat    json.RawMessage `json:"response_format"`
	ParallelToolCalls json.RawMessage `json:"parallel_tool_calls"`
	Logprobs          json.RawMessage `json:"logprobs"`
	TopLogprobs       json.RawMessage `json:"top_logprobs"`
	PresencePenalty   json.RawMessage `json:"presence_penalty"`
	FrequencyPenalty  json.RawMessage `json:"frequency_penalty"`
	Seed              json.RawMessage `json:"seed"`
	N                 *int            `json:"n"`
	User              json.RawMessage `json:"user"`
	Modalities        json.RawMessage `json:"modalities"`
	Audio             json.RawMessage `json:"audio"`
	ReasoningEffort   json.RawMessage `json:"reasoning_effort"`
	Reasoning         json.RawMessage `json:"reasoning"`
}

type openAIMessage struct {
	Role       string           `json:"role"`
	Content    json.RawMessage  `json:"content"`
	ToolCalls  []openAIToolCall `json:"tool_calls"`
	ToolCallID string           `json:"tool_call_id"`
	Name       string           `json:"name"`
}

type openAITool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

type openAIToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type anthropicWireRequest struct {
	Model       string             `json:"model"`
	MaxTokens   int                `json:"max_tokens"`
	System      string             `json:"system,omitempty"`
	Messages    []anthropicMessage `json:"messages"`
	Tools       []anthropicTool    `json:"tools,omitempty"`
	ToolChoice  any                `json:"tool_choice,omitempty"`
	Temperature *float64           `json:"temperature,omitempty"`
	TopP        *float64           `json:"top_p,omitempty"`
	Stream      bool               `json:"stream,omitempty"`
	Metadata    json.RawMessage    `json:"metadata,omitempty"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

func anthropicRequest(raw json.RawMessage, model string, stream bool) ([]byte, error) {
	var in openAIRequest
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, &RequestError{Message: "invalid JSON request"}
	}
	if err := rejectUnsupported(in); err != nil {
		return nil, err
	}
	if len(in.Messages) == 0 {
		return nil, &RequestError{Message: "messages are required"}
	}
	out := anthropicWireRequest{Model: model, MaxTokens: 1024, Temperature: in.Temperature, TopP: in.TopP, Stream: stream, Metadata: in.Metadata}
	if in.MaxTokens != nil {
		if *in.MaxTokens <= 0 {
			return nil, &RequestError{Message: "max_tokens must be greater than zero"}
		}
		out.MaxTokens = *in.MaxTokens
	}
	for _, tool := range in.Tools {
		if tool.Type != "function" || strings.TrimSpace(tool.Function.Name) == "" || len(tool.Function.Parameters) == 0 {
			return nil, &RequestError{Message: "only function tools with name and parameters are supported for Anthropic providers"}
		}
		if !json.Valid(tool.Function.Parameters) {
			return nil, &RequestError{Message: "tool parameters must be valid JSON"}
		}
		out.Tools = append(out.Tools, anthropicTool{Name: tool.Function.Name, Description: tool.Function.Description, InputSchema: tool.Function.Parameters})
	}
	choice, err := anthropicToolChoice(in.ToolChoice)
	if err != nil {
		return nil, err
	}
	out.ToolChoice = choice
	messages, system, err := anthropicMessages(in.Messages)
	if err != nil {
		return nil, err
	}
	out.Messages, out.System = messages, system
	return json.Marshal(out)
}

func rejectUnsupported(in openAIRequest) error {
	for name, value := range map[string]json.RawMessage{
		"response_format": in.ResponseFormat, "parallel_tool_calls": in.ParallelToolCalls, "logprobs": in.Logprobs,
		"top_logprobs": in.TopLogprobs, "presence_penalty": in.PresencePenalty, "frequency_penalty": in.FrequencyPenalty,
		"seed": in.Seed, "user": in.User, "modalities": in.Modalities, "audio": in.Audio,
		"reasoning_effort": in.ReasoningEffort, "reasoning": in.Reasoning,
	} {
		if len(value) > 0 && string(value) != "null" {
			return unsupportedField(name)
		}
	}
	if in.N != nil && *in.N != 1 {
		return unsupportedField("n values other than 1")
	}
	return nil
}

func anthropicToolChoice(raw json.RawMessage) (any, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var stringChoice string
	if json.Unmarshal(raw, &stringChoice) == nil {
		switch stringChoice {
		case "auto":
			return map[string]string{"type": "auto"}, nil
		case "none":
			return map[string]string{"type": "none"}, nil
		case "required":
			return map[string]string{"type": "any"}, nil
		default:
			return nil, &RequestError{Message: "unsupported tool_choice"}
		}
	}
	var choice struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &choice); err != nil || choice.Type != "function" || strings.TrimSpace(choice.Function.Name) == "" {
		return nil, &RequestError{Message: "unsupported tool_choice"}
	}
	return map[string]string{"type": "tool", "name": choice.Function.Name}, nil
}

func anthropicMessages(in []openAIMessage) ([]anthropicMessage, string, error) {
	var out []anthropicMessage
	var system []string
	knownCalls := map[string]struct{}{}
	for index := 0; index < len(in); index++ {
		message := in[index]
		switch message.Role {
		case "system":
			text, err := textContent(message.Content)
			if err != nil {
				return nil, "", err
			}
			system = append(system, text)
		case "user":
			content, err := textBlocks(message.Content)
			if err != nil {
				return nil, "", err
			}
			out = append(out, anthropicMessage{Role: "user", Content: content})
		case "assistant":
			content, err := textBlocks(message.Content)
			if err != nil {
				return nil, "", err
			}
			for _, call := range message.ToolCalls {
				if call.Type != "function" || call.ID == "" || call.Function.Name == "" {
					return nil, "", &RequestError{Message: "assistant tool_calls must be function calls with id and name"}
				}
				var input any
				if err := json.Unmarshal([]byte(call.Function.Arguments), &input); err != nil {
					return nil, "", &RequestError{Message: "tool call arguments must be valid JSON"}
				}
				content = append(content, map[string]any{"type": "tool_use", "id": call.ID, "name": call.Function.Name, "input": input})
				knownCalls[call.ID] = struct{}{}
			}
			out = append(out, anthropicMessage{Role: "assistant", Content: content})
		case "tool":
			var results []any
			for index < len(in) && in[index].Role == "tool" {
				tool := in[index]
				if tool.ToolCallID == "" {
					return nil, "", &RequestError{Message: "tool messages require tool_call_id"}
				}
				if _, ok := knownCalls[tool.ToolCallID]; !ok {
					return nil, "", &RequestError{Message: "tool result references an unknown tool_call_id"}
				}
				text, err := textContent(tool.Content)
				if err != nil {
					return nil, "", err
				}
				results = append(results, map[string]any{"type": "tool_result", "tool_use_id": tool.ToolCallID, "content": text})
				index++
			}
			index--
			out = append(out, anthropicMessage{Role: "user", Content: results})
		default:
			return nil, "", &RequestError{Message: fmt.Sprintf("message role %q is not supported for Anthropic providers", message.Role)}
		}
	}
	if len(out) == 0 {
		return nil, "", &RequestError{Message: "at least one user or assistant message is required"}
	}
	return out, strings.Join(system, "\n\n"), nil
}

func textContent(raw json.RawMessage) (string, error) {
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text, nil
	}
	blocks, err := textBlocks(raw)
	if err != nil {
		return "", err
	}
	var texts []string
	for _, block := range blocks {
		if value, ok := block.(map[string]any); ok {
			if text, ok := value["text"].(string); ok {
				texts = append(texts, text)
			}
		}
	}
	return strings.Join(texts, ""), nil
}

func textBlocks(raw json.RawMessage) ([]any, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return []any{map[string]any{"type": "text", "text": text}}, nil
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, &RequestError{Message: "message content must be text"}
	}
	out := make([]any, 0, len(blocks))
	for _, block := range blocks {
		if block.Type != "text" {
			return nil, &RequestError{Message: "only text message content is supported for Anthropic providers"}
		}
		out = append(out, map[string]any{"type": "text", "text": block.Text})
	}
	return out, nil
}

type anthropicWireResponse struct {
	ID         string `json:"id"`
	Model      string `json:"model"`
	StopReason string `json:"stop_reason"`
	Content    []struct {
		Type  string          `json:"type"`
		Text  string          `json:"text"`
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	} `json:"content"`
	Usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

func anthropicResult(data []byte) (Result, error) {
	var in anthropicWireResponse
	if err := json.Unmarshal(data, &in); err != nil {
		return Result{}, fmt.Errorf("decode Anthropic response: %w", err)
	}
	message := map[string]any{"role": "assistant"}
	var texts []string
	var calls []any
	for index, block := range in.Content {
		switch block.Type {
		case "text":
			texts = append(texts, block.Text)
		case "tool_use":
			arguments := string(block.Input)
			if arguments == "" {
				arguments = "{}"
			}
			calls = append(calls, map[string]any{"index": index, "id": block.ID, "type": "function", "function": map[string]any{"name": block.Name, "arguments": arguments}})
		default:
			return Result{}, fmt.Errorf("unsupported Anthropic response content block %q", block.Type)
		}
	}
	if len(texts) > 0 {
		message["content"] = strings.Join(texts, "")
	} else {
		message["content"] = nil
	}
	if len(calls) > 0 {
		message["tool_calls"] = calls
	}
	body, err := json.Marshal(map[string]any{
		"id":      in.ID,
		"object":  "chat.completion",
		"model":   in.Model,
		"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": openAIFinishReason(in.StopReason)}},
		"usage":   map[string]int{"prompt_tokens": in.Usage.InputTokens, "completion_tokens": in.Usage.OutputTokens, "total_tokens": in.Usage.InputTokens + in.Usage.OutputTokens},
	})
	if err != nil {
		return Result{}, err
	}
	return Result{Body: body, Model: in.Model, InputTokens: in.Usage.InputTokens, OutputTokens: in.Usage.OutputTokens}, nil
}

func openAIFinishReason(reason string) string {
	switch reason {
	case "tool_use":
		return "tool_calls"
	case "max_tokens", "model_context_window_exceeded":
		return "length"
	case "end_turn", "stop_sequence":
		return "stop"
	default:
		return "stop"
	}
}

type anthropicStream struct {
	scanner      *bufio.Scanner
	response     *http.Response
	cancel       context.CancelFunc
	model        string
	inputTokens  int
	outputTokens int
	completed    bool
}

func (s *anthropicStream) Next() (StreamEvent, error) {
	for s.scanner.Scan() {
		line := s.scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := []byte(strings.TrimPrefix(line, "data: "))
		var event struct {
			Type    string `json:"type"`
			Message struct {
				ID    string `json:"id"`
				Model string `json:"model"`
				Usage struct {
					InputTokens int `json:"input_tokens"`
				} `json:"usage"`
			} `json:"message"`
			Index        int `json:"index"`
			ContentBlock struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"content_block"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				PartialJSON string `json:"partial_json"`
				StopReason  string `json:"stop_reason"`
			} `json:"delta"`
			Usage struct {
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(data, &event); err != nil {
			return StreamEvent{}, fmt.Errorf("decode Anthropic stream event: %w", err)
		}
		switch event.Type {
		case "ping", "content_block_stop":
			continue
		case "error":
			return StreamEvent{}, &HTTPError{StatusCode: http.StatusBadGateway, Message: event.Error.Message}
		case "message_start":
			s.model = event.Message.Model
			s.inputTokens = event.Message.Usage.InputTokens
			return anthropicChunk(s.model, map[string]any{"role": "assistant"}, ""), nil
		case "content_block_start":
			if event.ContentBlock.Type == "tool_use" {
				return anthropicChunk(s.model, map[string]any{"tool_calls": []any{map[string]any{"index": event.Index, "id": event.ContentBlock.ID, "type": "function", "function": map[string]any{"name": event.ContentBlock.Name, "arguments": ""}}}}, ""), nil
			}
		case "content_block_delta":
			switch event.Delta.Type {
			case "text_delta":
				return anthropicChunk(s.model, map[string]any{"content": event.Delta.Text}, ""), nil
			case "input_json_delta":
				return anthropicChunk(s.model, map[string]any{"tool_calls": []any{map[string]any{"index": event.Index, "function": map[string]any{"arguments": event.Delta.PartialJSON}}}}, ""), nil
			}
		case "message_delta":
			s.outputTokens = event.Usage.OutputTokens
			return anthropicChunk(s.model, map[string]any{}, openAIFinishReason(event.Delta.StopReason)), nil
		case "message_stop":
			s.completed = true
			return StreamEvent{Done: true, InputTokens: s.inputTokens, OutputTokens: s.outputTokens}, nil
		}
	}
	if err := s.scanner.Err(); err != nil {
		return StreamEvent{}, err
	}
	if s.completed {
		return StreamEvent{}, io.EOF
	}
	return StreamEvent{}, io.ErrUnexpectedEOF
}

func anthropicChunk(model string, delta map[string]any, finishReason string) StreamEvent {
	data, _ := json.Marshal(map[string]any{"id": "chatcmpl-anthropic", "object": "chat.completion.chunk", "model": model, "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": nullableFinishReason(finishReason)}}})
	return StreamEvent{Data: data}
}

func nullableFinishReason(reason string) any {
	if reason == "" {
		return nil
	}
	return reason
}

func (s *anthropicStream) Close() error {
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	if s.response == nil || s.response.Body == nil {
		return nil
	}
	return s.response.Body.Close()
}

var _ Adapter = (*anthropicAdapter)(nil)
var _ Stream = (*anthropicStream)(nil)
