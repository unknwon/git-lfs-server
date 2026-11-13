package logx

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// traceHandler wraps a slog.Handler to correlate records with the span in their
// context. Records get the trace and span IDs as attributes, and error records
// are added to the span as events and fail it, so a trace shows why it failed
// without searching the logs.
type traceHandler struct {
	handler slog.Handler
}

// NewTraceHandler returns a handler that correlates records with spans before
// passing them to handler.
func NewTraceHandler(handler slog.Handler) slog.Handler {
	return &traceHandler{handler: handler}
}

func (h *traceHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.handler.Enabled(ctx, level)
}

func (h *traceHandler) Handle(ctx context.Context, r slog.Record) error {
	span := trace.SpanFromContext(ctx)
	spanContext := span.SpanContext()
	if !spanContext.IsValid() {
		return h.handler.Handle(ctx, r)
	}

	if r.Level >= slog.LevelError && span.IsRecording() {
		attrs := make([]attribute.KeyValue, 0, r.NumAttrs())
		r.Attrs(func(a slog.Attr) bool {
			attrs = append(attrs, attribute.String(a.Key, a.Value.String()))
			return true
		})
		span.AddEvent(r.Message, trace.WithAttributes(attrs...))
		span.SetStatus(codes.Error, r.Message)
	}

	r = r.Clone()
	r.AddAttrs(
		slog.String("traceID", spanContext.TraceID().String()),
		slog.String("spanID", spanContext.SpanID().String()),
	)
	return h.handler.Handle(ctx, r)
}

func (h *traceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &traceHandler{handler: h.handler.WithAttrs(attrs)}
}

func (h *traceHandler) WithGroup(name string) slog.Handler {
	return &traceHandler{handler: h.handler.WithGroup(name)}
}
