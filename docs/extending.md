# Extending

The core uses narrow interfaces for optional deployment-specific capabilities.

```go
type Authenticator interface {
    Authenticate(context.Context, *http.Request) (Principal, error)
}
```

Implement this interface for multi-tenant API keys, external identity providers, or a database-backed policy. Return `auth.ErrUnauthorized` for credentials that must produce a 401 response.

```go
type Sink interface {
    Record(context.Context, Event) error
}
```

Usage recording is best effort and can run after the HTTP response is committed. A custom sink must respect its context deadline and must not assume it can change the client response. This provides an integration point for queues, HTTP collectors, or persistent analytics storage without making them dependencies of the gateway core.
