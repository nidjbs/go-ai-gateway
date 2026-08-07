package provider

import (
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

type Client struct{ HTTPClient *http.Client }

func NewClient() *Client { return &Client{HTTPClient: http.DefaultClient} }

type Result struct {
	Body         json.RawMessage
	Model        string
	InputTokens  int
	OutputTokens int
}

func (c *Client) Request(ctx context.Context, endpoint string, body json.RawMessage, candidate routing.Candidate) (Result, error) {
	if candidate.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, candidate.Timeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpointURL(candidate.BaseURL, endpoint), bytes.NewReader(body))
	if err != nil {
		return Result{}, fmt.Errorf("create upstream request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+candidate.APIKey)
	response, err := c.HTTPClient.Do(req)
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
	message := envelope.Error.Message
	if message == "" {
		message = strings.TrimSpace(string(data))
	}
	return &HTTPError{StatusCode: status, Message: message}
}

type Stream struct {
	Response  *http.Response
	StartedAt time.Time
	cancel    context.CancelFunc
}

func (s *Stream) Close() error {
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	if s.Response == nil || s.Response.Body == nil {
		return nil
	}
	return s.Response.Body.Close()
}

func (c *Client) OpenStream(ctx context.Context, body json.RawMessage, candidate routing.Candidate) (*Stream, error) {
	cancel := func() {}
	if candidate.Timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, candidate.Timeout)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpointURL(candidate.BaseURL, "chat/completions"), bytes.NewReader(body))
	if err != nil {
		cancel()
		return nil, fmt.Errorf("create upstream stream: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+candidate.APIKey)
	response, err := c.HTTPClient.Do(req)
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
	return &Stream{Response: response, StartedAt: time.Now(), cancel: cancel}, nil
}
