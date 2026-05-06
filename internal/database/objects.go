package database

import (
	"context"
	"strconv"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/lib/pq"
	"gorm.io/gorm"

	"unknwon.dev/git-lfs-server/internal/ptrx"
)

type Object struct {
	ID         int64      `gorm:"column:id;primaryKey"`
	OID        string     `gorm:"column:oid;uniqueIndex;not null"`
	Size       *int64     `gorm:"column:size"`
	ObjectURI  *string    `gorm:"column:object_uri"`
	RepoCount  int64      `gorm:"column:repo_count;not null;default:0"`
	CreatedAt  time.Time  `gorm:"column:created_at;not null;default:now()"`
	VerifiedAt *time.Time `gorm:"column:verified_at"`
}

var ErrObjectNotFound = errors.New("database: object not found")

// GetRepoObjectByOID fetches the verified Object linked to repoName by oid.
// Pending (unverified) rows are excluded so unverified objects can never serve
// as download sources. Returns ErrObjectNotFound when no row matches.
func (d *DB) GetRepoObjectByOID(ctx context.Context, repoName, oid string) (*Object, error) {
	var object Object
	err := d.db.WithContext(ctx).Raw(`
SELECT objects.*
FROM objects
JOIN repo_objects ON repo_objects.object_id = objects.id
WHERE
	repo_objects.repo_name = @repoName
AND objects.oid = @oid
AND objects.verified_at IS NOT NULL`,
		map[string]any{"repoName": repoName, "oid": oid},
	).Scan(&object).Error
	if err != nil {
		return nil, errors.Wrap(err, "select repo object by oid")
	}
	if object.ID == 0 {
		return nil, errors.WithStack(ErrObjectNotFound)
	}
	return &object, nil
}

// GetObjectByOID fetches an Object by oid (verified or pending) without any
// repo filtering. Used by the upload handler for the dedup short-circuit and
// to detect pending rows. Returns ErrObjectNotFound when no row matches.
func (d *DB) GetObjectByOID(ctx context.Context, oid string) (*Object, error) {
	var object Object
	err := d.db.WithContext(ctx).Raw(
		`SELECT * FROM objects WHERE oid = @oid LIMIT 1`,
		map[string]any{"oid": oid},
	).Scan(&object).Error
	if err != nil {
		return nil, errors.Wrap(err, "select object by oid")
	}
	if object.ID == 0 {
		return nil, errors.WithStack(ErrObjectNotFound)
	}
	return &object, nil
}

