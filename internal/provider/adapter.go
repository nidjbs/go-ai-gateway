package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"example.com/light-llm-gateway/internal/routing"
)

type Operation string

const (
	ChatCompletions Operation = "chat.completions"
	Responses       Operation = "responses"
	Embeddings      Operation = "embeddings"
)

type Request struct {
	Operation Operation
	Body      json.RawMessage
}

// Usage carries token accounting for a single upstream response or stream.
// It supersedes the older InputTokens/OutputTokens fields on Result/StreamEvent.
type Usage struct {
	InputTokens         int
	OutputTokens        int
	CacheReadTokens     int
	CacheCreationTokens int
	ReasoningTokens     int
}

// Total returns the canonical token total used for quota charge and audit.
// Cache tokens are excluded from the billable total since they represent
// discounted/reused context, not fresh generation.
func (u Usage) Total() int {
	return u.InputTokens + u.OutputTokens
}

type Result struct {
	Body  json.RawMessage
	Model string
	Usage Usage
}

type StreamEvent struct {
	Event string
	Data  json.RawMessage
	Done  bool
	Usage Usage
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

func NewClient() *Client {
	c := NewClientWithOpts(defaultClientOpts())
	c.adapters = map[string]Adapter{
		"openai":    &openAIAdapter{client: c},
		"anthropic": &anthropicAdapter{client: c},
	}
	return c
}

func (c *Client) Supports(candidate routing.Candidate, operation Operation) bool {
	adapter, ok := c.adapter(candidate.Type)
	return ok && adapter.Supports(operation)
}

// Filter returns the subset of candidates whose adapter both supports the
// operation and validates the request body. When no candidate survives, the
// validation error from the FIRST failing candidate is returned, since
// candidates are ordered by priority and the primary candidate's diagnosis is
// the most actionable signal for the caller.
func (c *Client) Filter(candidates []routing.Candidate, request Request) ([]routing.Candidate, error) {
	out := make([]routing.Candidate, 0, len(candidates))
	var validationErr error
	for _, candidate := range candidates {
		adapter, ok := c.adapter(candidate.Type)
		if !ok || !adapter.Supports(request.Operation) {
			continue
		}
		if err := adapter.Validate(request); err != nil {
			if validationErr == nil {
				validationErr = err
			}
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

// sseScannerMaxLine caps a single SSE line; the bufio.Scanner default of 64 KiB is too small for large JSON payloads.
const sseScannerMaxLine = 4 << 20 // 4 MiB

// newSSEScanner returns a bufio.Scanner configured for SSE with an enlarged line-size cap.
func newSSEScanner(r io.Reader) *bufio.Scanner {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), sseScannerMaxLine)
	return scanner
}
