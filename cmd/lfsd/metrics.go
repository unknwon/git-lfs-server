package main

import (
	"net/http"

	"github.com/cockroachdb/errors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	otelprometheus "go.opentelemetry.io/otel/exporters/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

// setupMetrics installs the global meter provider and returns the handler of
// the Prometheus endpoint. Metrics are exported to the default Prometheus
// registry, which also collects the Go runtime and process metrics.
func setupMetrics(serviceVersion string) (http.Handler, error) {
	exporter, err := otelprometheus.New()
	if err != nil {
		return nil, errors.Wrap(err, "create Prometheus exporter")
	}
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(exporter),
		sdkmetric.WithResource(resource.NewSchemaless(
			semconv.ServiceName("lfsd"),
			semconv.ServiceVersion(serviceVersion),
		)),
	))
	return promhttp.Handler(), nil
}
