package janitor

import (
	"context"
	"strings"
	"time"

	"github.com/cockroachdb/errors"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"unknwon.dev/git-lfs-server/internal/database"
	"unknwon.dev/git-lfs-server/internal/logx"
	"unknwon.dev/git-lfs-server/internal/ptrx"
	"unknwon.dev/git-lfs-server/lfs/storage"
)

const instrumentationName = "unknwon.dev/git-lfs-server/internal/janitor"

const (
	sweepInterval  = 30 * time.Minute
	orphanAge      = 24 * time.Hour
	sweepBatchSize = 100
)

// Janitor sweeps orphan (never verified) and unreferenced (repo_count = 0)
// objects.
type Janitor struct {
	db           *database.DB
	storages     []storage.Backend
	sweptObjects metric.Int64Counter
}

// New constructs a Janitor using the given list of storage backends.
func New(db *database.DB, storages map[string]storage.Backend) *Janitor {
	seen := make(map[string]struct{})
	uniq := make([]storage.Backend, 0, len(storages))
	for _, b := range storages {
		if _, ok := seen[b.Scheme()]; ok {
			continue
		}
		seen[b.Scheme()] = struct{}{}
		uniq = append(uniq, b)
	}
	sweptObjects, _ := otel.Meter(instrumentationName).Int64Counter("lfsd.janitor.objects",
		metric.WithUnit("{object}"),
		metric.WithDescription("Objects the janitor swept, by pass and whether their content was deleted."),
	)
	return &Janitor{db: db, storages: uniq, sweptObjects: sweptObjects}
}

// Run blocks until ctx is canceled, sweeping on the configured interval. The
// first tick fires after one interval.
func (j *Janitor) Run(ctx context.Context, logger *logx.Logger) {
	logger.InfoContext(ctx, "Janitor started")

	t := time.NewTicker(sweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			logger.InfoContext(ctx, "Janitor stopped")
			return
		case <-t.C:
			j.Sweep(ctx, logger)
		}
	}
}

// Sweep runs both cleanup passes once. Errors are logged but not returned.
// The next tick will retry.
func (j *Janitor) Sweep(ctx context.Context, logger *logx.Logger) {
	tracer := otel.Tracer(instrumentationName)
	ctx, span := tracer.Start(ctx, "janitor.Sweep")
	defer span.End()

	orphanCtx, orphanSpan := tracer.Start(ctx, "janitor.SweepOrphans")
	if objects, err := j.db.SweepOrphanObjects(orphanCtx, orphanAge, sweepBatchSize); err != nil {
		logger.ErrorContext(orphanCtx, "Failed to sweep orphan objects", "error", err)
	} else if len(objects) > 0 {
		j.deleteObjects(orphanCtx, logger.Scoped("orphan"), "orphan", objects)
	}
	orphanSpan.End()

	unreferencedCtx, unreferencedSpan := tracer.Start(ctx, "janitor.SweepUnreferenced")
	if objects, err := j.db.SweepUnreferencedObjects(unreferencedCtx, sweepBatchSize); err != nil {
		logger.ErrorContext(unreferencedCtx, "Failed to sweep unreferenced objects", "error", err)
	} else if len(objects) > 0 {
		j.deleteObjects(unreferencedCtx, logger.Scoped("unreferenced"), "unreferenced", objects)
	}
	unreferencedSpan.End()
}

// deleteObjects deletes the given list of objects from the storage backends.
// The pass names the sweep that found them in metrics.
func (j *Janitor) deleteObjects(ctx context.Context, logger *logx.Logger, pass string, objects []database.Object) {
	var deletedCount, failedCount int
	for _, o := range objects {
		uri := ptrx.Deref(o.ObjectURI, "")
		if uri != "" {
			if err := j.deleteByURI(ctx, uri); err != nil {
				logger.ErrorContext(ctx, "Failed to delete object", "oid", o.OID, "uri", uri, "error", err)
				failedCount++
			} else {
				deletedCount++
			}
			continue
		}
		failed := false
		for _, b := range j.storages {
			p, ok := b.(storage.Presigner)
			if !ok {
				continue
			}
			presignURI := p.URI(o.OID)
			if err := p.Delete(ctx, presignURI); err != nil {
				logger.ErrorContext(ctx, "Failed to delete presign object", "oid", o.OID, "scheme", b.Scheme(), "error", err)
				failed = true
			}
		}
		if failed {
			failedCount++
		} else {
			deletedCount++
		}
	}
	trace.SpanFromContext(ctx).SetAttributes(
		attribute.Int("janitor.deleted_count", deletedCount),
		attribute.Int("janitor.failed_count", failedCount),
	)
	passAttr := attribute.String("janitor.pass", pass)
	j.sweptObjects.Add(ctx, int64(deletedCount), metric.WithAttributes(passAttr, attribute.String("janitor.result", "deleted")))
	j.sweptObjects.Add(ctx, int64(failedCount), metric.WithAttributes(passAttr, attribute.String("janitor.result", "failed")))
	logFn := logger.InfoContext
	if failedCount > 0 {
		logFn = logger.ErrorContext
	}
	logFn(ctx, "Swept objects", "deletedCount", deletedCount, "failedCount", failedCount)
}

func (j *Janitor) deleteByURI(ctx context.Context, uri string) error {
	for _, b := range j.storages {
		if strings.HasPrefix(uri, b.Scheme()) {
			return b.Delete(ctx, uri)
		}
	}
	return errors.Newf("no backend for uri %q", uri)
}
