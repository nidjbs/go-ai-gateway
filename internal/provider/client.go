package provider

import (
	"net/http"
	"sort"
	"time"
)

// Client manages per-provider HTTP clients so that each provider type gets
// its own connection pool (Transport, idle-conn settings, TLS config, etc.)
// without sharing state with other providers.
type Client struct {
	httpClients map[string]*http.Client // key = provider type (e.g. "openai", "anthropic")
	adapters    map[string]Adapter
	defaultOpts ClientOpts
}

// ClientOpts controls the default Transport settings applied to every new
// per-provider HTTP client created by buildHTTPClient.
type ClientOpts struct {
	MaxIdleConns        int
	MaxIdleConnsPerHost int
	MaxConnsPerHost     int
	IdleConnTimeout     time.Duration
	TLSHandshakeTimeout time.Duration
	Timeout             time.Duration
}

// defaultClientOpts returns the out-of-the-box settings matching
// http.DefaultTransport so behaviour is unchanged for callers that do not
// supply custom options.
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

// NewClientWithOpts is the same as NewClient but applies the supplied options
// to every per-provider HTTP client.
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

// HTTPClient returns the *http.Client for the given provider type.  If the
// type is unknown the "openai" client is returned as a safe fallback because
// most third-party providers expose an OpenAI-compatible REST API.
func (c *Client) HTTPClient(providerType string) *http.Client {
	if cl, ok := c.httpClients[providerType]; ok {
		return cl
	}
	if cl, ok := c.httpClients["openai"]; ok {
		return cl
	}
	return http.DefaultClient
}

// SetHTTPClient replaces the *http.Client for a specific provider type.  This
// is used by the gateway to wrap the Transport with oTel tracing after the
// Client has been constructed, without affecting other providers.
func (c *Client) SetHTTPClient(providerType string, cl *http.Client) {
	c.httpClients[providerType] = cl
}

// RegisteredTypes returns the set of provider types that have their own pool.
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