// LinkObject is the storage.Proxier verify-equivalent path: bytes are already
// stored, write a fully-populated (verified) objects row and link it to
// repoName in one transaction. Returns the canonical object_uri (which may
// differ from the just-uploaded URI if another concurrent upload won the
// race, and caller MUST compare and clean up).
func (d *DB) LinkObject(ctx context.Context, repoName, oid string, size int64, objectURI string) (storedURI string, err error) {
	err = d.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var object Object
		if err := tx.Raw(`
INSERT INTO objects (oid, size, object_uri, repo_count, created_at, verified_at)
VALUES (@oid, @size, @objectURI, 0, now(), now())
ON CONFLICT (oid) DO UPDATE SET
	size        = COALESCE(objects.size, EXCLUDED.size),
	object_uri  = COALESCE(objects.object_uri, EXCLUDED.object_uri),
	verified_at = COALESCE(objects.verified_at, EXCLUDED.verified_at)
RETURNING *`,
			map[string]any{"oid": oid, "size": size, "objectURI": objectURI},
		).Scan(&object).Error; err != nil {
			return errors.Wrap(err, "upsert object")
		}
		if storedSize := ptrx.Deref(object.Size, -1); storedSize != size {
			return errors.Newf("oid %q already stored with size %d (request claimed %d)", oid, storedSize, size)
		}
		storedURI = ptrx.Deref(object.ObjectURI, "")
		if storedURI == "" {
			return errors.Newf("oid %q upserted without object_uri", oid)
		}

		res := tx.Exec(`
INSERT INTO repo_objects (repo_name, object_id, created_at)
VALUES (@repoName, @objectID, now())
ON CONFLICT (repo_name, object_id) DO NOTHING`,
			map[string]any{"repoName": repoName, "objectID": object.ID},
		)
		if res.Error != nil {
			return errors.Wrap(res.Error, "upsert repo object")
		}
		if res.RowsAffected > 0 {
			if err := tx.Exec(`
UPDATE objects
SET repo_count = (SELECT COUNT(*) FROM repo_objects WHERE object_id = @id)
WHERE id = @id`,
				map[string]any{"id": object.ID},
			).Error; err != nil {
				return errors.Wrap(err, "update repo_count")
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return storedURI, nil
}

// InsertPendingObject inserts a row with NULL size/object_uri/verified_at only if
// no row exists for oid.
func (d *DB) InsertPendingObject(ctx context.Context, oid string) error {
	if err := d.db.WithContext(ctx).Exec(`
INSERT INTO objects (oid, repo_count, created_at)
VALUES (@oid, 0, now())
ON CONFLICT (oid) DO NOTHING`,
		map[string]any{"oid": oid},
	).Error; err != nil {
		return errors.Wrap(err, "insert pending object")
	}
	return nil
}

// VerifyObject marks the pending object for oid as verified and links to the
// given repository. Returns the canonical object_uri (which may differ from
// the just-uploaded URI if another concurrent verify won the race, and caller
// MUST compare and clean up).
func (d *DB) VerifyObject(ctx context.Context, repoName, oid string, size int64, objectURI string) (storedURI string, err error) {
	err = d.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(`
UPDATE objects
SET size = @size, object_uri = @objectURI, verified_at = now()
WHERE oid = @oid AND verified_at IS NULL`,
			map[string]any{"oid": oid, "size": size, "objectURI": objectURI},
		).Error; err != nil {
			return errors.Wrap(err, "mark object verified")
		}

		var object Object
		if err := tx.Raw(
			`SELECT * FROM objects WHERE oid = @oid LIMIT 1`,
			map[string]any{"oid": oid},
		).Scan(&object).Error; err != nil {
			return errors.Wrap(err, "reload verified object")
		}
		if object.ID == 0 {
			return errors.WithStack(ErrObjectNotFound)
		}
		if storedSize := ptrx.Deref(object.Size, -1); storedSize != size {
			return errors.Newf("oid %q stored with size %d (verify claimed %d)", oid, storedSize, size)
		}
		storedURI = ptrx.Deref(object.ObjectURI, "")
		if storedURI == "" {
			return errors.Newf("oid %q verified without object_uri", oid)
		}

		res := tx.Exec(`
INSERT INTO repo_objects (repo_name, object_id, created_at)
VALUES (@repoName, @objectID, now())
ON CONFLICT (repo_name, object_id) DO NOTHING`,
			map[string]any{"repoName": repoName, "objectID": object.ID},
		)
		if res.Error != nil {
			return errors.Wrap(res.Error, "upsert repo object")
		}
		if res.RowsAffected > 0 {
			if err := tx.Exec(`
UPDATE objects
SET repo_count = (SELECT COUNT(*) FROM repo_objects WHERE object_id = @id)
WHERE id = @id`,
				map[string]any{"id": object.ID},
			).Error; err != nil {
				return errors.Wrap(err, "update repo_count")
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return storedURI, nil
}

// GetObjectsByOIDs fetches verified Object rows for the given OIDs. Results are
// keyed by OID, silently dropping missing OIDs.
func (d *DB) GetObjectsByOIDs(ctx context.Context, oids []string) (map[string]Object, error) {
	if len(oids) == 0 {
		return map[string]Object{}, nil
	}
	var rows []Object
	err := d.db.WithContext(ctx).Raw(`
SELECT * FROM objects
WHERE oid = ANY(@oids) AND verified_at IS NOT NULL`,
		map[string]any{"oids": pq.Array(oids)},
	).Scan(&rows).Error
	if err != nil {
		return nil, errors.Wrap(err, "select objects by OIDs")
	}
	out := make(map[string]Object, len(rows))
	for _, r := range rows {
		out[r.OID] = r
	}
	return out, nil
}

// GetRepoObjectsByOIDs fetches verified Object rows for the given OIDs that are
// linked to the repository. Results are keyed by OID, silently dropping missing
// OIDs.
func (d *DB) GetRepoObjectsByOIDs(ctx context.Context, repoName string, oids []string) (map[string]Object, error) {
	if len(oids) == 0 {
		return map[string]Object{}, nil
	}
	var rows []Object
	err := d.db.WithContext(ctx).Raw(`
SELECT objects.*
FROM objects
JOIN repo_objects ON repo_objects.object_id = objects.id
WHERE
	repo_objects.repo_name = @repoName
AND objects.oid = ANY(@oids)
AND objects.verified_at IS NOT NULL`,
		map[string]any{"repoName": repoName, "oids": pq.Array(oids)},
	).Scan(&rows).Error
	if err != nil {
		return nil, errors.Wrap(err, "select repo objects by OIDs")
	}
	out := make(map[string]Object, len(rows))
	for _, r := range rows {
		out[r.OID] = r
	}
	return out, nil
}

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
