package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strconv"

	"github.com/cockroachdb/errors"
	"github.com/flamego/flamego"

	"unknwon.dev/git-lfs-server/internal/database"
	"unknwon.dev/git-lfs-server/internal/forge"
	"unknwon.dev/git-lfs-server/internal/iox"
	"unknwon.dev/git-lfs-server/internal/logx"
	"unknwon.dev/git-lfs-server/internal/ptrx"
	"unknwon.dev/git-lfs-server/internal/storage"
)

// oidPattern matches the SHA-256 hex digest format used as the LFS object ID:
// exactly 64 lowercase hex characters.
var oidPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

const (
	operationUpload   = "upload"
	operationDownload = "download"
	transferBasic     = "basic"
	mediaTypeLFS      = "application/vnd.git-lfs+json"

	// maxJSONRequestBytes caps JSON request bodies (batch, verify) to prevent
	// authenticated callers from exhausting memory with oversized payloads.
	maxJSONRequestBytes = 10 << 20
)

type batchRequest struct {
	Operation string        `json:"operation"`
	Transfers []string      `json:"transfers,omitempty"`
	Objects   []batchObject `json:"objects"`
}

type batchObject struct {
	OID  string `json:"oid"`
	Size int64  `json:"size"`
}

type batchResponse struct {
	Transfer string                `json:"transfer"`
	Objects  []batchResponseObject `json:"objects"`
}

type batchResponseObject struct {
	OID           string                 `json:"oid"`
	Size          int64                  `json:"size"`
	Authenticated bool                   `json:"authenticated,omitempty"`
	Actions       map[string]batchAction `json:"actions,omitempty"`
	Error         *batchObjectError      `json:"error,omitempty"`
}

type batchAction struct {
	Href   string            `json:"href"`
	Header map[string]string `json:"header,omitempty"`
}

type batchObjectError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type verifyRequest struct {
	OID  string `json:"oid"`
	Size int64  `json:"size"`
}

