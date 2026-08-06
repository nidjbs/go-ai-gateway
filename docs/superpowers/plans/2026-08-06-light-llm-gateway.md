# Light LLM Gateway Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build `light-llm-gateway`, a standalone lightweight OpenAI-compatible LLM gateway with alias routing, retry/failover, optional static-token authentication, and future extension interfaces.

**Architecture:** Create a new Go module with a thin HTTP gateway over a generic OpenAI-compatible upstream client. Configuration resolves public alias names to ordered provider candidates; the retry executor retries one candidate before trying the next. Authentication and usage reporting stay behind narrow interfaces with noop implementations so PostgreSQL and Kafka can be added without changing request execution.

**Tech Stack:** Go 1.26, standard `net/http`, `log/slog`, `gopkg.in/yaml.v3`, `github.com/go-chi/chi/v5`, `github.com/cenkalti/backoff/v7`, OpenAI-compatible JSON/SSE APIs, Docker, GitHub Actions.

## Global Constraints

- Create all implementation files under `/Users/huayuanlin/GolandProjects/light-llm-gateway`; do not modify `/Users/huayuanlin/GolandProjects/siu/ai-gateway`.
- The module must not import `github.com/sundayfun/siu/...`, use a local Go `replace`, or require a parent-directory build context.
- Default runtime dependencies are Go and configured upstream providers only; PostgreSQL, Kafka, and ClickHouse are excluded.
- Support `GET /healthz`, `GET /v1/models`, `POST /v1/chat/completions`, and `POST /v1/embeddings`.
- Default authentication is disabled. Static Bearer-token authentication reads its secret only from a named environment variable.
- Retry/failover is allowed only before an SSE response sends its first event. A post-first-event failure is emitted as an SSE error event and is never retried or failed over.
- Usage sink failures must not change the model response.
- Use Apache-2.0 licensing; never include siu branding, private registries, service endpoints, or credentials in source, tests, or documentation.
- Run `gofmt`, `go test ./...`, `go vet ./...`, and `go build ./cmd/gateway`; report results and wait for user acceptance. Do not run git add, commit, push, reset, checkout, stash, restore, clean, or branch commands.

---

## Target File Structure

- `go.mod`: standalone module using only public dependencies.
- `cmd/gateway/main.go`: loads configuration, creates dependencies, runs HTTP server, handles signals.
- `internal/config/config.go`: YAML configuration types, defaults, environment resolution, validation, and alias ordering.
- `internal/config/config_test.go`: configuration defaults and validation tests.
- `internal/apierr/apierr.go`: OpenAI-style JSON error envelope.
- `internal/auth/auth.go`: `Authenticator`, `Principal`, and context helpers.
- `internal/auth/static.go`: noop and static Bearer-token authenticators.
- `internal/auth/static_test.go`: authentication behavior tests.
- `internal/usage/usage.go`: `UsageEvent`, `UsageSink`, and noop sink.
- `internal/provider/types.go`: provider candidate, chat and embedding request/response types, upstream error.
- `internal/provider/client.go`: JSON upstream request transport and error decoding.
- `internal/provider/stream.go`: SSE upstream decoding.
- `internal/provider/client_test.go`: URL, request, and error mapping tests using `httptest`.
- `internal/routing/router.go`: resolves aliases to priority-ordered candidates.
- `internal/routing/router_test.go`: alias and missing-secret tests.
- `internal/retry/executor.go`: retry classification, backoff, blocking and streaming execution.
- `internal/retry/executor_test.go`: retry, failover, and no-post-first-event-retry tests.
- `internal/gateway/server.go`: chi router, middleware, and graceful HTTP server.
- `internal/gateway/handlers.go`: models, chat, embeddings handlers, request validation, response conversion, and usage recording.
- `internal/gateway/server_test.go`: API-level `httptest` coverage.
- `configs/config.example.yaml`: secret-free example configuration.
- `.env.example`: environment variable names only.
- `Dockerfile`, `docker-compose.yaml`, `.dockerignore`: standalone build and local mock-upstream workflow.
- `scripts/mock_upstream.go`, `scripts/e2e.sh`: reproducible local end-to-end verification.
- `README.md`, `docs/architecture.md`, `docs/extending.md`, `CONTRIBUTING.md`, `SECURITY.md`, `CODE_OF_CONDUCT.md`, `LICENSE`: open-source documentation.
- `.github/workflows/ci.yml`: formatting, tests, vet, and build workflow.

### Task 1: Create Standalone Project Skeleton

