FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/gateway ./cmd/gateway

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/gateway /usr/local/bin/gateway
ENTRYPOINT ["/usr/local/bin/gateway"]
