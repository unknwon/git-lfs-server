package lfs

import (
	"context"

	"github.com/cockroachdb/errors"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"unknwon.dev/git-lfs-server/lfs/storage"
)

const instrumentationName = "unknwon.dev/git-lfs-server/lfs"

func (s *Service) startSpan(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	tp := s.TracerProvider
	if tp == nil {
		tp = otel.GetTracerProvider()
	}
	return tp.Tracer(instrumentationName).Start(ctx, name, trace.WithAttributes(attrs...))
}

// startStorageSpan starts the span of a call to backend, so that every backend
// plugged into the Service is traced the same way.
func (s *Service) startStorageSpan(ctx context.Context, method string, backend storage.Backend) (context.Context, trace.Span) {
	return s.startSpan(ctx, "storage."+method,
		attribute.String("lfs.storage.name", backend.Name()),
		attribute.String("lfs.storage.type", string(backend.Type())),
	)
}

// endSpan ends span with the outcome of err. A client error is the expected
// answer to a bad request rather than a failure of the operation, so it is
// recorded as an attribute and leaves the span status unset.
func endSpan(span trace.Span, err error) {
	if err != nil {
		var lfsErr *Error
		if errors.As(err, &lfsErr) {
			span.SetAttributes(attribute.Int("lfs.error.code", lfsErr.Code))
		} else {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
	}
	span.End()
}
