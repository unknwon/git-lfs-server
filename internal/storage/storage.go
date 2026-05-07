package storage

import (
	"context"
	"io"

	"github.com/cockroachdb/errors"
)

var ErrNotFound = errors.New("storage: object not found")

// Backend is a storage backend. Implementations must also satisfy either Proxier
// or Presigner.
type Backend interface {
	// Name is the configured name from the [storage "{name}"] section, used in logs
	// to identify which backend handled a request.
	Name() string
	// Type identifies the storage implementation, matching the TYPE key in the
	// [storage "{name}"] config section.
	Type() Type
	// Scheme returns the URI scheme prefix (including "://") that this backend owns.
	Scheme() string
	// Delete removes the object at uri. Returns nil if already absent.
	Delete(ctx context.Context, uri string) error
}

// Type identifies a storage backend implementation. The value matches the
// TYPE key inside a [storage "{name}"] section.
type Type string

const (
	TypeFilesystem Type = "filesystem"
	TypeS3Presign  Type = "s3-presign"
)

// Proxier is implemented by backends that cannot validate object integrity when
// directly uploading from clients, and must proxy via the server.
type Proxier interface {
	Backend

	// Put stores the bytes from the reader under the given OID and returns a URI
	// that uniquely identifies the stored object, including the backend scheme.
	Put(ctx context.Context, oid string, r io.Reader) (uri string, err error)
	// Open returns a reader for the object at URI. It returns ErrNotFound when no
	// matching object exists. The caller MUST close the reader.
	Open(ctx context.Context, uri string) (io.ReadCloser, error)
}

// Presigner is implemented by backends that validate object integrity when
// directly uploading from clients, and reject uploads with mismatched content.
type Presigner interface {
	Backend

	// URI returns the canonical, non-expiring location of the object. Pure
	// function of the OID and the backend's static config (e.g. bucket name),
	// safe to compute without I/O.
	URI(oid string) string
	// PresignPut returns a short-lived URL the client PUTs to, along with the
	// headers the client MUST send verbatim.
	PresignPut(ctx context.Context, oid string, size int64) (url string, headers map[string]string, err error)
	// PresignGet returns a short-lived URL the client GETs from.
	PresignGet(ctx context.Context, uri string) (url string, err error)
	// Head returns the size of the object. It returns ErrNotFound when no matching
	// object exists.
	Head(ctx context.Context, uri string) (size int64, err error)
}
