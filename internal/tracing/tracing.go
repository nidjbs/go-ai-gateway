// Package tracing wires the OpenTelemetry tracing pipeline used by the
// gateway. It installs the global TracerProvider, registers the W3C
// TraceContext propagator (so traceparent headers flow in both directions)
// and returns a shutdown hook that flushes the exporter on graceful
// termination.
//
// The package is a no-op when TracingConfig.Enabled is false: it returns a
// nil shutdown and leaves the global TracerProvider as the SDK default (a
// non-recording provider), so call sites can run tracer.Start unconditionally
// without paying any cost.
package tracing

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.27.0"

	"example.com/light-llm-gateway/internal/config"
)

// ShutdownFunc is the cleanup hook returned by Init. Callers should defer it
// during gateway startup; a nil ShutdownFunc is a valid no-op.
type ShutdownFunc func(context.Context) error

// Init configures the global TracerProvider from cfg. When Enabled is false
// it still installs the W3C propagator so downstream code can emit
// traceparent headers, and returns a nil shutdown.
func Init(ctx context.Context, cfg config.TracingConfig) (ShutdownFunc, error) {
	// Always install the W3C propagator. The OTel default propagator is a
	// noop so without this outbound HTTP would never inject traceparent.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	if !cfg.Enabled {
		return nil, nil
	}

	endpoint := cfg.Endpoint
	if endpoint == "" {
		endpoint = "localhost:4318"
	}
	sampleRatio := cfg.SampleRatio
	if sampleRatio <= 0 || sampleRatio > 1 {
		sampleRatio = 1.0
	}
	serviceName := cfg.ServiceName
	if serviceName == "" {
		serviceName = "ai-gateway"
	}

	opts := []otlptracehttp.Option{
		otlptracehttp.WithEndpoint(endpoint),
		otlptracehttp.WithTimeout(10 * time.Second),
	}
	if cfg.Insecure {
		opts = append(opts, otlptracehttp.WithInsecure())
	}
	exporter, err := otlptracehttp.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("init otlp trace exporter: %w", err)
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(serviceName),
		),
	)
	if err != nil {
		// Resource creation can fail on unsupported environments; we fall
		// back to the default resource rather than failing startup.
		res = resource.Default()
	}

	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.TraceIDRatioBased(sampleRatio)),
	)
	otel.SetTracerProvider(provider)
	return func(shutdownCtx context.Context) error {
		return errors.Join(provider.Shutdown(shutdownCtx), exporter.Shutdown(shutdownCtx))
	}, nil
}