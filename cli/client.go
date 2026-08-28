package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Message is one OpenAI-style chat message.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Client talks to a running go-ai-gateway over its OpenAI-compatible API and
// admin surface.
type Client struct {
	baseURL    string // OpenAI-compatible API (main port)
	adminURL   string // ops/healthz port for admin endpoints
	apiKey     string
	adminToken string
	http       *http.Client
}

func NewClient(cfg *Config) *Client {
	adminURL := cfg.AdminURL
	if adminURL == "" {
		adminURL = cfg.GatewayURL
	}
	return &Client{
		baseURL:    strings.TrimRight(cfg.GatewayURL, "/"),
		adminURL:   strings.TrimRight(adminURL, "/"),
		apiKey:     cfg.APIKey,
		adminToken: cfg.AdminToken,
		http:       &http.Client{},
	}
}

// do performs an authenticated request; body nil means no payload.
func (c *Client) do(ctx context.Context, method, path, token string, body any) (*http.Response, error) {
	return c.doURL(ctx, method, c.baseURL, path, token, body)
}

// doAdmin is do against the admin (ops) endpoint.
func (c *Client) doAdmin(ctx context.Context, method, path string, body any) (*http.Response, error) {
	return c.doURL(ctx, method, c.adminURL, path, c.adminToken, body)
}

func (c *Client) doURL(ctx context.Context, method, base, path, token string, body any) (*http.Response, error) {
	var rdr io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return c.http.Do(req)
}

// apiError reads a gateway error envelope into a readable error.
func apiError(resp *http.Response) error {
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var env struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err == nil && env.Error.Message != "" {
		return fmt.Errorf("gateway error (%d): %s", resp.StatusCode, env.Error.Message)
	}
	return fmt.Errorf("gateway error (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
}

// Models lists the aliases the gateway exposes via /v1/models.
func (c *Client) Models(ctx context.Context) ([]string, error) {
	resp, err := c.do(ctx, http.MethodGet, "/v1/models", c.apiKey, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, apiError(resp)
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(out.Data))
	for _, m := range out.Data {
		if m.ID != "" {
			names = append(names, m.ID)
		}
	}
	return names, nil
}

// ChatStream streams a chat completion, writing content deltas to w.
func (c *Client) ChatStream(ctx context.Context, model string, messages []Message, w io.Writer) error {
	payload := map[string]any{"model": model, "messages": messages, "stream": true}
	resp, err := c.do(ctx, http.MethodPost, "/v1/chat/completions", c.apiKey, payload)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return apiError(resp)
	}
	defer resp.Body.Close()
	return parseSSE(resp.Body, func(delta string) {
		if delta != "" {
			_, _ = io.WriteString(w, delta)
		}
	}, nil)
}

// Chat runs a non-streaming chat completion and returns the assistant content.
func (c *Client) Chat(ctx context.Context, model string, messages []Message) (string, error) {
	payload := map[string]any{"model": model, "messages": messages, "stream": false}
	resp, err := c.do(ctx, http.MethodPost, "/v1/chat/completions", c.apiKey, payload)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", apiError(resp)
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("empty completion response")
	}
	return out.Choices[0].Message.Content, nil
}

// Reload triggers the gateway's config hot-reload; path optionally overrides
// the config file location.
func (c *Client) Reload(ctx context.Context, path string) error {
	payload := map[string]any{}
	if path != "" {
		payload["path"] = path
	}
	resp, err := c.doAdmin(ctx, http.MethodPost, "/admin/reload", payload)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return apiError(resp)
	}
	defer resp.Body.Close()
	return nil
}

// Status checks gateway liveness via /healthz.
func (c *Client) Status(ctx context.Context) error {
	resp, err := c.do(ctx, http.MethodGet, "/healthz", "", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return apiError(resp)
	}
	return nil
}

// Usage queries the admin usage summary; filters are optional.
func (c *Client) Usage(ctx context.Context, alias, from, to string) (string, error) {
	q := []string{}
	if alias != "" {
		q = append(q, "alias="+alias)
	}
	if from != "" {
		q = append(q, "from="+from)
	}
	if to != "" {
		q = append(q, "to="+to)
	}
	path := "/admin/usage/summary"
	if len(q) > 0 {
		path += "?" + strings.Join(q, "&")
	}
	resp, err := c.doAdmin(ctx, http.MethodGet, path, nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", apiError(resp)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return string(body), err
}