**Files:**
- Create: `go.mod`
- Create: `.gitignore`
- Create: `.dockerignore`
- Create: `cmd/gateway/main.go`
- Create: `internal/config/config.go`
- Create: `internal/config/config_test.go`
- Create: `configs/config.example.yaml`
- Create: `.env.example`

**Interfaces:**
- Produces: `config.Load(path string) (*config.Config, error)` and `config.Config` used by all later tasks.
- Produces: a binary command `gateway --config <path>` that loads valid configuration and exits cleanly on SIGINT/SIGTERM.

- [ ] **Step 1: Write failing configuration tests**

```go
func TestLoadAppliesDefaults(t *testing.T) {
    path := writeConfig(t, `providers:
  local:
    base_url: http://127.0.0.1:18080/v1
    api_key_env: UPSTREAM_API_KEY
aliases:
  chat:
    provider: local
    model: test-model
`)
    t.Setenv("UPSTREAM_API_KEY", "test-upstream-token")

    cfg, err := Load(path)
    require.NoError(t, err)
    require.Equal(t, "127.0.0.1:8080", cfg.Listen)
    require.Equal(t, "none", cfg.Auth.Mode)
    require.Equal(t, uint(1), cfg.Retry.MaxAttemptsPerProvider)
}
```

- [ ] **Step 2: Run the new test and verify it fails**

Run: `go test ./internal/config -run TestLoadAppliesDefaults -count=1`

Expected: FAIL because `Load` and its configuration types do not exist.

- [ ] **Step 3: Implement a minimal standalone configuration loader**

```go
type Config struct {
    Listen    string              `yaml:"listen"`
    Healthz   string              `yaml:"healthz"`
    Auth      AuthConfig          `yaml:"auth"`
    Providers map[string]Provider `yaml:"providers"`
    Aliases   map[string]Alias    `yaml:"aliases"`
    Retry     RetryConfig         `yaml:"retry"`
    Failover  FailoverConfig      `yaml:"failover"`
}

func Load(path string) (*Config, error) {
    data, err := os.ReadFile(path)
    if err != nil { return nil, fmt.Errorf("read config: %w", err) }
    cfg := Config{Listen: "127.0.0.1:8080", Healthz: "127.0.0.1:8081"}
    if err := yaml.Unmarshal(data, &cfg); err != nil { return nil, fmt.Errorf("parse config: %w", err) }
    cfg.applyDefaults()
    if err := cfg.Validate(); err != nil { return nil, err }
    return &cfg, nil
}
```

Validate host:port values, non-empty providers/aliases, supported provider type `openai`, valid retry values, alias/provider references, and required API key environment variables. Create the example configuration with only `UPSTREAM_API_KEY` as a placeholder environment variable.

- [ ] **Step 4: Implement the process entry point**

```go
func main() {
    var configPath string
    flag.StringVar(&configPath, "config", "configs/config.yaml", "configuration file")
    flag.Parse()

    cfg, err := config.Load(configPath)
    if err != nil { log.Fatal(err) }
    ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
    defer stop()
    if err := gateway.Run(ctx, cfg, slog.Default()); err != nil { log.Fatal(err) }
}
```

Initially add a temporary `gateway.Run` stub returning `nil`; later tasks replace it with the HTTP server.

- [ ] **Step 5: Run focused validation**

Run: `gofmt -w cmd/gateway internal/config && go test ./internal/config -count=1`

Expected: PASS.

### Task 2: Add Authentication and Usage Extension Contracts

**Files:**
- Create: `internal/auth/auth.go`
- Create: `internal/auth/static.go`
- Create: `internal/auth/static_test.go`
- Create: `internal/usage/usage.go`
- Modify: `internal/config/config.go`

**Interfaces:**
- Consumes: `config.AuthConfig{Mode, TokenEnv}`.
- Produces: `auth.Authenticator`, `auth.Principal`, `auth.New(config.AuthConfig) (Authenticator, error)`.
- Produces: `usage.Event`, `usage.Sink`, `usage.NoopSink`.

- [ ] **Step 1: Write failing static-token tests**

```go
func TestStaticAuthenticatorAcceptsOnlyConfiguredBearerToken(t *testing.T) {
    t.Setenv("GATEWAY_TOKEN", "test-token")
    authenticator, err := New(config.AuthConfig{Mode: "static", TokenEnv: "GATEWAY_TOKEN"})
    require.NoError(t, err)

    ok := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
    ok.Header.Set("Authorization", "Bearer test-token")
    _, err = authenticator.Authenticate(ok.Context(), ok)
    require.NoError(t, err)

    denied := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
    denied.Header.Set("Authorization", "Bearer wrong")
    _, err = authenticator.Authenticate(denied.Context(), denied)
    require.ErrorIs(t, err, ErrUnauthorized)
}
```

