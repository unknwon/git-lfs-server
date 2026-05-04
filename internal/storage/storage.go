package storage

import (
	"context"
	"io"

	"github.com/cockroachdb/errors"
)

var ErrNotFound = errors.New("storage: object not found")

// Backend is a storage backend. Implementations must also satisfy either
// Proxier or Presigner.
type Backend interface {
	// Name is the configured name from the [storage "{name}"] section,
	// used in logs to identify which backend handled a request.
	Name() string
	// Type identifies the storage implementation, matching the TYPE key in the
	// [storage "{name}"] config section.
	Type() Type
}

// Type identifies a storage backend implementation. The value matches the
// TYPE key inside a [storage "{name}"] section.
type Type string

const (
	TypeFilesystem Type = "filesystem"
	TypeS3Presign  Type = "s3-presign"
)

// Proxier is implemented by backends that cannot validate object
// integrity server-side. The lfsd server itself reads the body, hashes it
// via iox.SHA256Reader, and writes the bytes to the backend.
type Proxier interface {
	Backend

	// Put stores the bytes from r under the given oid and returns a URI that
	// uniquely identifies the stored object, including the backend scheme.
	Put(ctx context.Context, oid string, r io.Reader) (uri string, err error)
	// Open returns a reader for the object at uri. Returns ErrNotFound if no
	// object exists at uri. The caller closes.
	Open(ctx context.Context, uri string) (io.ReadCloser, error)
	// Delete removes the object at uri. Returns nil if already absent.
	Delete(ctx context.Context, uri string) error
}

// Presigner is implemented by backends that validate SHA-256 server-side.
// The server hands the client a short-lived presigned URL, and the client
// transfers bytes directly to/from the backend. The lfsd server only sees
// JSON exchanges (batch, verify), never the object bytes.
type Presigner interface {
	Backend

	// URI returns the canonical, non-expiring location of the object. Pure
	// function of the OID and the backend's static config (e.g. bucket name),
	// safe to compute without I/O.
	URI(oid string) string
	// PresignPut returns a short-lived URL the client PUTs to, plus the
	// headers the client must send verbatim. The SDK signs these into the
	// URL, so the server cannot omit them.
	PresignPut(ctx context.Context, oid string, size int64) (url string, headers map[string]string, err error)
	// PresignGet returns a short-lived URL the client GETs from. No headers
	// are required: the URL alone is sufficient for retrieval.
	PresignGet(ctx context.Context, uri string) (url string, err error)
	// Head returns the object's size, or ErrNotFound if absent.
	Head(ctx context.Context, uri string) (size int64, err error)
	// Delete removes the object at uri. Returns nil if already absent.
	Delete(ctx context.Context, uri string) error
}