func serveBatch(db *database.DB, externalURL string, maxObjectSize int64) flamego.Handler {
	return func(c flamego.Context, logger *logx.Logger, perm forge.Permission) {
		logger = logger.Scoped("batch")
		ctx := c.Request().Context()

		body := http.MaxBytesReader(c.ResponseWriter(), c.Request().Body().ReadCloser(), maxJSONRequestBytes)
		defer func() { _ = body.Close() }()

		var req batchRequest
		if err := json.NewDecoder(body).Decode(&req); err != nil {
			writeBatchError(c, http.StatusBadRequest, fmt.Sprintf("invalid JSON body: %v", err))
			return
		}

		switch req.Operation {
		case operationUpload:
			if perm != forge.PermissionWrite {
				writeBatchError(c, http.StatusForbidden, "write permission required for upload")
				return
			}
		case operationDownload:
			if perm != forge.PermissionRead && perm != forge.PermissionWrite {
				writeBatchError(c, http.StatusForbidden, "read permission required for download")
				return
			}
		default:
			writeBatchError(c, http.StatusBadRequest, `operation must be "upload" or "download"`)
			return
		}

		if len(req.Transfers) > 0 && !slices.Contains(req.Transfers, transferBasic) {
			writeBatchError(c, http.StatusUnprocessableEntity, `only "basic" transfer is supported`)
			return
		}

		for _, o := range req.Objects {
			if !oidPattern.MatchString(o.OID) {
				writeBatchError(c, http.StatusBadRequest, fmt.Sprintf("invalid oid %q", o.OID))
				return
			}
		}

		repoName := repoNameFromContext(c)

		var actionHeader map[string]string
		if authz := c.Request().Header.Get("Authorization"); authz != "" {
			actionHeader = map[string]string{"Authorization": authz}
		}

		oids := make([]string, len(req.Objects))
		for i, o := range req.Objects {
			oids[i] = o.OID
		}

		var (
			existing map[string]database.Object
			err      error
		)
		switch req.Operation {
		case operationDownload:
			existing, err = db.GetRepoObjectsByOIDs(ctx, repoName, oids)
		case operationUpload:
			existing, err = db.GetObjectsByOIDs(ctx, oids)
		}
		if err != nil {
			logger.ErrorContext(ctx, "Failed to look up objects", "error", err)
			writeBatchError(c, http.StatusInternalServerError, "failed to process batch")
			return
		}

		out := batchResponse{
			Transfer: transferBasic,
			Objects:  make([]batchResponseObject, 0, len(req.Objects)),
		}
		for _, in := range req.Objects {
			obj, ok := existing[in.OID]
			respObj := batchResponseObject{OID: in.OID, Size: in.Size, Authenticated: true}

			switch req.Operation {
			case operationDownload:
				switch {
				case !ok:
					respObj.Error = &batchObjectError{Code: http.StatusNotFound, Message: "object does not exist"}
				case ptrx.Deref(obj.Size, -1) != in.Size:
					respObj.Error = &batchObjectError{Code: http.StatusUnprocessableEntity, Message: "size mismatch"}
				default:
					respObj.Actions = map[string]batchAction{
						"download": {Href: objectHref(externalURL, repoName, in.OID), Header: actionHeader},
					}
				}
			case operationUpload:
				switch {
				case maxObjectSize > 0 && in.Size > maxObjectSize:
					respObj.Error = &batchObjectError{
						Code:    http.StatusRequestEntityTooLarge,
						Message: fmt.Sprintf("object size %d exceeds limit %d", in.Size, maxObjectSize),
					}
				case ok && ptrx.Deref(obj.Size, -1) == in.Size:
					// Cross-repo dedup: object is verified globally, ensure
					// the calling repo is linked so future downloads succeed.
					// COALESCE in LinkObject's upsert preserves the existing
					// object_uri, so the placeholder is never written.
					if _, err := db.LinkObject(ctx, repoName, in.OID, in.Size, "ignored"); err != nil {
						logger.ErrorContext(ctx, "Failed to link repo to existing object", "oid", in.OID, "error", err)
						respObj.Error = &batchObjectError{Code: http.StatusInternalServerError, Message: "failed to link object to repo"}
					}
				case ok && ptrx.Deref(obj.Size, -1) != in.Size:
					respObj.Error = &batchObjectError{Code: http.StatusConflict, Message: "OID exists with different size"}
				default:
					respObj.Actions = map[string]batchAction{
						"upload": {Href: objectHref(externalURL, repoName, in.OID), Header: actionHeader},
						"verify": {Href: verifyHref(externalURL, repoName), Header: actionHeader},
					}
				}
			}
			out.Objects = append(out.Objects, respObj)
		}

		c.ResponseWriter().Header().Set("Content-Type", mediaTypeLFS)
		if err := json.NewEncoder(c.ResponseWriter()).Encode(out); err != nil {
			logger.ErrorContext(ctx, "Failed to encode batch response", "error", err)
		}
	}
}

