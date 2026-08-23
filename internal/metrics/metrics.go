package metrics

import (
	"context"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel/attribute"
	prometheusexporter "go.opentelemetry.io/otel/exporters/prometheus"
	apimetric "go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

const instrumentationName = "github.com/nidjbs/go-ai-gateway/internal/metrics"

var (
	llmRequestDurationBuckets    = []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120}
	ttftBuckets                  = []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}
	llmTimePerOutputTokenBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5}
	tokenUsageBuckets            = []float64{1, 4, 16, 64, 256, 1024, 4096, 16384, 65536, 262144}
)

type Request struct {
	Operation     string
	Provider      string
	Model         string
	UpstreamModel string
	APIKeyID      string
	TeamID        string
	StartedAt     time.Time
}

type Result struct {
	StatusCode          int
	ErrorType           string
	ResponseModel       string
	InputTokens         int
	OutputTokens        int
	CacheReadTokens     int
	CacheCreationTokens int
	FirstTokenAt        time.Time
	CompletedAt         time.Time
}

type Recorder struct {
	handler            http.Handler
	requestDuration    apimetric.Float64Histogram
	timeToFirstToken   apimetric.Float64Histogram
	timePerOutputToken apimetric.Float64Histogram
	tokenUsage         apimetric.Int64Histogram
	dlpDetections      apimetric.Int64Counter
}

func New() (*Recorder, error) {
	registry := prometheus.NewRegistry()
	exporter, err := prometheusexporter.New(prometheusexporter.WithRegisterer(registry))
	if err != nil {
		return nil, err
	}
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(exporter))
	meter := provider.Meter(instrumentationName)
	return newRecorder(meter, promhttp.HandlerFor(registry, promhttp.HandlerOpts{Registry: registry})), nil
}

func (r *Recorder) Handler() http.Handler { return r.handler }

// RecordDLP counts a DLP detection, labelled by pattern and mode.
func (r *Recorder) RecordDLP(ctx context.Context, pattern, mode string) {
	if r.dlpDetections == nil {
		return
	}
	r.dlpDetections.Add(ctx, 1, apimetric.WithAttributes(
		attribute.String("pattern", pattern),
		attribute.String("mode", mode),
	))
}

func (r *Recorder) Record(ctx context.Context, request Request, result Result) {
	if request.StartedAt.IsZero() || result.CompletedAt.IsZero() || result.CompletedAt.Before(request.StartedAt) {
		return
	}
	success := result.ErrorType == "" && result.StatusCode >= http.StatusOK && result.StatusCode < http.StatusMultipleChoices
	attrs := llmAttributes(request, result, success)
	r.requestDuration.Record(ctx, result.CompletedAt.Sub(request.StartedAt).Seconds(), apimetric.WithAttributes(attrs...))
	if !success {
		return
	}
	if result.InputTokens > 0 {
		r.tokenUsage.Record(ctx, int64(result.InputTokens), apimetric.WithAttributes(append(attrs, attribute.String("token_type", "input"))...))
	}
	if result.OutputTokens > 0 {
		r.tokenUsage.Record(ctx, int64(result.OutputTokens), apimetric.WithAttributes(append(attrs, attribute.String("token_type", "output"))...))
	}
	if result.CacheReadTokens > 0 {
		r.tokenUsage.Record(ctx, int64(result.CacheReadTokens), apimetric.WithAttributes(append(attrs, attribute.String("token_type", "cache_read"))...))
	}
	if result.CacheCreationTokens > 0 {
		r.tokenUsage.Record(ctx, int64(result.CacheCreationTokens), apimetric.WithAttributes(append(attrs, attribute.String("token_type", "cache_creation"))...))
	}
	if result.FirstTokenAt.IsZero() || result.FirstTokenAt.Before(request.StartedAt) || result.FirstTokenAt.After(result.CompletedAt) {
		return
	}
	r.timeToFirstToken.Record(ctx, result.FirstTokenAt.Sub(request.StartedAt).Seconds(), apimetric.WithAttributes(attrs...))
	if result.OutputTokens > 1 {
		r.timePerOutputToken.Record(ctx, result.CompletedAt.Sub(result.FirstTokenAt).Seconds()/float64(result.OutputTokens-1), apimetric.WithAttributes(attrs...))
	}
}

func newRecorder(meter apimetric.Meter, handler http.Handler) *Recorder {
	return &Recorder{
		handler:            handler,
		requestDuration:    must(meter.Float64Histogram("ai_gateway.llm.request.duration", apimetric.WithUnit("s"), apimetric.WithExplicitBucketBoundaries(llmRequestDurationBuckets...))),
		timeToFirstToken:   must(meter.Float64Histogram("ai_gateway.llm.time_to_first_token", apimetric.WithUnit("s"), apimetric.WithExplicitBucketBoundaries(ttftBuckets...))),
		timePerOutputToken: must(meter.Float64Histogram("ai_gateway.llm.time_per_output_token", apimetric.WithUnit("s"), apimetric.WithExplicitBucketBoundaries(llmTimePerOutputTokenBuckets...))),
		tokenUsage:         must(meter.Int64Histogram("ai_gateway.llm.token.usage", apimetric.WithUnit("token"), apimetric.WithExplicitBucketBoundaries(tokenUsageBuckets...))),
		dlpDetections:      must(meter.Int64Counter("ai_gateway.dlp.detections")),
	}
}

func llmAttributes(request Request, result Result, success bool) []attribute.KeyValue {
	responseModel := result.ResponseModel
	if responseModel == "" {
		responseModel = "unknown"
	}
	attrs := []attribute.KeyValue{
		attribute.String("operation", request.Operation),
		attribute.String("provider", request.Provider),
		attribute.String("model", request.Model),
		attribute.String("upstream_model", request.UpstreamModel),
		attribute.String("response_model", responseModel),
		attribute.Bool("success", success),
	}
	if request.APIKeyID != "" {
		attrs = append(attrs, attribute.String("api_key_id", request.APIKeyID))
	}
	if request.TeamID != "" {
		attrs = append(attrs, attribute.String("team_id", request.TeamID))
	}
	if result.ErrorType != "" {
		attrs = append(attrs, attribute.String("error.type", result.ErrorType))
	}
	return attrs
}

func must[T any](value T, err error) T {
	if err != nil {
		panic(err)
	}
	return value
}
