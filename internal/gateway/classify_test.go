package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"

	"example.com/light-llm-gateway/internal/circuitbreaker"
	"example.com/light-llm-gateway/internal/provider"
)

func TestClassifyProviderError(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantType   string
		wantCode   string
		wantStatus int
	}{
		{
			name:       "protocol",
			err:        &provider.ProviderError{Kind: provider.ErrorKindProtocol, Message: "bad json"},
			wantType:   "upstream_protocol_error",
			wantCode:   "upstream_protocol_error",
			wantStatus: http.StatusBadGateway,
		},
		{
			name:       "upstream sanitises to 502",
			err:        &provider.ProviderError{Kind: provider.ErrorKindUpstream, Status: 503},
			wantType:   "upstream_error",
			wantCode:   "upstream_error",
			wantStatus: http.StatusBadGateway,
		},
		{
			name:       "timeout maps to 504",
			err:        &provider.ProviderError{Kind: provider.ErrorKindTimeout, Message: "ctx deadline"},
			wantType:   "upstream_timeout",
			wantCode:   "upstream_timeout",
			wantStatus: http.StatusGatewayTimeout,
		},
		{
			name:       "canceled maps to 499",
			err:        &provider.ProviderError{Kind: provider.ErrorKindCanceled, Message: "client aborted"},
			wantType:   "client_aborted",
			wantCode:   "client_aborted",
			wantStatus: 499,
		},
		{
			name:       "network maps to 502",
			err:        &provider.ProviderError{Kind: provider.ErrorKindNetwork, Message: "eof"},
			wantType:   "upstream_network_error",
			wantCode:   "upstream_network_error",
			wantStatus: http.StatusBadGateway,
		},
		{
			name:       "invalid request maps to 400",
			err:        &provider.ProviderError{Kind: provider.ErrorKindInvalidRequest, Message: "bad input"},
			wantType:   "invalid_request",
			wantCode:   "invalid_request",
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "unknown falls back to upstream_error/502",
			err:        &provider.ProviderError{Kind: provider.ErrorKindUnknown, Message: "?"},
			wantType:   "upstream_error",
			wantCode:   "upstream_error",
			wantStatus: http.StatusBadGateway,
		},
		{
			name:       "legacy HTTPError sanitises to 502",
			err:        &provider.HTTPError{StatusCode: 503, Message: "boom"},
			wantType:   "upstream_error",
			wantCode:   "upstream_error",
			wantStatus: http.StatusBadGateway,
		},
		{
			name:       "circuit breaker open maps to 502",
			err:        circuitbreaker.ErrOpen,
			wantType:   "upstream_unavailable",
			wantCode:   "upstream_unavailable",
			wantStatus: http.StatusBadGateway,
		},
		{
			name:       "untyped error maps to upstream_error/502",
			err:        errors.New("generic"),
			wantType:   "upstream_error",
			wantCode:   "upstream_error",
			wantStatus: http.StatusBadGateway,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotType, gotCode, gotStatus := classifyProviderError(tt.err)
			if gotType != tt.wantType || gotCode != tt.wantCode || gotStatus != tt.wantStatus {
				t.Fatalf("classify(%v) = (%q, %q, %d); want (%q, %q, %d)",
					tt.err, gotType, gotCode, gotStatus, tt.wantType, tt.wantCode, tt.wantStatus)
			}
		})
	}
}

func TestStreamOutcomeFor(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantType   string
		wantStatus int
	}{
		{
			name:       "canceled maps to 499",
			err:        context.Canceled,
			wantType:   "client_aborted",
			wantStatus: 499,
		},
		{
			name:       "protocol maps to 502",
			err:        &provider.ProviderError{Kind: provider.ErrorKindProtocol},
			wantType:   "upstream_protocol_error",
			wantStatus: http.StatusBadGateway,
		},
		{
			name:       "timeout maps to 504",
			err:        &provider.ProviderError{Kind: provider.ErrorKindTimeout},
			wantType:   "upstream_timeout",
			wantStatus: http.StatusGatewayTimeout,
		},
		{
			name:       "network maps to 502",
			err:        &provider.ProviderError{Kind: provider.ErrorKindNetwork},
			wantType:   "upstream_network_error",
			wantStatus: http.StatusBadGateway,
		},
		{
			name:       "upstream maps to 502",
			err:        &provider.ProviderError{Kind: provider.ErrorKindUpstream, Status: 503},
			wantType:   "upstream_error",
			wantStatus: http.StatusBadGateway,
		},
		{
			name:       "io EOF maps to upstream_error/502",
			err:        io.EOF,
			wantType:   "upstream_error",
			wantStatus: http.StatusBadGateway,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotType, _, gotStatus := streamOutcomeFor(tt.err, context.Background())
			if gotType != tt.wantType || gotStatus != tt.wantStatus {
				t.Fatalf("streamOutcomeFor(%v) = (%q, %d); want (%q, %d)",
					tt.err, gotType, gotStatus, tt.wantType, tt.wantStatus)
			}
		})
	}
}
