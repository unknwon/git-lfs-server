// Package lfs implements the server side of the Git LFS Batch API and the
// basic transfer adapter on top of pluggable metadata and object storage.
//
// It leaves HTTP routing, authentication, and authorization to the caller, so
// that servers with different URL layouts, credentials, and databases can
// share the transfer logic. It is a separate Go module so that importers do
// not pull in the dependencies of lfsd (PostgreSQL, Flamego, forge clients).
//
// A [Service] combines a [Store], which records objects and the repositories
// they are linked to, with a storage.Backend, which holds the content. A
// storage.Proxier backend (e.g., storage.FilesystemBackend) streams
// content through the server, and a storage.Presigner backend (e.g.,
// storage.S3PresignBackend) redirects clients to the storage.
//
// An OID is not secret, because pointer files carry it in plain text. So a
// repository only gains an object by uploading its content, even when the
// object is already stored for another repository. Storage keeps one copy of
// each object, shared by the repositories it is linked to. A presigned upload
// goes to its own upload location, and verification copies it to the location
// of the object.
//
// [Service.Serve] handles the HTTP requests of the batch, upload, download,
// and verify endpoints. The caller routes the requests, authenticates them,
// and passes an [Access] with the repository, the base URL of its endpoints,
// and whether the actor can read and write. Serve works with any router that
// can run a net/http handler, e.g., Flamego, Macaron, or [net/http.ServeMux]:
//
//	mux.HandleFunc("/{repo}/info/lfs/objects/", func(w http.ResponseWriter, r *http.Request) {
//		access, ok := authorize(w, r) // Your authentication and authorization.
//		if !ok {
//			return
//		}
//		service.Serve(logger, w, r, access)
//	})
//
// The lower-level [Service.Batch], [Service.Upload], [Service.Download], and
// [Service.Verify] methods are available for servers that need their own HTTP
// handling.
//
// Spec: https://github.com/git-lfs/git-lfs/blob/main/docs/api/batch.md
package lfs

import (
	"context"
	"regexp"
)

// oidPattern matches the SHA-256 hex digest format used as the LFS object ID:
// exactly 64 lowercase hex characters.
var oidPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidOID reports whether oid is a valid LFS object ID.
func ValidOID(oid string) bool {
	return oidPattern.MatchString(oid)
}

const (
	OperationUpload   = "upload"
	OperationDownload = "download"
	TransferBasic     = "basic"
)

// BatchRequest is the request payload of the batch endpoint.
type BatchRequest struct {
	Operation string        `json:"operation"`
	Transfers []string      `json:"transfers,omitempty"`
	Objects   []BatchObject `json:"objects"`
}

// BatchObject is an object requested in a BatchRequest.
type BatchObject struct {
	OID  string `json:"oid"`
	Size int64  `json:"size"`
}

// BatchResponse is the response payload of the batch endpoint.
type BatchResponse struct {
	Transfer string                `json:"transfer"`
	Objects  []BatchResponseObject `json:"objects"`
}

// BatchResponseObject is the result for one object of a BatchRequest. Either
// Actions or Error is set, unless the object needs no transfer.
type BatchResponseObject struct {
	OID           string            `json:"oid"`
	Size          int64             `json:"size"`
	Authenticated bool              `json:"authenticated,omitempty"`
	Actions       map[string]Action `json:"actions,omitempty"`
	Error         *ObjectError      `json:"error,omitempty"`
}

// Action tells the client where and how to transfer an object.
type Action struct {
	Href   string            `json:"href"`
	Header map[string]string `json:"header,omitempty"`
}

// ObjectError is the error of one object in a BatchResponse.
type ObjectError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// VerifyRequest is the request payload of the verify endpoint.
type VerifyRequest struct {
	OID  string `json:"oid"`
	Size int64  `json:"size"`
}

// Error is a client error that the caller should respond with, using Code as
// the HTTP status code.
type Error struct {
	Code    int
	Message string
}

func (e *Error) Error() string {
	return e.Message
}

// Object is a verified LFS object.
type Object struct {
	OID  string
	Size int64
	// URI is the location of the object content, as returned by the storage
	// backend that stores it.
	URI string
}

// Store is the metadata layer of LFS objects. An object is "pending" from the
// verification of a presigned upload until the verification succeeds, and only
// verified objects are served. A repository is identified by an opaque string
// chosen by the caller.
type Store interface {
	// GetRepoObjects returns the verified objects among oids that are linked to
	// repo, keyed by OID. Missing OIDs are omitted.
	GetRepoObjects(ctx context.Context, repo string, oids []string) (map[string]Object, error)
	// LinkObject records a verified object whose content is stored at uri and
	// links it to repo. It returns the URI the object is recorded with, and when
	// that differs from uri, the caller deletes the content at uri as no longer
	// referenced.
	LinkObject(ctx context.Context, repo, oid string, size int64, uri string) (storedURI string, err error)
	// InsertPendingObject records a pending object for oid, unless the object is
	// already recorded. It is called before the content of a presigned upload is
	// copied to the canonical location of the object, so that the content can be
	// cleaned up when the verification does not complete.
	InsertPendingObject(ctx context.Context, oid string) error
	// VerifyObject marks the pending object for oid as verified with content
	// stored at uri and links it to repo. It returns the URI the object is
	// recorded with, like LinkObject. It fails when no object is recorded for
	// oid.
	VerifyObject(ctx context.Context, repo, oid string, size int64, uri string) (storedURI string, err error)
}