- [ ] **Step 2: Run the test and verify it fails**

Run: `go test ./internal/auth -run TestStaticAuthenticatorAcceptsOnlyConfiguredBearerToken -count=1`

Expected: FAIL because the auth package does not exist.

- [ ] **Step 3: Implement authentication interfaces and built-ins**

```go
type Principal struct { Subject string }
type Authenticator interface {
    Authenticate(context.Context, *http.Request) (Principal, error)
}
var ErrUnauthorized = errors.New("unauthorized")

type NoopAuthenticator struct{}
func (NoopAuthenticator) Authenticate(ctx context.Context, _ *http.Request) (Principal, error) {
    return Principal{Subject: "anonymous"}, nil
}
```

Implement static authentication using `subtle.ConstantTimeCompare`; accept `Authorization: Bearer <token>` only, reject absent, malformed, and incorrect values with `ErrUnauthorized`. `auth.New` selects `none` or `static` and returns configuration errors for unsupported modes or missing environment variables.

- [ ] **Step 4: Define the non-blocking usage contract**

```go
type Event struct {
    RequestID string
    Endpoint string
    Alias string
    Provider string
    UpstreamModel string
    StatusCode int
    StartedAt time.Time
    CompletedAt time.Time
    InputTokens int
    OutputTokens int
    Streaming bool
}
type Sink interface { Record(context.Context, Event) error }
type NoopSink struct{}
func (NoopSink) Record(context.Context, Event) error { return nil }
```

Do not add database, Kafka, or HTTP sink implementations.

- [ ] **Step 5: Run focused validation**

Run: `gofmt -w internal/auth internal/usage internal/config && go test ./internal/auth ./internal/config -count=1`

Expected: PASS.

### Task 3: Implement Alias Routing and Provider Resolution

**Files:**
- Create: `internal/routing/router.go`
- Create: `internal/routing/router_test.go`
- Modify: `internal/config/config.go`

**Interfaces:**
- Consumes: `config.Config.Providers` and `config.Config.Aliases`.
- Produces: `routing.Candidate{Name, Model, BaseURL, APIKey, Timeout}` and `routing.Resolve(cfg *config.Config, alias string) ([]Candidate, error)`.

- [ ] **Step 1: Write failing resolution and priority-order tests**

```go
func TestResolveSortsProvidersByPriority(t *testing.T) {
    cfg := testConfig(t, map[string]config.Provider{
        "primary": {BaseURL: "http://primary", APIKey: "a"},
        "backup": {BaseURL: "http://backup", APIKey: "b"},
    }, map[string]config.Alias{
        "chat": {Providers: []config.AliasProvider{
            {Name: "backup", Model: "backup-model", Priority: 10},
            {Name: "primary", Model: "primary-model", Priority: 0},
        }},
    })

    candidates, err := Resolve(cfg, "chat")
    require.NoError(t, err)
    require.Equal(t, []string{"primary", "backup"}, []string{candidates[0].Name, candidates[1].Name})
}
```

- [ ] **Step 2: Run the test and verify it fails**

Run: `go test ./internal/routing -run TestResolveSortsProvidersByPriority -count=1`

Expected: FAIL because `Resolve` does not exist.

- [ ] **Step 3: Implement candidate resolution**

```go
type Candidate struct {
    Name string
    Model string
    BaseURL string
    APIKey string
    Timeout time.Duration
}

func Resolve(cfg *config.Config, alias string) ([]Candidate, error) {
    definition, ok := cfg.Aliases[alias]
    if !ok { return nil, fmt.Errorf("alias %q is not defined", alias) }
    // Normalize legacy provider/model into one AliasProvider, resolve each
    // provider secret, and sort stably by ascending priority.
}
```

Resolve `api_key_env` when loading config, with plaintext `api_key` intentionally unsupported to avoid secrets in configuration files. Return errors naming the alias and provider for missing alias, unknown provider, or unavailable secret.

- [ ] **Step 4: Add missing-alias and missing-environment-secret tests**

```go
func TestResolveRejectsMissingAlias(t *testing.T) {
    _, err := Resolve(testConfig(t, nil, nil), "unknown")
    require.EqualError(t, err, `alias "unknown" is not defined`)
}
```

