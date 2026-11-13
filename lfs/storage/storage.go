package storage

import (
	"context"
	"io"
	"time"

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
	// URI returns the canonical, non-expiring location of the object. Pure
	// function of the OID and the backend's static config (e.g. bucket name),
	// safe to compute without I/O.
	URI(oid string) string
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
	// It may return without reading r when the object is already stored.
	Put(ctx context.Context, oid string, r io.Reader) (uri string, err error)
	// Open returns a reader for the object at URI. It returns ErrNotFound when no
	// matching object exists. The caller MUST close the reader.
	Open(ctx context.Context, uri string) (io.ReadCloser, error)
}

// Presigner is implemented by backends that validate object integrity when
// directly uploading from clients, and reject uploads with mismatched content.
//
// Each upload goes to its own upload location, so that an object at an upload
// location proves that the upload happened. The server copies the content to
// the canonical location of the object once the upload is verified.
type Presigner interface {
	Backend

	// UploadURI returns the location that the upload with the given ID sends the
	// content of oid to. Pure function, safe to compute without I/O.
	UploadURI(uploadID, oid string) string
	// PresignPut returns a short-lived URL the client PUTs the content of oid to,
	// along with the headers the client MUST send verbatim. The object is stored
	// at uri, and only when the content matches oid and size.
	PresignPut(ctx context.Context, uri, oid string, size int64) (url string, headers map[string]string, err error)
	// PresignGet returns a short-lived URL the client GETs from.
	PresignGet(ctx context.Context, uri string) (url string, err error)
	// Head returns the size of the object. It returns ErrNotFound when no matching
	// object exists.
	Head(ctx context.Context, uri string) (size int64, err error)
	// Copy copies the object of size bytes at srcURI to dstURI within the
	// storage, replacing any object at dstURI.
	Copy(ctx context.Context, srcURI, dstURI string, size int64) error
	// DeleteUploads deletes the objects at upload locations that were last
	// modified before the cutoff, and returns how many it deleted.
	DeleteUploads(ctx context.Context, before time.Time) (deleted int, err error)
}
