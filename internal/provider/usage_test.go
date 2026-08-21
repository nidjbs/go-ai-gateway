package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nidjbs/go-ai-gateway/internal/routing"
)

// Usage.Total excludes cache tokens since they are discounted/reused context.
func TestUsageTotalExcludesCache(t *testing.T) {
	u := Usage{
		InputTokens:         100,
		OutputTokens:        50,
		CacheReadTokens:     200,
		CacheCreationTokens: 75,
		ReasoningTokens:     20,
	}
	if got, want := u.Total(), 150; got != want {
		t.Fatalf("Total() = %d, want %d", got, want)
	}
}

func TestUsageTotalZero(t *testing.T) {
	var u Usage
	if got := u.Total(); got != 0 {
		t.Fatalf("Total() = %d, want 0", got)
	}
}

// ClassifyError maps each legacy error type to its canonical ProviderError kind.
func TestClassifyError(t *testing.T) {
	tests := []struct {
		name string
		in   error
		want ErrorKind
	}{
		{"nil", nil, ""},
		{"HTTPError", &HTTPError{StatusCode: 503, Message: "x"}, ErrorKindUpstream},
		{"RequestError", &RequestError{Message: "x"}, ErrorKindInvalidRequest},
		{"context canceled", context.Canceled, ErrorKindCanceled},
		{"deadline exceeded", context.DeadlineExceeded, ErrorKindTimeout},
		{"unexpected EOF", io.ErrUnexpectedEOF, ErrorKindNetwork},
		{"plain EOF", io.EOF, ErrorKindNetwork},
		{"unknown", errors.New("mystery"), ErrorKindUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ClassifyError(tt.in)
			if tt.in == nil {
				if got != nil {
					t.Fatalf("ClassifyError(nil) = %v, want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("ClassifyError(%v) = nil", tt.in)
			}
			if got.Kind != tt.want {
				t.Fatalf("kind = %q, want %q", got.Kind, tt.want)
			}
		})
	}
}

func TestClassifyErrorNetTimeout(t *testing.T) {
	netErr := &timeoutErr{}
	got := ClassifyError(netErr)
	if got.Kind != ErrorKindTimeout {
		t.Fatalf("kind = %q, want %q", got.Kind, ErrorKindTimeout)
	}
}

func TestClassifyErrorNetNonTimeout(t *testing.T) {
	netErr := &netErrImpl{}
	got := ClassifyError(netErr)
	if got.Kind != ErrorKindNetwork {
		t.Fatalf("kind = %q, want %q", got.Kind, ErrorKindNetwork)
	}
}

func TestClassifyErrorPassthroughProviderError(t *testing.T) {
	original := &ProviderError{Kind: ErrorKindProtocol, Message: "x"}
	got := ClassifyError(original)
	if got != original {
		t.Fatalf("ClassifyError did not pass through ProviderError")
	}
}

func TestIsRetryableKind(t *testing.T) {
	cases := map[ErrorKind]bool{
		ErrorKindNetwork:        true,
		ErrorKindTimeout:        true,
		ErrorKindUpstream:       false,
		ErrorKindProtocol:       false,
		ErrorKindInvalidRequest: false,
		ErrorKindCanceled:       false,
		ErrorKindUnknown:        false,
		ErrorKind("not-a-kind"): false,
	}
	for kind, want := range cases {
		if got := IsRetryableKind(kind); got != want {
			t.Errorf("IsRetryableKind(%q) = %v, want %v", kind, got, want)
		}
	}
}

func TestProviderErrorErrorString(t *testing.T) {
	tests := []struct {
		name string
		pe   *ProviderError
		want string
	}{
		{
			name: "with wrapped error",
			pe:   &ProviderError{Kind: ErrorKindProtocol, Status: 502, Message: "bad json", Wrapped: errors.New("json: bad")},
			want: "provider error (protocol, status=502): bad json",
		},
		{
			name: "no wrapped, no message",
			pe:   &ProviderError{Kind: ErrorKindUpstream, Status: 503},
			want: "provider error (upstream, status=503)",
		},
		{
			name: "message only",
			pe:   &ProviderError{Kind: ErrorKindTimeout, Status: 504, Message: "upstream timeout"},
			want: "provider error (timeout, status=504): upstream timeout",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.pe.Error()
			if !contains(got, tt.want) {
				t.Fatalf("Error() = %q, want substring %q", got, tt.want)
			}
		})
	}
}

func TestProviderErrorUnwrap(t *testing.T) {
	inner := errors.New("inner")
	pe := &ProviderError{Kind: ErrorKindProtocol, Wrapped: inner}
	if !errors.Is(pe, inner) {
		t.Fatalf("errors.Is did not unwrap to inner")
	}
}

// AnthropicResponseMalformedAsProtocolError verifies that a 2xx response with
// unparseable JSON body is treated as a protocol error, not a success.
func TestAnthropicResponseMalformedAsProtocolError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{not-json"))
	}))
	defer upstream.Close()

	_, err := NewClient().Do(context.Background(),
		Request{Operation: ChatCompletions, Body: json.RawMessage(`{"model":"chat","messages":[{"role":"user","content":"hi"}]}`)},
		routing.Candidate{Type: "anthropic", Model: "claude", BaseURL: upstream.URL, APIKey: "x"},
	)
	if err == nil {
		t.Fatal("expected error for malformed response")
	}
	var pe *ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %T %v, want ProviderError", err, err)
	}
	if pe.Kind != ErrorKindProtocol {
		t.Fatalf("kind = %q, want protocol", pe.Kind)
	}
}

// OpenAIResponseMalformedAsProtocolError verifies the same for the OpenAI adapter.
func TestOpenAIResponseMalformedAsProtocolError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("definitely-not-json"))
	}))
	defer upstream.Close()

	_, err := NewClient().Do(context.Background(),
		Request{Operation: ChatCompletions, Body: json.RawMessage(`{"model":"chat","messages":[{"role":"user","content":"hi"}]}`)},
		routing.Candidate{Type: "openai", Model: "gpt-x", BaseURL: upstream.URL, APIKey: "x"},
	)
	if err == nil {
		t.Fatal("expected error for malformed response")
	}
	var pe *ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %T %v, want ProviderError", err, err)
	}
	if pe.Kind != ErrorKindProtocol {
		t.Fatalf("kind = %q, want protocol", pe.Kind)
	}
}

// Test helper types.

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

var _ net.Error = (*timeoutErr)(nil)

type netErrImpl struct{}

func (netErrImpl) Error() string   { return "net" }
func (netErrImpl) Timeout() bool   { return false }
func (netErrImpl) Temporary() bool { return true }

var _ net.Error = (*netErrImpl)(nil)

func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
