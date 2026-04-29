package database

import "time"

type RepoObject struct {
	ID        int64     `gorm:"column:id;primaryKey"`
	RepoName  string    `gorm:"column:repo_name;not null;uniqueIndex:idx_repo_objects_repo_name_object_id,priority:1"`
	ObjectID  int64     `gorm:"column:object_id;not null;index;uniqueIndex:idx_repo_objects_repo_name_object_id,priority:2"`
	CreatedAt time.Time `gorm:"column:created_at;not null;default:now()"`
}
