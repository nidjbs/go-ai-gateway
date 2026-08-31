package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

// Message is one OpenAI-style chat message. ToolCallID is set for role=tool
// results, ToolCalls for role=assistant function-call turns.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
}

// ToolCall is one function call requested by the model.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

// ToolFunction carries the invoked function's name and JSON arguments.
type ToolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ToolSpec advertises a callable tool to the model (OpenAI function calling).
type ToolSpec struct {
	Type     string           `json:"type"`
	Function ToolSpecFunction `json:"function"`
}

// ToolSpecFunction describes one function to the model.
type ToolSpecFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// AgentResult is one non-streaming agent turn: reply content, requested tool
// calls, finish reason, and token usage.
type AgentResult struct {
	Content      string
	ToolCalls    []ToolCall
	FinishReason string
	InputTokens  int
	OutputTokens int
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
	return c.doURL(ctx, method, c.baseURL, path, token, body, "")
}

// doAdmin is do against the admin (ops) endpoint.
func (c *Client) doAdmin(ctx context.Context, method, path string, body any) (*http.Response, error) {
	return c.doURL(ctx, method, c.adminURL, path, c.adminToken, body, "")
}

func (c *Client) doURL(ctx context.Context, method, base, path, token string, body any, reqID string) (*http.Response, error) {
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
	if reqID != "" {
		req.Header.Set("X-Request-Id", reqID)
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

// AgentTurn runs one non-streaming chat turn with tools, returning the model's
// reply plus any tool calls it requested. reqID is forwarded as X-Request-Id so
// gateway-side events correlate with the CLI session log.
func (c *Client) AgentTurn(ctx context.Context, reqID, model string, messages []Message, tools []ToolSpec) (*AgentResult, error) {
	payload := map[string]any{"model": model, "messages": messages, "stream": false}
	if len(tools) > 0 {
		payload["tools"] = tools
	}
	resp, err := c.doURL(ctx, http.MethodPost, c.baseURL, "/v1/chat/completions", c.apiKey, payload, reqID)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, apiError(resp)
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content   string     `json:"content"`
				ToolCalls []ToolCall `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if len(out.Choices) == 0 {
		return nil, fmt.Errorf("empty completion response")
	}
	m := out.Choices[0].Message
	return &AgentResult{
		Content:      m.Content,
		ToolCalls:    m.ToolCalls,
		FinishReason: out.Choices[0].FinishReason,
		InputTokens:  out.Usage.PromptTokens,
		OutputTokens: out.Usage.CompletionTokens,
	}, nil
}

// AgentTurnStream runs a streaming chat turn with tools, writing content deltas
// to w as they arrive and accumulating tool_calls (fragments merged by index).
// Returns the full turn result for session logging.
func (c *Client) AgentTurnStream(ctx context.Context, reqID, model string, messages []Message, tools []ToolSpec, w io.Writer) (*AgentResult, error) {
	payload := map[string]any{"model": model, "messages": messages, "stream": true}
	if len(tools) > 0 {
		payload["tools"] = tools
	}
	resp, err := c.doURL(ctx, http.MethodPost, c.baseURL, "/v1/chat/completions", c.apiKey, payload, reqID)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, apiError(resp)
	}
	if w == nil {
		w = io.Discard
	}
	res := &AgentResult{}
	type streamTool struct {
		id   string
		name string
		args strings.Builder
	}
	toolsByIndex := map[int]*streamTool{}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var ev struct {
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			continue
		}
		if ev.Error != nil && ev.Error.Message != "" {
			return nil, fmt.Errorf("stream error: %s", ev.Error.Message)
		}
		if ev.Usage.PromptTokens > 0 || ev.Usage.CompletionTokens > 0 {
			res.InputTokens = ev.Usage.PromptTokens
			res.OutputTokens = ev.Usage.CompletionTokens
		}
		if len(ev.Choices) == 0 {
			continue
		}
		ch := ev.Choices[0]
		if ch.Delta.Content != "" {
			if _, err := io.WriteString(w, ch.Delta.Content); err != nil {
				return nil, err
			}
			res.Content += ch.Delta.Content
		}
		for _, tc := range ch.Delta.ToolCalls {
			slot, ok := toolsByIndex[tc.Index]
			if !ok {
				slot = &streamTool{}
				toolsByIndex[tc.Index] = slot
			}
			if tc.ID != "" {
				slot.id = tc.ID
			}
			if tc.Function.Name != "" {
				slot.name = tc.Function.Name
			}
			slot.args.WriteString(tc.Function.Arguments)
		}
		if ch.FinishReason != "" {
			res.FinishReason = ch.FinishReason
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	indices := make([]int, 0, len(toolsByIndex))
	for i := range toolsByIndex {
		indices = append(indices, i)
	}
	sort.Ints(indices)
	for _, i := range indices {
		slot := toolsByIndex[i]
		res.ToolCalls = append(res.ToolCalls, ToolCall{
			ID:   slot.id,
			Type: "function",
			Function: ToolFunction{
				Name:      slot.name,
				Arguments: slot.args.String(),
			},
		})
	}
	return res, nil
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