func serveUpload(db *database.DB, storages map[string]storage.Backend, maxObjectSize int64) flamego.Handler {
	return func(c flamego.Context, logger *logx.Logger, perm forge.Permission) {
		logger = logger.Scoped("upload")
		ctx := c.Request().Context()

		if perm != forge.PermissionWrite {
			http.Error(c.ResponseWriter(), "write permission required for upload", http.StatusForbidden)
			return
		}

		oid := c.Param("oid")
		if !oidPattern.MatchString(oid) {
			http.Error(c.ResponseWriter(), "invalid oid", http.StatusBadRequest)
			return
		}

		size := c.Request().ContentLength
		if size < 0 {
			http.Error(c.ResponseWriter(), "Content-Length is required", http.StatusLengthRequired)
			return
		}
		if maxObjectSize > 0 && size > maxObjectSize {
			http.Error(c.ResponseWriter(), fmt.Sprintf("object size %d exceeds limit %d", size, maxObjectSize), http.StatusRequestEntityTooLarge)
			return
		}

		backend, ok := storages[hostFromContext(c)]
		if !ok {
			logger.ErrorContext(ctx, "No storage backend configured for host")
			http.Error(c.ResponseWriter(), http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
			return
		}

		switch b := backend.(type) {
		case storage.Proxier:
			serveUploadProxy(c, logger, db, b, oid, size)
		case storage.Presigner:
			serveUploadPresign(c, logger, db, b, oid, size)
		default:
			logger.ErrorContext(ctx, "Storage backend does not implement a known transfer mode", "type", fmt.Sprintf("%T", backend))
			http.Error(c.ResponseWriter(), http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		}
	}
}

func serveUploadProxy(c flamego.Context, logger *logx.Logger, db *database.DB, backend storage.Proxier, oid string, size int64) {
	ctx := c.Request().Context()

	body := c.Request().Body().ReadCloser()
	defer func() { _ = body.Close() }()

	reader := iox.NewSHA256Reader(iox.NewExactSizeReader(body, size), oid)

	uri, err := backend.Put(ctx, oid, reader)
	if err != nil {
		var sizeErr *iox.SizeMismatchError
		var hashErr *iox.SHA256MismatchError
		switch {
		case errors.As(err, &sizeErr):
			http.Error(c.ResponseWriter(), sizeErr.Error(), http.StatusUnprocessableEntity)
		case errors.As(err, &hashErr):
			http.Error(c.ResponseWriter(), hashErr.Error(), http.StatusUnprocessableEntity)
		default:
			logger.ErrorContext(ctx, "Failed to store object", "oid", oid, "error", err)
			http.Error(c.ResponseWriter(), http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		}
		return
	}

	repoName := repoNameFromContext(c)
	storedURI, err := db.LinkObject(ctx, repoName, oid, size, uri)
	if err != nil {
		logger.ErrorContext(ctx, "Failed to link object", "oid", oid, "error", err)
		http.Error(c.ResponseWriter(), http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	if storedURI != uri {
		if delErr := backend.Delete(ctx, uri); delErr != nil {
			logger.ErrorContext(ctx, "Failed to delete orphan object", "uri", uri, "error", delErr)
		}
	}

	c.ResponseWriter().WriteHeader(http.StatusOK)
}

func serveUploadPresign(c flamego.Context, logger *logx.Logger, db *database.DB, backend storage.Presigner, oid string, size int64) {
	ctx := c.Request().Context()

	existing, err := db.GetObjectByOID(ctx, oid)
	if err != nil && !errors.Is(err, database.ErrObjectNotFound) {
		logger.ErrorContext(ctx, "Failed to look up object", "oid", oid, "error", err)
		http.Error(c.ResponseWriter(), http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	if existing != nil && existing.VerifiedAt != nil {
		c.ResponseWriter().WriteHeader(http.StatusOK)
		return
	}

	if err := db.InsertPendingObject(ctx, oid); err != nil {
		logger.ErrorContext(ctx, "Failed to insert pending object", "oid", oid, "error", err)
		http.Error(c.ResponseWriter(), http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}

	url, headers, err := backend.PresignPut(ctx, oid, size)
	if err != nil {
		logger.ErrorContext(ctx, "Failed to presign upload", "oid", oid, "error", err)
		http.Error(c.ResponseWriter(), http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}

	w := c.ResponseWriter()
	for k, v := range headers {
		w.Header().Set(k, v)
	}
	w.Header().Set("Location", url)
	w.WriteHeader(http.StatusTemporaryRedirect)
}

func serveDownload(db *database.DB, storages map[string]any) flamego.Handler {
	return func(c flamego.Context, logger *logx.Logger, perm forge.Permission) {
		logger = logger.Scoped("download")
		ctx := c.Request().Context()

		if perm != forge.PermissionRead && perm != forge.PermissionWrite {
			http.Error(c.ResponseWriter(), "read permission required for download", http.StatusForbidden)
			return
		}

		oid := c.Param("oid")
		if !oidPattern.MatchString(oid) {
			http.Error(c.ResponseWriter(), "invalid oid", http.StatusBadRequest)
			return
		}

		backend, ok := storages[hostFromContext(c)]
		if !ok {
			logger.ErrorContext(ctx, "No storage backend configured for host")
			http.Error(c.ResponseWriter(), http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
			return
		}

		repoName := repoNameFromContext(c)
		object, err := db.GetRepoObjectByOID(ctx, repoName, oid)
		if err != nil {
			if errors.Is(err, database.ErrObjectNotFound) {
				http.Error(c.ResponseWriter(), "object does not exist", http.StatusNotFound)
				return
			}
			logger.ErrorContext(ctx, "Failed to look up object", "oid", oid, "error", err)
			http.Error(c.ResponseWriter(), http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
			return
		}
		// GetRepoObjectByOID filters verified rows, so Size and ObjectURI are
		// guaranteed non-nil here by the schema CHECK.
		objectURI := ptrx.Deref(object.ObjectURI, "")
		objectSize := ptrx.Deref(object.Size, -1)
		if objectURI == "" || objectSize < 0 {
			logger.ErrorContext(ctx, "Verified object has nil size or uri (schema violation)", "oid", oid)
			http.Error(c.ResponseWriter(), http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
			return
		}

		switch b := backend.(type) {
		case storage.Proxier:
			serveDownloadProxy(c, logger, b, oid, objectURI, objectSize)
		case storage.Presigner:
			serveDownloadPresign(c, logger, b, oid, objectURI)
		default:
			logger.ErrorContext(ctx, "Storage backend does not implement a known transfer mode", "type", fmt.Sprintf("%T", backend))
			http.Error(c.ResponseWriter(), http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		}
	}
}

func serveDownloadProxy(c flamego.Context, logger *logx.Logger, backend storage.Proxier, oid, uri string, size int64) {
	ctx := c.Request().Context()

	rc, err := backend.Open(ctx, uri)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			logger.ErrorContext(ctx, "DB row exists but storage object is missing", "oid", oid, "uri", uri)
		} else {
			logger.ErrorContext(ctx, "Failed to open object", "oid", oid, "error", err)
		}
		http.Error(c.ResponseWriter(), http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	defer func() { _ = rc.Close() }()

	w := c.ResponseWriter()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.WriteHeader(http.StatusOK)

	if _, err := io.Copy(w, rc); err != nil {
		logger.ErrorContext(ctx, "Failed to stream object", "oid", oid, "error", err)
	}
}

func serveDownloadPresign(c flamego.Context, logger *logx.Logger, backend storage.Presigner, oid, uri string) {
	ctx := c.Request().Context()

	url, err := backend.PresignGet(ctx, uri)
	if err != nil {
		logger.ErrorContext(ctx, "Failed to presign download", "oid", oid, "error", err)
		http.Error(c.ResponseWriter(), http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}

	w := c.ResponseWriter()
	w.Header().Set("Location", url)
	w.WriteHeader(http.StatusTemporaryRedirect)
}

func serveVerify(db *database.DB, storages map[string]any) flamego.Handler {
	return func(c flamego.Context, logger *logx.Logger, perm forge.Permission) {
		logger = logger.Scoped("verify")
		ctx := c.Request().Context()

		if perm != forge.PermissionWrite {
			http.Error(c.ResponseWriter(), "write permission required for verify", http.StatusForbidden)
			return
		}

		body := http.MaxBytesReader(c.ResponseWriter(), c.Request().Body().ReadCloser(), maxJSONRequestBytes)
		defer func() { _ = body.Close() }()

		var req verifyRequest
		if err := json.NewDecoder(body).Decode(&req); err != nil {
			http.Error(c.ResponseWriter(), fmt.Sprintf("invalid JSON body: %v", err), http.StatusBadRequest)
			return
		}
		if !oidPattern.MatchString(req.OID) {
			http.Error(c.ResponseWriter(), "invalid oid", http.StatusBadRequest)
			return
		}

		backend, ok := storages[hostFromContext(c)]
		if !ok {
			logger.ErrorContext(ctx, "No storage backend configured for host")
			http.Error(c.ResponseWriter(), http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
			return
		}

		switch b := backend.(type) {
		case storage.Proxier:
			serveVerifyProxy(c, logger, db, req)
		case storage.Presigner:
			serveVerifyPresign(c, logger, db, b, req)
		default:
			logger.ErrorContext(ctx, "Storage backend does not implement a known transfer mode", "type", fmt.Sprintf("%T", backend))
			http.Error(c.ResponseWriter(), http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		}
	}
}

func serveVerifyProxy(c flamego.Context, logger *logx.Logger, db *database.DB, req verifyRequest) {
	ctx := c.Request().Context()

	repoName := repoNameFromContext(c)
	object, err := db.GetRepoObjectByOID(ctx, repoName, req.OID)
	if err != nil {
		if errors.Is(err, database.ErrObjectNotFound) {
			http.Error(c.ResponseWriter(), "object does not exist", http.StatusNotFound)
			return
		}
		logger.ErrorContext(ctx, "Failed to look up object", "oid", req.OID, "error", err)
		http.Error(c.ResponseWriter(), http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	if ptrx.Deref(object.Size, -1) != req.Size {
		http.Error(c.ResponseWriter(), "size mismatch", http.StatusUnprocessableEntity)
		return
	}

	c.ResponseWriter().WriteHeader(http.StatusNoContent)
}

func serveVerifyPresign(c flamego.Context, logger *logx.Logger, db *database.DB, backend storage.Presigner, req verifyRequest) {
	ctx := c.Request().Context()

	uri := backend.URI(req.OID)
	headSize, err := backend.Head(ctx, uri)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			http.Error(c.ResponseWriter(), "object does not exist in storage", http.StatusUnprocessableEntity)
			return
		}
		logger.ErrorContext(ctx, "Failed to head object", "oid", req.OID, "error", err)
		http.Error(c.ResponseWriter(), http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	// R2's signed Content-Length should make this impossible, but treat any
	// mismatch as a hard error rather than trusting either side.
	if headSize != req.Size {
		http.Error(c.ResponseWriter(), "size mismatch", http.StatusUnprocessableEntity)
		return
	}

	repoName := repoNameFromContext(c)
	storedURI, err := db.VerifyObject(ctx, repoName, req.OID, req.Size, uri)
	if err != nil {
		logger.ErrorContext(ctx, "Failed to mark object verified", "oid", req.OID, "error", err)
		http.Error(c.ResponseWriter(), http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	if storedURI != uri {
		if delErr := backend.Delete(ctx, uri); delErr != nil {
			logger.ErrorContext(ctx, "Failed to delete orphan object", "uri", uri, "error", delErr)
		}
	}

	c.ResponseWriter().WriteHeader(http.StatusNoContent)
}

func objectHref(externalURL, repoName, oid string) string {
	return fmt.Sprintf("%s/%s/info/lfs/objects/%s", externalURL, repoName, oid)
}

func verifyHref(externalURL, repoName string) string {
	return fmt.Sprintf("%s/%s/info/lfs/objects/verify", externalURL, repoName)
}

func writeBatchError(c flamego.Context, code int, msg string) {
	c.ResponseWriter().Header().Set("Content-Type", mediaTypeLFS)
	c.ResponseWriter().WriteHeader(code)
	_ = json.NewEncoder(c.ResponseWriter()).Encode(map[string]string{"message": msg})
}
