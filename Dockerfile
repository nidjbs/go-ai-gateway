ARG GO_VERSION=1.26
ARG VERSION=dev
ARG GIT_COMMIT=unknown
ARG BUILD_DATE=unknown

FROM golang:${GO_VERSION}-alpine AS builder
RUN apk add --no-cache git
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w \
  -X github.com/nidjbs/go-ai-gateway/internal/version.Version=${VERSION} \
  -X github.com/nidjbs/go-ai-gateway/internal/version.Commit=${GIT_COMMIT} \
  -X github.com/nidjbs/go-ai-gateway/internal/version.BuildDate=${BUILD_DATE}" \
  -o /out/gateway ./cmd/gateway
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/mock ./scripts/mock_upstream.go

FROM gcr.io/distroless/static-debian13:nonroot AS gateway
COPY --from=builder /out/gateway /usr/local/bin/gateway
ENTRYPOINT ["/usr/local/bin/gateway"]

FROM gcr.io/distroless/static-debian13:nonroot AS mock
COPY --from=builder /out/mock /usr/local/bin/mock
EXPOSE 19090
ENTRYPOINT ["/usr/local/bin/mock"]