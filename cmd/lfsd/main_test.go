package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/flamego/flamego"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestInstrumentRoute(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	reader := sdkmetric.NewManualReader()

	const route = "/{host}/{**}/info/lfs/objects/{oid}"
	f := flamego.New()
	f.Get(route, instrumentRoute(route), func(c flamego.Context) {
		c.ResponseWriter().WriteHeader(http.StatusOK)
	})
	handler := otelhttp.NewHandler(f, "lfsd",
		otelhttp.WithTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))),
		otelhttp.WithMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))),
	)

	r := httptest.NewRequest(http.MethodGet, "/GitHub.com/Org/Repo/info/lfs/objects/abc", nil)
	handler.ServeHTTP(httptest.NewRecorder(), r)

	spans := recorder.Ended()
	require.Len(t, spans, 1)
	assert.Equal(t, "GET "+route, spans[0].Name())
	assert.Contains(t, spans[0].Attributes(), attribute.String("http.route", route))
	assert.Contains(t, spans[0].Attributes(), attribute.String("lfs.forge.host", "github.com"))
	assert.Contains(t, spans[0].Attributes(), attribute.String("lfs.repo", "github.com/org/repo"))

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(t.Context(), &rm))
	var routes []string
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "http.server.request.duration" {
				continue
			}
			for _, dp := range m.Data.(metricdata.Histogram[float64]).DataPoints {
				route, _ := dp.Attributes.Value("http.route")
				routes = append(routes, route.AsString())
			}
		}
	}
	assert.Equal(t, []string{route}, routes, "the request duration must be labeled with the route")
}