- [ ] **Step 5: Run focused validation**

Run: `gofmt -w internal/routing internal/config && go test ./internal/routing ./internal/config -count=1`

Expected: PASS.

### Task 4: Build the OpenAI-Compatible Upstream Client

**Files:**
- Create: `internal/provider/types.go`
- Create: `internal/provider/client.go`
- Create: `internal/provider/stream.go`
- Create: `internal/provider/client_test.go`

**Interfaces:**
- Consumes: `routing.Candidate`.
- Produces: `provider.Client` with `Chat(context.Context, ChatRequest, routing.Candidate) (ChatResponse, error)`, `Embeddings(context.Context, EmbeddingRequest, routing.Candidate) (EmbeddingResponse, error)`, and `StreamChat(context.Context, ChatRequest, routing.Candidate) (*Stream, error)`.
- Produces: `provider.HTTPError{StatusCode, Message}` used by retry classification.

- [ ] **Step 1: Write failing chat forwarding test**

```go
func TestChatForwardsRequestToOpenAIEndpoint(t *testing.T) {
    upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        require.Equal(t, "/v1/chat/completions", r.URL.Path)
        require.Equal(t, "Bearer upstream-token", r.Header.Get("Authorization"))
        require.JSONEq(t, `{"model":"provider-model","messages":[{"role":"user","content":"hi"}]}`,
            readBody(t, r))
        _, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"provider-model","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`))
    }))
    defer upstream.Close()

    response, err := NewClient().Chat(context.Background(), ChatRequest{Model: "alias", Messages: json.RawMessage(`[{"role":"user","content":"hi"}]`)}, candidate(upstream.URL, "upstream-token", "provider-model"))
    require.NoError(t, err)
    require.Equal(t, "hello", response.Choices[0].Message.Content)
}
```

- [ ] **Step 2: Run the test and verify it fails**

Run: `go test ./internal/provider -run TestChatForwardsRequestToOpenAIEndpoint -count=1`

Expected: FAIL because the provider package does not exist.

- [ ] **Step 3: Implement request transport and error normalization**

Use `net/http` with candidate-specific timeout contexts. Build endpoint URLs so a base URL ending in `/v1` receives `/chat/completions` and a host-only base URL receives `/v1/chat/completions`. Preserve OpenAI-compatible JSON request fields as `json.RawMessage` where the gateway does not need interpretation. Decode upstream non-2xx JSON errors into `HTTPError` without forwarding upstream headers or secrets.

- [ ] **Step 4: Implement embeddings and SSE stream parsing**

Implement `Embeddings` against `POST /v1/embeddings`. Implement `StreamChat` with `Accept: text/event-stream`; scan `data:` frames, decode JSON chunks, report `[DONE]`, and expose stream read errors separately from successful completion. Do not buffer the full stream.

- [ ] **Step 5: Add error and streaming tests**

```go
func TestChatReturnsHTTPError(t *testing.T) {
    upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
        w.WriteHeader(http.StatusTooManyRequests)
        _, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
    }))
    defer upstream.Close()

    _, err := NewClient().Chat(context.Background(), validChatRequest(), candidate(upstream.URL, "token", "model"))
    var httpErr *HTTPError
    require.ErrorAs(t, err, &httpErr)
    require.Equal(t, http.StatusTooManyRequests, httpErr.StatusCode)
    require.Equal(t, "rate limited", httpErr.Message)
}

