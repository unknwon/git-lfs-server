package storage

import (
	"context"
	"io"

	"github.com/cockroachdb/errors"
)

var ErrNotFound = errors.New("storage: object not found")

// Type identifies a storage backend implementation. The value matches the
// subsection name used in ini configuration, e.g. [storage "local"].
type Type string

const (
	TypeLocal Type = "local"
)

// Backend is the storage backend for LFS object bytes.
type Backend interface {
	// Put stores the bytes from r under the given oid and returns a URI that
	// uniquely identifies the stored object, including the backend scheme.
	Put(ctx context.Context, oid string, r io.Reader) (uri string, err error)
	// Open returns a reader for the object at uri. Returns ErrNotFound if no
	// object exists at uri. The caller closes.
	Open(ctx context.Context, uri string) (io.ReadCloser, error)
	// Delete removes the object at uri. Returns nil if already absent.
	Delete(ctx context.Context, uri string) error
}
