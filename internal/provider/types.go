package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
)

// ErrorKind classifies provider failures for routing decisions and audit labels.
// ProviderError.Kind is the single source of truth; HTTPError and RequestError
// are kept for backward compatibility and converted to ProviderError at the gateway edge.
type ErrorKind string

const (
	// ErrorKindProtocol indicates a malformed or unparseable response on the wire.
	// This is non-retryable: the upstream gave us something that cannot be consumed.
	ErrorKindProtocol ErrorKind = "protocol"
	// ErrorKindUpstream indicates an upstream-returned HTTP error (>=400).
	// Retryability depends on the status code.
	ErrorKindUpstream ErrorKind = "upstream"
	// ErrorKindTimeout indicates the request exceeded its deadline.
	// Retryable in most configurations.
	ErrorKindTimeout ErrorKind = "timeout"
	// ErrorKindCanceled indicates the caller canceled the request.
	// Never retryable.
	ErrorKindCanceled ErrorKind = "canceled"
	// ErrorKindNetwork indicates a transport-level failure (DNS, TCP, TLS, EOF).
	// Retryable by default.
	ErrorKindNetwork ErrorKind = "network"
	// ErrorKindInvalidRequest indicates the client payload was rejected before
	// reaching the wire (validation, malformed JSON). Never retryable.
	ErrorKindInvalidRequest ErrorKind = "invalid_request"
	// ErrorKindUnknown is the fallback when classification is impossible.
	ErrorKindUnknown ErrorKind = "unknown"
)

// ProviderError is the unified error type returned by adapters. Adapters should
// produce one of these directly; the retry executor and gateway classify and
// label errors through Kind. Existing HTTPError/RequestError types are preserved
// for compatibility but are wrapped/converted at the gateway.
type ProviderError struct {
	Kind    ErrorKind
	Status  int // HTTP status from upstream; 0 if not applicable.
	Message string
	Wrapped error
}

func (e *ProviderError) Error() string {
	if e.Wrapped != nil {
		return fmt.Sprintf("provider error (%s, status=%d): %s: %v", e.Kind, e.Status, e.Message, e.Wrapped)
	}
	if e.Message == "" {
		return fmt.Sprintf("provider error (%s, status=%d)", e.Kind, e.Status)
	}
	return fmt.Sprintf("provider error (%s, status=%d): %s", e.Kind, e.Status, e.Message)
}

func (e *ProviderError) Unwrap() error { return e.Wrapped }

// HTTPError is retained for backward compatibility; new code should use ProviderError.
// It is converted to ProviderError{Kind: ErrorKindUpstream, Status, Message} at the edge.
type HTTPError struct {
	StatusCode int
	Message    string
}

func (e *HTTPError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("upstream returned HTTP %d", e.StatusCode)
	}
	return fmt.Sprintf("upstream returned HTTP %d: %s", e.StatusCode, e.Message)
}

// ClassifyError maps an arbitrary error from an adapter into a ProviderError.
// If err already is a *ProviderError, it is returned as-is. HTTPError,
// RequestError, context.Canceled, io errors, and net.Error are mapped to the
// corresponding kind. Unknown errors are wrapped as ErrorKindUnknown.
func ClassifyError(err error) *ProviderError {
	if err == nil {
		return nil
	}
	var pe *ProviderError
	if errors.As(err, &pe) {
		return pe
	}
	var httpErr *HTTPError
	if errors.As(err, &httpErr) {
		return &ProviderError{Kind: ErrorKindUpstream, Status: httpErr.StatusCode, Message: httpErr.Error(), Wrapped: err}
	}
	var reqErr *RequestError
	if errors.As(err, &reqErr) {
		return &ProviderError{Kind: ErrorKindInvalidRequest, Message: reqErr.Error(), Wrapped: err}
	}
	if errors.Is(err, context.Canceled) {
		return &ProviderError{Kind: ErrorKindCanceled, Message: err.Error(), Wrapped: err}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &ProviderError{Kind: ErrorKindTimeout, Message: err.Error(), Wrapped: err}
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return &ProviderError{Kind: ErrorKindNetwork, Message: err.Error(), Wrapped: err}
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		kind := ErrorKindNetwork
		if netErr.Timeout() {
			kind = ErrorKindTimeout
		}
		return &ProviderError{Kind: kind, Message: err.Error(), Wrapped: err}
	}
	return &ProviderError{Kind: ErrorKindUnknown, Message: err.Error(), Wrapped: err}
}

// IsRetryableKind returns whether errors of this kind are eligible for retry
// under the default policy. Specific status codes (e.g., 429) are checked by
// the retry executor against RetryableStatuses.
func IsRetryableKind(k ErrorKind) bool {
	switch k {
	case ErrorKindNetwork, ErrorKindTimeout:
		return true
	case ErrorKindUpstream, ErrorKindProtocol, ErrorKindInvalidRequest, ErrorKindCanceled, ErrorKindUnknown:
		return false
	}
	return false
}
