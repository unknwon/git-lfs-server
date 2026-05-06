package database

import (
	"context"
	"strconv"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/lib/pq"
	"gorm.io/gorm"
)

// SweepOrphanObjects deletes up to limit pending objects (verified_at IS NULL)
// whose created_at is older than orphanAge. Selection uses FOR UPDATE SKIP
// LOCKED so multiple lfsd instances running the janitor concurrently never
// contend on the same rows.
//
// Returns the deleted rows so the caller can delete the corresponding storage
// blobs. The DB delete and the row lock are held in a single transaction. Blob
// delete happens after commit, so callers must tolerate a row whose blob
// outlives the row by a few milliseconds.
func (d *DB) SweepOrphanObjects(ctx context.Context, orphanAge time.Duration, limit int) ([]Object, error) {
	if limit <= 0 {
		return nil, nil
	}
	var deleted []Object
	err := d.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var rows []Object
		if err := tx.Raw(`
SELECT *
FROM objects
WHERE verified_at IS NULL
  AND created_at < now() - CAST(@age AS interval)
ORDER BY created_at
LIMIT @limit
FOR UPDATE SKIP LOCKED`,
			map[string]any{
				"age":   formatPGInterval(orphanAge),
				"limit": limit,
			},
		).Scan(&rows).Error; err != nil {
			return errors.Wrap(err, "select orphan objects")
		}
		if len(rows) == 0 {
			return nil
		}
		ids := make([]int64, len(rows))
		for i, r := range rows {
			ids[i] = r.ID
		}
		if err := tx.Exec(
			`DELETE FROM objects WHERE id = ANY(@ids)`,
			map[string]any{"ids": pq.Array(ids)},
		).Error; err != nil {
			return errors.Wrap(err, "delete orphan objects")
		}
		deleted = rows
		return nil
	})
	if err != nil {
		return nil, err
	}
	return deleted, nil
}

// SweepUnreferencedObjects deletes up to limit verified objects whose
// repo_count has dropped to zero.
//
// Selection uses FOR UPDATE SKIP LOCKED. The link count is re-derived from
// repo_objects inside the locked transaction so we never delete a row that
// was relinked between the WHERE evaluation and the lock acquisition.
func (d *DB) SweepUnreferencedObjects(ctx context.Context, limit int) ([]Object, error) {
	if limit <= 0 {
		return nil, nil
	}
	var deleted []Object
	err := d.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var candidates []Object
		if err := tx.Raw(`
SELECT *
FROM objects
WHERE verified_at IS NOT NULL
  AND repo_count = 0
ORDER BY verified_at
LIMIT @limit
FOR UPDATE SKIP LOCKED`,
			map[string]any{
				"limit": limit,
			},
		).Scan(&candidates).Error; err != nil {
			return errors.Wrap(err, "select unreferenced objects")
		}
		if len(candidates) == 0 {
			return nil
		}

		// repo_count is a denormalised cache. Re-derive the live link count
		// from repo_objects under the row lock so we never delete a row that
		// LinkObject relinked between the WHERE evaluation and the lock
		// acquisition.
		ids := make([]int64, 0, len(candidates))
		for _, c := range candidates {
			var live int64
			if err := tx.Raw(
				`SELECT COUNT(*) FROM repo_objects WHERE object_id = @id`,
				map[string]any{"id": c.ID},
			).Scan(&live).Error; err != nil {
				return errors.Wrap(err, "recount repo_objects")
			}
			if live == 0 {
				ids = append(ids, c.ID)
				deleted = append(deleted, c)
			}
		}
		if len(ids) == 0 {
			return nil
		}
		if err := tx.Exec(
			`DELETE FROM objects WHERE id = ANY(@ids)`,
			map[string]any{"ids": pq.Array(ids)},
		).Error; err != nil {
			return errors.Wrap(err, "delete unreferenced objects")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return deleted, nil
}

// formatPGInterval renders a Go duration as a Postgres interval literal in
// seconds. The numeric form sidesteps locale-sensitive parsing of strings
// like "24 hours".
func formatPGInterval(d time.Duration) string {
	return strconv.FormatFloat(d.Seconds(), 'f', 3, 64) + " seconds"
}
