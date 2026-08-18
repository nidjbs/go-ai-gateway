package provider

import (
	"net/http"
	"sort"
	"time"
)

// Client holds per-provider HTTP clients so each provider type gets its own connection pool.
type Client struct {
	httpClients map[string]*http.Client // key = provider type (e.g. "openai", "anthropic")
	adapters    map[string]Adapter
	defaultOpts ClientOpts
}

// ClientOpts controls default Transport settings for per-provider HTTP clients.
type ClientOpts struct {
	MaxIdleConns        int
	MaxIdleConnsPerHost int
	MaxConnsPerHost     int
	IdleConnTimeout     time.Duration
	TLSHandshakeTimeout time.Duration
	Timeout             time.Duration
}

// defaultClientOpts matches http.DefaultTransport so callers that omit options get unchanged behavior.
func defaultClientOpts() ClientOpts {
	return ClientOpts{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		MaxConnsPerHost:     10,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
		Timeout:             0, // no global timeout; per-request timeout is set by callers
	}
}

func newHTTPClient(opts ClientOpts) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:        opts.MaxIdleConns,
			MaxIdleConnsPerHost: opts.MaxIdleConnsPerHost,
			MaxConnsPerHost:     opts.MaxConnsPerHost,
			IdleConnTimeout:     opts.IdleConnTimeout,
			TLSHandshakeTimeout: opts.TLSHandshakeTimeout,
		},
		Timeout: opts.Timeout,
	}
}

// NewClientWithOpts is like NewClient but applies the given options to every per-provider HTTP client.
func NewClientWithOpts(opts ClientOpts) *Client {
	c := &Client{
		httpClients: make(map[string]*http.Client),
		defaultOpts: opts,
	}
	for _, typ := range []string{"openai", "anthropic"} {
		c.httpClients[typ] = newHTTPClient(opts)
	}
	return c
}

// HTTPClient returns the *http.Client for the given provider type; unknown types fall back to "openai" then http.DefaultClient.
func (c *Client) HTTPClient(providerType string) *http.Client {
	if cl, ok := c.httpClients[providerType]; ok {
		return cl
	}
	if cl, ok := c.httpClients["openai"]; ok {
		return cl
	}
	return http.DefaultClient
}

// SetHTTPClient replaces the *http.Client for a specific provider type (e.g. to wrap Transport with oTel tracing).
func (c *Client) SetHTTPClient(providerType string, cl *http.Client) {
	c.httpClients[providerType] = cl
}

// RegisteredTypes returns the sorted provider types that have their own pool.
func (c *Client) RegisteredTypes() []string {
	types := make([]string, 0, len(c.httpClients))
	for t := range c.httpClients {
		types = append(types, t)
	}
	sort.Strings(types)
	return types
}

func unsupportedField(name string) error {
	return &RequestError{Message: name + " is not supported for Anthropic providers"}
}
