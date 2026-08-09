package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"example.com/light-llm-gateway/internal/routing"
)

type Operation string

const (
	ChatCompletions Operation = "chat.completions"
	Embeddings      Operation = "embeddings"
)

type Request struct {
	Operation Operation
	Body      json.RawMessage
}

type Result struct {
	Body         json.RawMessage
	Model        string
	InputTokens  int
	OutputTokens int
}

type StreamEvent struct {
	Data         json.RawMessage
	Done         bool
	InputTokens  int
	OutputTokens int
}

type Stream interface {
	Next() (StreamEvent, error)
	Close() error
}

type Adapter interface {
	Type() string
	Supports(Operation) bool
	Validate(Request) error
	Do(context.Context, Request, routing.Candidate) (Result, error)
	OpenStream(context.Context, Request, routing.Candidate) (Stream, error)
}

type RequestError struct {
	Message string
}

func (e *RequestError) Error() string {
	if strings.TrimSpace(e.Message) == "" {
		return "invalid request"
	}
	return e.Message
}

type Client struct {
	HTTPClient *http.Client
	adapters   map[string]Adapter
}

func NewClient() *Client {
	client := &Client{HTTPClient: http.DefaultClient}
	client.adapters = map[string]Adapter{
		"openai":    &openAIAdapter{client: client},
		"anthropic": &anthropicAdapter{client: client},
	}
	return client
}

func (c *Client) Supports(candidate routing.Candidate, operation Operation) bool {
	adapter, ok := c.adapter(candidate.Type)
	return ok && adapter.Supports(operation)
}

func (c *Client) Filter(candidates []routing.Candidate, request Request) ([]routing.Candidate, error) {
	out := make([]routing.Candidate, 0, len(candidates))
	var validationErr error
	for _, candidate := range candidates {
		adapter, ok := c.adapter(candidate.Type)
		if !ok || !adapter.Supports(request.Operation) {
			continue
		}
		if err := adapter.Validate(request); err != nil {
			validationErr = err
			continue
		}
		out = append(out, candidate)
	}
	if len(out) == 0 && validationErr != nil {
		return nil, validationErr
	}
	return out, nil
}

func (c *Client) Do(ctx context.Context, request Request, candidate routing.Candidate) (Result, error) {
	adapter, ok := c.adapter(candidate.Type)
	if !ok {
		return Result{}, fmt.Errorf("provider type %q is unsupported", candidate.Type)
	}
	if !adapter.Supports(request.Operation) {
		return Result{}, &RequestError{Message: fmt.Sprintf("provider type %q does not support %s", candidate.Type, request.Operation)}
	}
	if err := adapter.Validate(request); err != nil {
		return Result{}, err
	}
	return adapter.Do(ctx, request, candidate)
}

func (c *Client) OpenStream(ctx context.Context, request Request, candidate routing.Candidate) (Stream, error) {
	adapter, ok := c.adapter(candidate.Type)
	if !ok {
		return nil, fmt.Errorf("provider type %q is unsupported", candidate.Type)
	}
	if !adapter.Supports(request.Operation) {
		return nil, &RequestError{Message: fmt.Sprintf("provider type %q does not support %s", candidate.Type, request.Operation)}
	}
	if err := adapter.Validate(request); err != nil {
		return nil, err
	}
	return adapter.OpenStream(ctx, request, candidate)
}

func (c *Client) adapter(typ string) (Adapter, bool) {
	if strings.TrimSpace(typ) == "" {
		typ = "openai"
	}
	adapter, ok := c.adapters[strings.ToLower(strings.TrimSpace(typ))]
	return adapter, ok
}

func (c *Client) newRequest(ctx context.Context, method, url string, body []byte, timeout time.Duration) (*http.Request, context.CancelFunc, error) {
	cancel := func() {}
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, timeout)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		cancel()
		return nil, nil, fmt.Errorf("create upstream request: %w", err)
	}
	return req, cancel, nil
}

func endpointURL(baseURL, endpoint string) string {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if strings.HasSuffix(base, "/v1") {
		return base + "/" + endpoint
	}
	return base + "/v1/" + endpoint
}

func decodeHTTPError(status int, data []byte) error {
	var envelope struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(data, &envelope)
	message := strings.TrimSpace(envelope.Error.Message)
	if message == "" {
		message = strings.TrimSpace(string(data))
	}
	return &HTTPError{StatusCode: status, Message: message}
}