func TestStreamChatYieldsChunksAndDone(t *testing.T) {
    upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
        w.Header().Set("Content-Type", "text/event-stream")
        _, _ = io.WriteString(w, "data: {\"id\":\"one\",\"choices\":[{\"delta\":{\"content\":\"hel\"}}]}\n\n")
        _, _ = io.WriteString(w, "data: {\"id\":\"one\",\"choices\":[{\"delta\":{\"content\":\"lo\"}}]}\n\n")
        _, _ = io.WriteString(w, "data: [DONE]\n\n")
    }))
    defer upstream.Close()

    stream, err := NewClient().StreamChat(context.Background(), validChatRequest(), candidate(upstream.URL, "token", "model"))
    require.NoError(t, err)
    require.Equal(t, "hel", (<-stream.Events).Choices[0].Delta.Content)
    require.Equal(t, "lo", (<-stream.Events).Choices[0].Delta.Content)
    require.NoError(t, <-stream.Done)
}
```

- [ ] **Step 6: Run focused validation**

Run: `gofmt -w internal/provider && go test ./internal/provider -count=1`

Expected: PASS.

### Task 5: Implement Retry and Failover Execution

**Files:**
- Create: `internal/retry/executor.go`
- Create: `internal/retry/executor_test.go`

**Interfaces:**
- Consumes: `config.RetryConfig`, `config.FailoverConfig`, `routing.Candidate`, and `provider.HTTPError`.
- Produces: `retry.Execute[T any](...) (T, routing.Candidate, Attempts, error)` and `retry.ExecuteStream(...) StreamResult`.

- [ ] **Step 1: Write failing blocking failover test**

```go
func TestExecuteRetriesThenFailsOver(t *testing.T) {
    candidates := []routing.Candidate{{Name: "primary"}, {Name: "backup"}}
    calls := map[string]int{}
    value, selected, attempts, err := Execute(context.Background(), config.RetryConfig{Enabled: true, MaxAttemptsPerProvider: 2}, config.FailoverConfig{Enabled: true}, candidates, func(_ context.Context, candidate routing.Candidate) (string, error) {
        calls[candidate.Name]++
        if candidate.Name == "primary" { return "", &provider.HTTPError{StatusCode: 503} }
        return "ok", nil
    })
    require.NoError(t, err)
    require.Equal(t, "ok", value)
    require.Equal(t, "backup", selected.Name)
    require.Equal(t, 3, attempts.Total)
}
```

- [ ] **Step 2: Run the test and verify it fails**

Run: `go test ./internal/retry -run TestExecuteRetriesThenFailsOver -count=1`

Expected: FAIL because the retry package does not exist.

- [ ] **Step 3: Implement blocking execution**

```go
func Execute[T any](ctx context.Context, retryCfg config.RetryConfig, failoverCfg config.FailoverConfig, candidates []routing.Candidate, attempt func(context.Context, routing.Candidate) (T, error)) (T, routing.Candidate, Attempts, error)
```

Classify retryability as configured HTTP status, timeout, unexpected EOF, or `net.Error`; never retry a canceled request. Apply exponential backoff with defaults: `200ms`, maximum `5s`, multiplier `2`, jitter `0.2`. Honor max attempts, max elapsed time, per-attempt timeout, and max providers.

- [ ] **Step 4: Write failing no-post-first-event-retry stream test**

```go
func TestExecuteStreamDoesNotRetryAfterFirstEvent(t *testing.T) {
    candidates := []routing.Candidate{{Name: "primary"}, {Name: "backup"}}
    opened := map[string]int{}
    written := 0

    result := ExecuteStream(context.Background(), enabledRetry(), enabledFailover(), candidates,
        func(_ context.Context, candidate routing.Candidate) (EventStream, error) {
            opened[candidate.Name]++
            return failingStreamAfterOneEvent(), nil
        },
        func(Event) error { written++; return nil },
    )

    require.Equal(t, 1, written)
    require.Equal(t, 1, opened["primary"])
    require.Zero(t, opened["backup"])
    require.ErrorIs(t, result.LastErr, io.ErrUnexpectedEOF)
}
```

- [ ] **Step 5: Implement stream execution semantics**

Retry/open the next provider only when opening or consuming a stream fails before the first emitted event. Once `write` successfully receives an event, immediately return any later error without retrying or failing over. Return selected provider, token usage supplied by the stream, first-event time, number of attempts, and final error.

- [ ] **Step 6: Run focused validation**

Run: `gofmt -w internal/retry && go test ./internal/retry -count=1`

Expected: PASS.

### Task 6: Build the HTTP Gateway and API Responses

**Files:**
- Create: `internal/apierr/apierr.go`
- Create: `internal/gateway/server.go`
- Create: `internal/gateway/handlers.go`
- Create: `internal/gateway/server_test.go`
- Modify: `cmd/gateway/main.go`

**Interfaces:**
- Consumes: `config.Config`, `auth.Authenticator`, `usage.Sink`, `routing.Resolve`, `provider.Client`, and retry executors.
- Produces: `gateway.New(gateway.Deps) *gateway.Server` and `gateway.Run(context.Context, *config.Config, *slog.Logger) error`.

- [ ] **Step 1: Write failing models and unauthenticated API tests**

```go
func TestModelsListsAliasesInSortedOrder(t *testing.T) {
    server := newTestServer(t, testConfigWithAliases("zeta", "alpha"), auth.NoopAuthenticator{})
    response := httptest.NewRecorder()
    server.HTTP.Handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
    require.Equal(t, http.StatusOK, response.Code)
    require.JSONEq(t, `{"object":"list","data":[{"id":"alpha","object":"model","owned_by":"light-llm-gateway"},{"id":"zeta","object":"model","owned_by":"light-llm-gateway"}]}`, normalizeCreated(t, response.Body.Bytes()))
}

