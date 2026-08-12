ARG GO_VERSION=1.26

FROM golang:${GO_VERSION} AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/gateway ./cmd/gateway
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/mock ./scripts/mock_upstream.go

FROM gcr.io/distroless/static-debian13:nonroot AS gateway
COPY --from=builder /out/gateway /usr/local/bin/gateway
ENTRYPOINT ["/usr/local/bin/gateway"]

FROM gcr.io/distroless/static-debian13:nonroot AS mock
COPY --from=builder /out/mock /usr/local/bin/mock
EXPOSE 19090
ENTRYPOINT ["/usr/local/bin/mock"]