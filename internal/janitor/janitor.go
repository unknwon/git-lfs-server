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

// Config holds tunables for the background janitor loop.
type Config struct {
	// Interval between sweeps, in minutes.
	Interval int `ini:"INTERVAL"`
	// OrphanAgeHours is the minimum age, in hours, before a pending object is
	// considered an orphan and eligible for deletion.
	OrphanAgeHours int `ini:"ORPHAN_AGE_HOURS"`
	// UnrefGraceHours is the minimum age, in hours, of verified_at before a
	// row whose repo_count has dropped to zero is eligible for deletion.
	UnrefGraceHours int `ini:"UNREF_GRACE_HOURS"`
	// BatchSize bounds the number of rows processed per sweep per category.
	BatchSize int `ini:"BATCH_SIZE"`
}

// Janitor sweeps orphan (never verified) and unreferenced (repo_count = 0)
// rows out of the objects table and deletes their storage blobs. Multiple
// lfsd instances can run concurrently: row selection uses FOR UPDATE SKIP
// LOCKED so they never contend on the same rows.
type Janitor struct {
	db       *database.DB
	storages []storage.Backend
	cfg      Config
}

func New(db *database.DB, storages map[string]storage.Backend, cfg Config) *Janitor {
	if cfg.Interval <= 0 {
		cfg.Interval = 30
	}
	if cfg.OrphanAgeHours <= 0 {
		cfg.OrphanAgeHours = 24
	}
	if cfg.UnrefGraceHours <= 0 {
		cfg.UnrefGraceHours = 1
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 100
	}

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
	return &Janitor{db: db, storages: uniq, cfg: cfg}
}

// Run blocks until ctx is cancelled, sweeping on the configured interval. The
// first tick fires after one interval; if startup-time cleanup is desired,
// callers can invoke Sweep directly before Run.
func (j *Janitor) Run(ctx context.Context, logger *logx.Logger) {
	logger = logger.With(
		"intervalMin", j.cfg.Interval,
		"orphanAgeHours", j.cfg.OrphanAgeHours,
		"unrefGraceHours", j.cfg.UnrefGraceHours,
		"batchSize", j.cfg.BatchSize,
	)
	logger.InfoContext(ctx, "Janitor started")

	t := time.NewTicker(time.Duration(j.cfg.Interval) * time.Minute)
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
	orphanAge := time.Duration(j.cfg.OrphanAgeHours) * time.Hour
	unrefGrace := time.Duration(j.cfg.UnrefGraceHours) * time.Hour

	if rows, err := j.db.SweepOrphanObjects(ctx, orphanAge, j.cfg.BatchSize); err != nil {
		logger.ErrorContext(ctx, "Failed to sweep orphan objects", "error", err)
	} else if len(rows) > 0 {
		j.deleteBlobs(ctx, logger.Scoped("orphan"), rows, true)
	}

	if rows, err := j.db.SweepUnreferencedObjects(ctx, unrefGrace, j.cfg.BatchSize); err != nil {
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
