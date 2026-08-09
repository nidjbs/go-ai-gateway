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

type openAIAdapter struct {
	client *Client
}

func (a *openAIAdapter) Type() string { return "openai" }

func (a *openAIAdapter) Supports(operation Operation) bool {
	return operation == ChatCompletions || operation == Embeddings
}

func (a *openAIAdapter) Validate(Request) error { return nil }

func (a *openAIAdapter) Do(ctx context.Context, request Request, candidate routing.Candidate) (Result, error) {
	endpoint := ""
	switch request.Operation {
	case ChatCompletions:
		endpoint = "chat/completions"
	case Embeddings:
		endpoint = "embeddings"
	default:
		return Result{}, unsupportedField(string(request.Operation))
	}
	req, cancel, err := a.client.newRequest(ctx, http.MethodPost, endpointURL(candidate.BaseURL, endpoint), bodyWithModel(request.Body, candidate.Model), candidate.Timeout)
	if err != nil {
		return Result{}, err
	}
	defer cancel()
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+candidate.APIKey)
	response, err := a.client.HTTPClient.Do(req)
	if err != nil {
		return Result{}, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 16<<20))
	if err != nil {
		return Result{}, fmt.Errorf("read upstream response: %w", err)
	}
	if response.StatusCode >= http.StatusBadRequest {
		return Result{}, decodeHTTPError(response.StatusCode, data)
	}
	var envelope struct {
		Model string `json:"model"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	_ = json.Unmarshal(data, &envelope)
	return Result{Body: data, Model: envelope.Model, InputTokens: envelope.Usage.PromptTokens, OutputTokens: envelope.Usage.CompletionTokens}, nil
}

func (a *openAIAdapter) OpenStream(ctx context.Context, request Request, candidate routing.Candidate) (Stream, error) {
	if request.Operation != ChatCompletions {
		return nil, unsupportedField(string(request.Operation))
	}
	req, cancel, err := a.client.newRequest(ctx, http.MethodPost, endpointURL(candidate.BaseURL, "chat/completions"), bodyWithModel(request.Body, candidate.Model), candidate.Timeout)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+candidate.APIKey)
	response, err := a.client.HTTPClient.Do(req)
	if err != nil {
		cancel()
		return nil, err
	}
	if response.StatusCode >= http.StatusBadRequest {
		defer response.Body.Close()
		defer cancel()
		data, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		return nil, decodeHTTPError(response.StatusCode, data)
	}
	return &openAIStream{scanner: bufio.NewScanner(response.Body), response: response, cancel: cancel}, nil
}

type openAIStream struct {
	scanner   *bufio.Scanner
	response  *http.Response
	cancel    context.CancelFunc
	completed bool
}

func (s *openAIStream) Next() (StreamEvent, error) {
	for s.scanner.Scan() {
		line := s.scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			s.completed = true
			return StreamEvent{Done: true}, nil
		}
		return StreamEvent{Data: json.RawMessage(payload)}, nil
	}
	if err := s.scanner.Err(); err != nil {
		return StreamEvent{}, err
	}
	if s.completed {
		return StreamEvent{}, io.EOF
	}
	return StreamEvent{}, io.ErrUnexpectedEOF
}

func (s *openAIStream) Close() error {
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	if s.response == nil || s.response.Body == nil {
		return nil
	}
	return s.response.Body.Close()
}

func bodyWithModel(body json.RawMessage, model string) json.RawMessage {
	var value map[string]json.RawMessage
	if json.Unmarshal(body, &value) != nil {
		return body
	}
	encoded, _ := json.Marshal(model)
	value["model"] = encoded
	out, _ := json.Marshal(value)
	return out
}

var _ Adapter = (*openAIAdapter)(nil)
var _ Stream = (*openAIStream)(nil)
