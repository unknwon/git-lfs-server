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
	sweepInterval    = 30 * time.Minute
	orphanAge        = 24 * time.Hour
	unrefGracePeriod = 1 * time.Hour
	sweepBatchSize   = 100
)

// Janitor sweeps orphan (never verified) and unreferenced (repo_count = 0)
// rows out of the objects table and deletes their storage blobs. Multiple
// lfsd instances can run concurrently: row selection uses FOR UPDATE SKIP
// LOCKED so they never contend on the same rows.
type Janitor struct {
	db       *database.DB
	storages []storage.Backend
}

func New(db *database.DB, storages map[string]storage.Backend) *Janitor {
	// Deduplicate by scheme since multiple forges can share one backend.
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

// Sweep runs both cleanup passes once. Errors are logged but not returned;
// the next tick will retry.
func (j *Janitor) Sweep(ctx context.Context, logger *logx.Logger) {
	if rows, err := j.db.SweepOrphanObjects(ctx, orphanAge, sweepBatchSize); err != nil {
		logger.ErrorContext(ctx, "Failed to sweep orphan objects", "error", err)
	} else if len(rows) > 0 {
		j.deleteBlobs(ctx, logger.Scoped("orphan"), rows, true)
	}

	if rows, err := j.db.SweepUnreferencedObjects(ctx, unrefGracePeriod, sweepBatchSize); err != nil {
		logger.ErrorContext(ctx, "Failed to sweep unreferenced objects", "error", err)
	} else if len(rows) > 0 {
		j.deleteBlobs(ctx, logger.Scoped("unreferenced"), rows, false)
	}
}

// deleteBlobs deletes storage blobs for rows whose DB tuples were just
// removed. tryAllPresigners controls fallback behaviour for rows whose
// object_uri is NULL (pending/orphan rows that may have been written to a
// presign backend without recording the URI). For unreferenced verified rows,
// object_uri is always set so the fallback is unused.
func (j *Janitor) deleteBlobs(ctx context.Context, logger *logx.Logger, rows []database.Object, tryAllPresigners bool) {
	logger.InfoContext(ctx, "Deleted DB rows", "count", len(rows))
	for _, r := range rows {
		uri := ptrx.Deref(r.ObjectURI, "")
		if uri != "" {
			if err := j.deleteByURI(ctx, uri); err != nil {
				logger.ErrorContext(ctx, "Failed to delete blob", "oid", r.OID, "uri", uri, "error", err)
			}
			continue
		}
		if !tryAllPresigners {
			continue
		}
		// Pending row with no recorded URI: the client may have PUT bytes via
		// a presigned URL keyed by oid. We don't know which host's backend was
		// used, so try every distinct presigner. Delete is idempotent.
		for _, b := range j.storages {
			p, ok := b.(storage.Presigner)
			if !ok {
				continue
			}
			presignURI := p.URI(r.OID)
			if err := p.Delete(ctx, presignURI); err != nil {
				logger.ErrorContext(ctx, "Failed to delete presign blob", "oid", r.OID, "scheme", b.Scheme(), "error", err)
			}
		}
	}
}

func (j *Janitor) deleteByURI(ctx context.Context, uri string) error {
	for _, b := range j.storages {
		if !strings.HasPrefix(uri, b.Scheme()) {
			continue
		}
		switch v := b.(type) {
		case storage.Proxier:
			return v.Delete(ctx, uri)
		case storage.Presigner:
			return v.Delete(ctx, uri)
		}
	}
	return errors.Newf("no backend for uri %q", uri)
}
