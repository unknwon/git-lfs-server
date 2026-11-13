package logx

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestTraceHandler(t *testing.T) {
	newLogger := func() (*Logger, *bytes.Buffer) {
		var buf bytes.Buffer
		return New(NewTraceHandler(slog.NewJSONHandler(&buf, nil))).Scoped("test"), &buf
	}
	decode := func(t *testing.T, buf *bytes.Buffer) map[string]any {
		t.Helper()
		var record map[string]any
		require.NoError(t, json.Unmarshal(buf.Bytes(), &record))
		return record
	}

	t.Run("records outside of a span are unchanged", func(t *testing.T) {
		logger, buf := newLogger()
		logger.InfoContext(t.Context(), "Hello")
		record := decode(t, buf)
		assert.NotContains(t, record, "traceID")
		assert.NotContains(t, record, "spanID")
	})

	t.Run("records in a span carry its IDs and only errors fail it", func(t *testing.T) {
		recorder := tracetest.NewSpanRecorder()
		tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
		ctx, span := tp.Tracer("test").Start(t.Context(), "span")

		logger, buf := newLogger()
		logger.WarnContext(ctx, "Slow")
		record := decode(t, buf)
		assert.Equal(t, span.SpanContext().TraceID().String(), record["traceID"])
		assert.Equal(t, span.SpanContext().SpanID().String(), record["spanID"])

		logger.ErrorContext(ctx, "Failed", "error", "boom")
		span.End()

		ended := recorder.Ended()
		require.Len(t, ended, 1)
		assert.Equal(t, codes.Error, ended[0].Status().Code)
		assert.Equal(t, "[test] Failed", ended[0].Status().Description)
		events := ended[0].Events()
		require.Len(t, events, 1, "only the error record becomes an event")
		assert.Equal(t, "[test] Failed", events[0].Name)
		assert.Equal(t, "boom", events[0].Attributes[0].Value.AsString())
	})
}
