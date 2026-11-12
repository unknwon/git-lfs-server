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
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestTraceRoute(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))

	const route = "/{host}/{**}/info/lfs/objects/{oid}"
	f := flamego.New()
	f.Get(route, traceRoute(route), func(c flamego.Context) {
		c.ResponseWriter().WriteHeader(http.StatusOK)
	})
	handler := otelhttp.NewHandler(f, "lfsd", otelhttp.WithTracerProvider(tp))

	r := httptest.NewRequest(http.MethodGet, "/GitHub.com/Org/Repo/info/lfs/objects/abc", nil)
	handler.ServeHTTP(httptest.NewRecorder(), r)

	spans := recorder.Ended()
	require.Len(t, spans, 1)
	assert.Equal(t, "GET "+route, spans[0].Name())
	assert.Contains(t, spans[0].Attributes(), attribute.String("http.route", route))
	assert.Contains(t, spans[0].Attributes(), attribute.String("lfs.forge.host", "github.com"))
	assert.Contains(t, spans[0].Attributes(), attribute.String("lfs.repo", "github.com/org/repo"))
}