func TestStaticAuthenticationRejectsAbsentToken(t *testing.T) {
    server := newTestServer(t, testConfig(t), staticAuthenticator(t))
    response := httptest.NewRecorder()
    server.HTTP.Handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
    require.Equal(t, http.StatusUnauthorized, response.Code)
}
```

- [ ] **Step 2: Run the tests and verify they fail**

Run: `go test ./internal/gateway -run 'TestModelsListsAliasesInSortedOrder|TestStaticAuthenticationRejectsAbsentToken' -count=1`

Expected: FAIL because the gateway package does not exist.

- [ ] **Step 3: Implement routing, health checks, request IDs, and OpenAI errors**

Define error serialization as:

```go
type Body struct { Error Detail `json:"error"` }
type Detail struct {
    Message string `json:"message"`
    Type string `json:"type"`
    Code string `json:"code,omitempty"`
    Param string `json:"param,omitempty"`
}
```

Configure chi middleware for request ID, real IP, panic recovery, and structured completion logging. Register unauthenticated `GET /healthz`; protect all `/v1/*` routes through the injected authenticator. Return 401 `invalid_api_key` for `auth.ErrUnauthorized`, and never log Authorization values.

- [ ] **Step 4: Write failing blocking chat and embeddings tests**

```go
func TestChatUsesAliasAndReturnsOpenAIResponse(t *testing.T) {
    upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        require.Equal(t, "/v1/chat/completions", r.URL.Path)
        var body map[string]any
        require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
        require.Equal(t, "provider-chat", body["model"])
        _, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"provider-chat","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`))
    }))
    defer upstream.Close()

    server := newTestServer(t, configForUpstream(t, upstream.URL), auth.NoopAuthenticator{})
    response := httptest.NewRecorder()
    request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"chat-alias","messages":[{"role":"user","content":"hi"}]}`))
    request.Header.Set("Content-Type", "application/json")
    server.HTTP.Handler.ServeHTTP(response, request)

    require.Equal(t, http.StatusOK, response.Code)
    require.JSONEq(t, `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"chat-alias","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`, response.Body.String())
}

func TestEmbeddingsAcceptsStringOrStringArray(t *testing.T) {
    server, calls := embeddingTestServer(t)
    for _, payload := range []string{
        `{"model":"embedding-alias","input":"one"}`,
        `{"model":"embedding-alias","input":["one","two"]}`,
    } {
        response := httptest.NewRecorder()
        request := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(payload))
        server.HTTP.Handler.ServeHTTP(response, request)
        require.Equal(t, http.StatusOK, response.Code)
    }
    require.Equal(t, 2, *calls)

    response := httptest.NewRecorder()
    request := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(`{"model":"embedding-alias","input":[]}`))
    server.HTTP.Handler.ServeHTTP(response, request)
    require.Equal(t, http.StatusBadRequest, response.Code)
}
```

- [ ] **Step 5: Implement chat and embeddings handlers**

Limit request bodies to `1 MiB`. Validate `model`, chat `messages`, and embedding input. Resolve aliases, call the retry executor using the provider client, and send OpenAI-compatible response JSON. Replace client-facing response `model` with the requested alias; retain upstream model in usage event and logs. Map upstream failures to 502 `upstream_error`; malformed client input to 400 `invalid_request`.

- [ ] **Step 6: Write failing streaming behavior tests**

```go
func TestStreamingChatWritesSSEFramesAndDone(t *testing.T) {
    server := streamingTestServer(t, false)
    response := doStreamingRequest(t, server)
    require.Equal(t, http.StatusOK, response.Code)
    require.Contains(t, response.Header().Get("Content-Type"), "text/event-stream")
    require.Contains(t, response.Body.String(), "data: [DONE]")
}

func TestStreamingChatWritesSSEErrorAfterFirstFrame(t *testing.T) {
    server := streamingTestServer(t, true)
    response := doStreamingRequest(t, server)
    require.Equal(t, http.StatusOK, response.Code)
    require.Contains(t, response.Body.String(), `"error"`)
}
```

- [ ] **Step 7: Implement streaming handler**

Set SSE headers only immediately before the first valid frame. Forward OpenAI-shaped chunk JSON, flush each event, and emit `data: [DONE]\n\n` on clean completion. If an error occurs after an emitted frame, emit one `data: {"error":...}\n\n` SSE event. If no frame has been emitted, return standard JSON 502 instead.

- [ ] **Step 8: Record usage without affecting responses**

After every chat and embedding outcome, call `UsageSink.Record` using `context.WithoutCancel` and a one-second timeout. Log a failed sink call with request ID and endpoint but do not alter the response status or body. Include request ID, endpoint, alias, selected provider, upstream model, status, times, tokens, and stream flag.

- [ ] **Step 9: Run focused validation**

Run: `gofmt -w internal/apierr internal/gateway cmd/gateway && go test ./internal/gateway -count=1`

Expected: PASS.

### Task 7: Add Observability and Independent Process Lifecycle Tests

**Files:**
- Modify: `internal/gateway/server.go`
- Modify: `internal/gateway/handlers.go`
- Create: `internal/gateway/observability_test.go`
- Modify: `cmd/gateway/main.go`

**Interfaces:**
- Consumes: request context and retry `Attempts` metadata.
- Produces: structured logs with `request_id`, `endpoint`, `provider`, `attempts`, `upstream_duration_ms`, and final `status` fields.

- [ ] **Step 1: Write failing log-field test**

```go
func TestCompletionLogIncludesRoutingFields(t *testing.T) {
    handler, records := serverWithCaptureLogger(t)
    handler.ServeHTTP(httptest.NewRecorder(), validChatRequest(t))
    require.Equal(t, "chat.completions", records[0].Attrs["endpoint"])
    require.NotEmpty(t, records[0].Attrs["request_id"])
    require.Equal(t, "primary", records[0].Attrs["provider"])
}
```

- [ ] **Step 2: Run the test and verify it fails**

Run: `go test ./internal/gateway -run TestCompletionLogIncludesRoutingFields -count=1`

Expected: FAIL because the completion log lacks these fields.

- [ ] **Step 3: Add request-scoped structured logging**

Use `log/slog` only. Log one completion record per request with sanitized path/method/status and routing metadata. Do not log bodies, Authorization headers, provider API keys, or user prompts. Ensure graceful shutdown uses a five-second deadline.

- [ ] **Step 4: Run focused validation**

Run: `gofmt -w internal/gateway cmd/gateway && go test ./internal/gateway -count=1`

Expected: PASS.

### Task 8: Add Standalone Local E2E Environment

**Files:**
- Create: `scripts/mock_upstream.go`
- Create: `scripts/e2e.sh`
- Create: `Dockerfile`
- Create: `docker-compose.yaml`
- Modify: `.dockerignore`

**Interfaces:**
- Consumes: `configs/config.example.yaml` and environment variables `UPSTREAM_API_KEY`, `GATEWAY_STATIC_TOKEN`.
- Produces: `go run ./scripts/mock_upstream.go`, `./scripts/e2e.sh`, and a Docker image buildable from repository root.

- [ ] **Step 1: Write an E2E script that expects a built gateway**

```bash
#!/usr/bin/env bash
set -euo pipefail
curl --fail --silent http://127.0.0.1:18080/healthz >/dev/null
curl --fail --silent http://127.0.0.1:18080/v1/models \
  -H "Authorization: Bearer ${GATEWAY_STATIC_TOKEN}" | grep -q 'local-chat'
curl --fail --silent http://127.0.0.1:18080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -H "Authorization: Bearer ${GATEWAY_STATIC_TOKEN}" \
  --data '{"model":"local-chat","messages":[{"role":"user","content":"ping"}]}' | grep -q 'mock response'
```

- [ ] **Step 2: Run the script and verify it initially fails**

Run: `./scripts/e2e.sh`

Expected: FAIL because the mock upstream and gateway process are not running.

- [ ] **Step 3: Implement a local mock upstream**

Provide `GET /v1/models`, successful and streaming `POST /v1/chat/completions`, and `POST /v1/embeddings`. Support a request header such as `X-Mock-Fail: 503` to exercise failover in manual tests. The mock must bind to `127.0.0.1:19090` by default and contain no external provider names or tokens.

- [ ] **Step 4: Create standalone container assets**

Use a public Go builder image and a public distroless or Alpine runtime image. Copy only the new repository context. The compose file starts the gateway and mock upstream only; it must not include PostgreSQL, Kafka, or ClickHouse.

```dockerfile
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/gateway ./cmd/gateway

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/gateway /usr/local/bin/gateway
ENTRYPOINT ["/usr/local/bin/gateway"]
```

- [ ] **Step 5: Run E2E and image build**

Run: `go run ./scripts/mock_upstream.go` in one terminal, `UPSTREAM_API_KEY=test GATEWAY_STATIC_TOKEN=test go run ./cmd/gateway --config configs/config.example.yaml` in another, then `GATEWAY_STATIC_TOKEN=test ./scripts/e2e.sh`.

Expected: health, models, chat, embeddings, retry/failover, and SSE probes PASS.

Run: `docker build -t light-llm-gateway:local .`

Expected: image build succeeds without parent-directory files.

### Task 9: Write Open-Source Documentation and Governance Files

**Files:**
- Create: `README.md`
- Create: `docs/architecture.md`
- Create: `docs/extending.md`
- Create: `CONTRIBUTING.md`
- Create: `SECURITY.md`
- Create: `CODE_OF_CONDUCT.md`
- Create: `LICENSE`

**Interfaces:**
- Documents: the implemented configuration schema, API compatibility boundary, retry/failover semantics, auth and usage contracts, development workflow, and security reporting process.

- [ ] **Step 1: Write README quick start against the actual example files**

Include exact commands:

```bash
cp configs/config.example.yaml configs/config.yaml
export UPSTREAM_API_KEY=replace-me
export GATEWAY_STATIC_TOKEN=replace-me
go run ./cmd/gateway --config configs/config.yaml
```

Document `auth.mode: none` and `auth.mode: static`; explain that production secrets use environment variables. Show chat, models, and streaming curl examples. State clearly that PostgreSQL, Kafka, and ClickHouse are not required.

- [ ] **Step 2: Write architecture and extension documentation**

`docs/architecture.md` must include the request flow:

```text
client -> auth -> alias resolver -> retry/failover -> OpenAI-compatible upstream
                           \-> usage sink (best effort after result)
```

`docs/extending.md` must reproduce the exact `Authenticator` and `UsageSink` signatures and explain that a custom sink must tolerate being called after the HTTP response is committed.

- [ ] **Step 3: Add governance files**

Use the full Apache License 2.0 text. Write concise contribution guidance requiring tests and `go vet`. Security guidance must instruct reporters not to include secrets or public exploit details and use a placeholder contact format `security@example.com` marked for replacement before publication. Use Contributor Covenant 2.1 for `CODE_OF_CONDUCT.md`.

- [ ] **Step 4: Search documentation and source for forbidden references**

Run: `rg -n -i '(github\.com/sundayfun/siu|siu-ai-gateway|siuper|aliyuncs|goproxy\.cn|postgres|kafka|clickhouse|sk-[A-Za-z0-9_-]{8,})' .`

Expected: no siu/private registry/service/token matches; permitted explanatory references to PostgreSQL/Kafka/ClickHouse occur only in README/architecture as explicitly excluded future extensions.

### Task 10: Add CI and Complete Independent Verification

**Files:**
- Create: `.github/workflows/ci.yml`
- Modify: `README.md` if validation exposes an inaccurate command.

**Interfaces:**
- Produces: CI jobs for formatting check, tests, vet, build, and Docker build from repository root.

- [ ] **Step 1: Add GitHub Actions workflow**

```yaml
name: ci
on: [push, pull_request]
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with: { go-version: '1.26.x' }
      - run: test -z "$(gofmt -l .)"
      - run: go test ./...
      - run: go vet ./...
      - run: go build ./cmd/gateway
      - run: docker build -t light-llm-gateway:ci .
```

- [ ] **Step 2: Verify no local module replacement or siu import remains**

Run: `rg -n '^(replace|module )|github\.com/sundayfun/siu' go.mod go.sum . --glob '*.go'`

Expected: module declaration uses a temporary neutral module path such as `example.com/light-llm-gateway`; no `replace` directive and no siu import.

- [ ] **Step 3: Run complete static verification**

Run: `gofmt -w $(find . -name '*.go' -not -path './.git/*') && go mod tidy && go test ./... && go vet ./... && go build ./cmd/gateway`

Expected: all commands exit 0.

- [ ] **Step 4: Run Docker and local E2E verification**

Run the mock and gateway processes using the documented commands, then run `./scripts/e2e.sh` with static-token auth enabled. Run `docker build -t light-llm-gateway:local .`.

Expected: all E2E probes and standalone Docker build succeed.

- [ ] **Step 5: Report implementation and verification status**

Report created files, removed dependencies compared with the siu gateway, all commands run and their results, and any validation that could not be run. Leave changes uncommitted for user acceptance.
