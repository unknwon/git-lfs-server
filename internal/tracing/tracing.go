// Package tracing sets up OpenTelemetry tracing for the process.
package tracing

import (
	"context"

	"github.com/cockroachdb/errors"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

// Config holds the [tracing] section of the config.
type Config struct {
	Enabled bool `ini:"ENABLED"`
	// Endpoint is the URL of an OTLP/HTTP traces endpoint. Empty falls back to
	// the OTEL_EXPORTER_OTLP_* environment variables.
	Endpoint string `ini:"ENDPOINT"`
	// SampleRatio is the fraction of new traces to sample. Requests that carry
	// a sampled trace context are always sampled.
	SampleRatio float64 `ini:"SAMPLE_RATIO"`
}

// Setup installs the global tracer provider and propagator when tracing is
// enabled, and returns the function that flushes and stops them. Instrumented
// code uses the global provider, so nothing is recorded when tracing is
// disabled.
func Setup(ctx context.Context, cfg Config, serviceVersion string) (shutdown func(context.Context) error, err error) {
	if !cfg.Enabled {
		return func(context.Context) error { return nil }, nil
	}

	var opts []otlptracehttp.Option
	if cfg.Endpoint != "" {
		opts = append(opts, otlptracehttp.WithEndpointURL(cfg.Endpoint))
	}
	exporter, err := otlptracehttp.New(ctx, opts...)
	if err != nil {
		return nil, errors.Wrap(err, "create OTLP trace exporter")
	}

	res, err := resource.New(ctx,
		resource.WithTelemetrySDK(),
		resource.WithAttributes(
			semconv.ServiceName("lfsd"),
			semconv.ServiceVersion(serviceVersion),
		),
		// Applied last so that OTEL_SERVICE_NAME and OTEL_RESOURCE_ATTRIBUTES
		// override the defaults above.
		resource.WithFromEnv(),
	)
	if err != nil {
		return nil, errors.Wrap(err, "create resource")
	}

	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.SampleRatio))),
	)
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	return func(ctx context.Context) error {
		return errors.Wrap(provider.Shutdown(ctx), "shut down tracer provider")
	}, nil
}
