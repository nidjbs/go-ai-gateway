# Contributing

1. Install Go 1.26 or newer.
2. Run `gofmt -w $(find . -name '*.go')`.
3. Run `go test ./...`, `go vet ./...`, and `go build ./cmd/gateway`.
4. Keep provider credentials in environment variables; never add secrets to commits or tests.
5. Keep the gateway core independent of database, queue, and warehouse clients.

Changes should include focused tests for new behavior and documentation for user-visible configuration changes.
