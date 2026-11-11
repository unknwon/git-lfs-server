// Package lfs implements the server side of the Git LFS Batch API and the
// basic transfer adapter on top of pluggable metadata and object storage.
//
// It leaves HTTP routing, authentication, authorization, and the encoding of
// responses to the caller, so that servers with different URL layouts,
// credentials, and databases can share the transfer logic.
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
// start of a presigned upload until it is verified, and only verified objects
// are served. A repository is identified by an opaque string chosen by the
// caller.
type Store interface {
	// GetRepoObjects returns the verified objects among oids that are linked to
	// repo, keyed by OID. Missing OIDs are omitted.
	GetRepoObjects(ctx context.Context, repo string, oids []string) (map[string]Object, error)
	// GetObjects returns the verified objects among oids regardless of the
	// repositories they are linked to, keyed by OID. Missing OIDs are omitted.
	// It is only called when Service.CrossRepoDedup is true.
	GetObjects(ctx context.Context, oids []string) (map[string]Object, error)
	// LinkObject records a verified object whose content is stored at uri and
	// links it to repo. It returns the URI the object is recorded with, and when
	// that differs from uri, the caller deletes the content at uri as no longer
	// referenced.
	LinkObject(ctx context.Context, repo, oid string, size int64, uri string) (storedURI string, err error)
	// InsertPendingObject records a pending object for oid, unless the object is
	// already recorded, so that abandoned presigned uploads can be cleaned up.
	InsertPendingObject(ctx context.Context, oid string) error
	// VerifyObject marks the pending object for oid as verified with content
	// stored at uri and links it to repo. It returns the URI the object is
	// recorded with, like LinkObject. It fails when no object is recorded for
	// oid.
	VerifyObject(ctx context.Context, repo, oid string, size int64, uri string) (storedURI string, err error)
}
