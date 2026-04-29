package database

import (
	"context"
	"time"

	"github.com/cockroachdb/errors"
	"gorm.io/gorm"
)

type Object struct {
	ID        int64     `gorm:"column:id;primaryKey"`
	OID       string    `gorm:"column:oid;uniqueIndex;not null"`
	Size      int64     `gorm:"column:size;not null"`
	ObjectURI string    `gorm:"column:object_uri;not null"`
	RepoCount int64     `gorm:"column:repo_count;not null;default:0"`
	CreatedAt time.Time `gorm:"column:created_at;not null;default:now()"`
}

var ErrObjectNotFound = errors.New("database: object not found")

// GetRepoObjectByOID fetches the Object linked to repoName by oid. Returns
// ErrObjectNotFound when no row matches.
func (d *DB) GetRepoObjectByOID(ctx context.Context, repoName, oid string) (*Object, error) {
	var object Object
	err := d.db.WithContext(ctx).Raw(
		`SELECT objects.*
		 FROM objects
		 JOIN repo_objects ON repo_objects.object_id = objects.id
		 WHERE repo_objects.repo_name = @repoName
		   AND objects.oid = @oid
		 LIMIT 1`,
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

// LinkObject upserts an Object by OID and links it to repoName via repo_objects
// in a single transaction. Returns the canonical object_uri (which may differ
// from the just-uploaded uri if another concurrent upload won the race —
// caller can compare and clean up). The repo link is idempotent on conflict;
// repo_count is bumped only when a new link is actually created.
func (d *DB) LinkObject(ctx context.Context, repoName, oid string, size int64, objectURI string) (storedURI string, err error) {
	err = d.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var object Object
		if err := tx.Raw(
			`INSERT INTO objects (oid, size, object_uri, repo_count, created_at)
			 VALUES (@oid, @size, @objectURI, 0, now())
			 ON CONFLICT (oid) DO UPDATE SET oid = excluded.oid
			 RETURNING *`,
			map[string]any{"oid": oid, "size": size, "objectURI": objectURI},
		).Scan(&object).Error; err != nil {
			return errors.Wrap(err, "upsert object")
		}
		if object.Size != size {
			return errors.Newf("oid %q already stored with size %d (request claimed %d)", oid, object.Size, size)
		}
		storedURI = object.ObjectURI

		res := tx.Exec(
			`INSERT INTO repo_objects (repo_name, object_id, created_at)
			 VALUES (@repoName, @objectID, now())
			 ON CONFLICT (repo_name, object_id) DO NOTHING`,
			map[string]any{"repoName": repoName, "objectID": object.ID},
		)
		if res.Error != nil {
			return errors.Wrap(res.Error, "insert repo object")
		}
		if res.RowsAffected > 0 {
			if err := tx.Exec(
				`UPDATE objects
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

// GetObjectsByOIDs fetches Object rows for the given OIDs across all repos
// (content-addressed dedup lookup). Result is keyed by OID; missing OIDs are
// absent from the map.
func (d *DB) GetObjectsByOIDs(ctx context.Context, oids []string) (map[string]Object, error) {
	if len(oids) == 0 {
		return map[string]Object{}, nil
	}
	var rows []Object
	err := d.db.WithContext(ctx).Raw(
		`SELECT * FROM objects WHERE oid IN (@oids)`,
		map[string]any{"oids": oids},
	).Scan(&rows).Error
	if err != nil {
		return nil, errors.Wrap(err, "select objects by oids")
	}
	out := make(map[string]Object, len(rows))
	for _, r := range rows {
		out[r.OID] = r
	}
	return out, nil
}

// GetRepoObjectsByOIDs fetches Object rows for the given OIDs that are linked
// to repoName via repo_objects. Result is keyed by OID; missing OIDs are absent.
func (d *DB) GetRepoObjectsByOIDs(ctx context.Context, repoName string, oids []string) (map[string]Object, error) {
	if len(oids) == 0 {
		return map[string]Object{}, nil
	}
	var rows []Object
	err := d.db.WithContext(ctx).Raw(
		`SELECT objects.*
		 FROM objects
		 JOIN repo_objects ON repo_objects.object_id = objects.id
		 WHERE repo_objects.repo_name = @repoName
		   AND objects.oid IN (@oids)`,
		map[string]any{"repoName": repoName, "oids": oids},
	).Scan(&rows).Error
	if err != nil {
		return nil, errors.Wrap(err, "select repo objects by oids")
	}
	out := make(map[string]Object, len(rows))
	for _, r := range rows {
		out[r.OID] = r
	}
	return out, nil
}
