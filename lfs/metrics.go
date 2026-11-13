package lfs

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Instruments are created on use rather than once per Service: the SDK returns
// the same instrument for the same name, and Service has no constructor to
// create them in.
func (s *Service) meter() metric.Meter {
	mp := s.MeterProvider
	if mp == nil {
		mp = otel.GetMeterProvider()
	}
	return mp.Meter(instrumentationName)
}

func (s *Service) addBatchObjects(ctx context.Context, operation string, n int) {
	counter, _ := s.meter().Int64Counter("lfs.batch.objects",
		metric.WithUnit("{object}"),
		metric.WithDescription("Objects requested in batch requests."),
	)
	counter.Add(ctx, int64(n), metric.WithAttributes(attribute.String("lfs.operation", operation)))
}

// addProxiedBytes records content that went through the server rather than
// directly between the client and a storage.Presigner.
func (s *Service) addProxiedBytes(ctx context.Context, operation string, n int64) {
	counter, _ := s.meter().Int64Counter("lfs.proxied",
		metric.WithUnit("By"),
		metric.WithDescription("Object content transferred through the server."),
	)
	counter.Add(ctx, n, metric.WithAttributes(attribute.String("lfs.operation", operation)))
}
