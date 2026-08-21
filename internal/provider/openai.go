package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/tidwall/sjson"

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
	response, err := a.client.HTTPClient(a.Type()).Do(req)
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
			PromptTokens        int `json:"prompt_tokens"`
			CompletionTokens    int `json:"completion_tokens"`
			PromptTokensDetails *struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return Result{}, &ProviderError{Kind: ErrorKindProtocol, Message: "decode OpenAI response", Wrapped: err}
	}
	usage := Usage{InputTokens: envelope.Usage.PromptTokens, OutputTokens: envelope.Usage.CompletionTokens}
	if envelope.Usage.PromptTokensDetails != nil {
		usage.CacheReadTokens = envelope.Usage.PromptTokensDetails.CachedTokens
	}
	return Result{Body: data, Model: envelope.Model, Usage: usage}, nil
}

func (a *openAIAdapter) OpenStream(ctx context.Context, request Request, candidate routing.Candidate) (Stream, error) {
	if request.Operation != ChatCompletions {
		return nil, unsupportedField(string(request.Operation))
	}
	// Detach from the caller's cancel: a stream outlives the attempt that produced it,
	// and retry/per-attempt cancellation must not abort reads of subsequent chunks.
	// candidate.Timeout still bounds the request via newRequest below.
	streamCtx := context.WithoutCancel(ctx)
	body := ensureIncludeUsage(request.Body)
	req, cancel, err := a.client.newRequest(streamCtx, http.MethodPost, endpointURL(candidate.BaseURL, "chat/completions"), bodyWithModel(body, candidate.Model), candidate.Timeout)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+candidate.APIKey)
	response, err := a.client.HTTPClient(a.Type()).Do(req)
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
	return &openAIStream{parser: newSSEParser(response.Body), response: response, cancel: cancel}, nil
}

// ensureIncludeUsage returns body with stream_options.include_usage=true set.
// The OpenAI streaming protocol only emits a usage chunk when this flag is on;
// without it the gateway cannot report input/output tokens.
func ensureIncludeUsage(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	var value map[string]json.RawMessage
	if err := json.Unmarshal(body, &value); err != nil {
		return body
	}
	opts, ok := value["stream_options"]
	if ok && len(opts) > 0 && !bytesEqual(opts, []byte("null")) {
		var parsed struct {
			IncludeUsage bool `json:"include_usage"`
		}
		if err := json.Unmarshal(opts, &parsed); err == nil && parsed.IncludeUsage {
			return body
		}
	}
	streamOpts := map[string]any{"include_usage": true}
	encoded, err := json.Marshal(streamOpts)
	if err != nil {
		return body
	}
	value["stream_options"] = encoded
	out, err := json.Marshal(value)
	if err != nil {
		return body
	}
	return out
}

func bytesEqual(a, b []byte) bool {
	return string(a) == string(b)
}

type openAIStream struct {
	parser       *sseParser
	response     *http.Response
	cancel       context.CancelFunc
	model        string
	inputTokens  int
	outputTokens int
	cacheRead    int
	completed    bool
}

func (s *openAIStream) Next() (StreamEvent, error) {
	for {
		evt, err := s.parser.Next()
		if err != nil {
			if err == io.EOF {
				if s.completed {
					return StreamEvent{}, io.EOF
				}
				return StreamEvent{}, io.ErrUnexpectedEOF
			}
			return StreamEvent{}, err
		}
		if len(evt.Data) == 0 {
			continue
		}
		if string(evt.Data) == "[DONE]" {
			s.completed = true
			return StreamEvent{Done: true, Usage: Usage{
				InputTokens:     s.inputTokens,
				OutputTokens:    s.outputTokens,
				CacheReadTokens: s.cacheRead,
			}}, nil
		}
		var peek struct {
			Model   string `json:"model"`
			Choices []struct {
				Delta json.RawMessage `json:"delta"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens        int `json:"prompt_tokens"`
				CompletionTokens    int `json:"completion_tokens"`
				PromptTokensDetails *struct {
					CachedTokens int `json:"cached_tokens"`
				} `json:"prompt_tokens_details"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(evt.Data, &peek); err != nil {
			return StreamEvent{}, &ProviderError{Kind: ErrorKindProtocol, Message: "decode OpenAI stream event", Wrapped: err}
		}
		if s.model == "" && peek.Model != "" {
			s.model = peek.Model
		}
		event := StreamEvent{Data: json.RawMessage(evt.Data)}
		// Usage-only chunk: choices is empty but usage is present.
		if peek.Usage != nil && len(peek.Choices) == 0 {
			s.inputTokens = peek.Usage.PromptTokens
			s.outputTokens = peek.Usage.CompletionTokens
			if peek.Usage.PromptTokensDetails != nil {
				s.cacheRead = peek.Usage.PromptTokensDetails.CachedTokens
			}
			event.Usage = Usage{
				InputTokens:     s.inputTokens,
				OutputTokens:    s.outputTokens,
				CacheReadTokens: s.cacheRead,
			}
		}
		return event, nil
	}
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

// bodyWithModel rewrites the "model" field in the request body using sjson's
// path-aware JSON mutation, so the original field order and untouched keys are
// preserved. Invalid JSON falls back to the original body.
func bodyWithModel(body json.RawMessage, model string) json.RawMessage {
	out, err := sjson.SetBytes(body, "model", model)
	if err != nil {
		return body
	}
	return out
}

var _ Adapter = (*openAIAdapter)(nil)
var _ Stream = (*openAIStream)(nil)
