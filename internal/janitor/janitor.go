package janitor

import (
	"context"
	"strings"
	"time"

	"github.com/cockroachdb/errors"

	"unknwon.dev/git-lfs-server/internal/database"
	"unknwon.dev/git-lfs-server/internal/logx"
	"unknwon.dev/git-lfs-server/internal/ptrx"
	"unknwon.dev/git-lfs-server/internal/storage"
)

const (
	sweepInterval  = 30 * time.Minute
	orphanAge      = 24 * time.Hour
	sweepBatchSize = 100
)

// Janitor sweeps orphan (never verified) and unreferenced (repo_count = 0)
// objects.
type Janitor struct {
	db       *database.DB
	storages []storage.Backend
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
	return &Janitor{db: db, storages: uniq}
}

// Run blocks until ctx is cancelled, sweeping on the configured interval. The
// first tick fires after one interval; if startup-time cleanup is desired,
// callers can invoke Sweep directly before Run.
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
	if objects, err := j.db.SweepOrphanObjects(ctx, orphanAge, sweepBatchSize); err != nil {
		logger.ErrorContext(ctx, "Failed to sweep orphan objects", "error", err)
	} else if len(objects) > 0 {
		j.deleteObjects(ctx, logger.Scoped("orphan"), objects)
	}

	if objects, err := j.db.SweepUnreferencedObjects(ctx, sweepBatchSize); err != nil {
		logger.ErrorContext(ctx, "Failed to sweep unreferenced objects", "error", err)
	} else if len(objects) > 0 {
		j.deleteObjects(ctx, logger.Scoped("unreferenced"), objects)
	}
}

// deleteObjects deletes the given list of objects from the storage backends.
func (j *Janitor) deleteObjects(ctx context.Context, logger *logx.Logger, objects []database.Object) {
	logger.InfoContext(ctx, "Deleted objects", "count", len(objects))
	for _, o := range objects {
		uri := ptrx.Deref(o.ObjectURI, "")
		if uri != "" {
			if err := j.deleteByURI(ctx, uri); err != nil {
				logger.ErrorContext(ctx, "Failed to delete object", "oid", o.OID, "uri", uri, "error", err)
			}
			continue
		}
		for _, b := range j.storages {
			p, ok := b.(storage.Presigner)
			if !ok {
				continue
			}
			presignURI := p.URI(o.OID)
			if err := p.Delete(ctx, presignURI); err != nil {
				logger.ErrorContext(ctx, "Failed to delete presign object", "oid", o.OID, "scheme", b.Scheme(), "error", err)
			}
		}
	}
}

func (j *Janitor) deleteByURI(ctx context.Context, uri string) error {
	for _, b := range j.storages {
		if strings.HasPrefix(uri, b.Scheme()) {
			return b.Delete(ctx, uri)
		}
	}
	return errors.Newf("no backend for uri %q", uri)
}
