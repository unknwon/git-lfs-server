package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/cockroachdb/errors"
	"github.com/flamego/flamego"

	"unknwon.dev/git-lfs-server/internal/forge"
	"unknwon.dev/git-lfs-server/internal/logx"
	"unknwon.dev/git-lfs-server/lfs"
)

const (
	mediaTypeLFS = "application/vnd.git-lfs+json"

	// maxJSONRequestBytes caps JSON request bodies (batch, verify) to prevent
	// authenticated callers from exhausting memory with oversized payloads.
	maxJSONRequestBytes = 10 << 20
)

// mapService maps the *lfs.Service of the forge host to the request context.
func mapService(services map[string]*lfs.Service) flamego.Handler {
	return func(c flamego.Context, logger *logx.Logger) {
		service, ok := services[hostFromContext(c)]
		if !ok {
			logger.ErrorContext(c.Request().Context(), "No storage backend configured for host")
			writeLFSError(c.ResponseWriter(), http.StatusInternalServerError, http.StatusText(http.StatusInternalServerError))
			return
		}
		c.Map(service)
	}
}

func serveBatch(externalURL string) flamego.Handler {
	return func(c flamego.Context, logger *logx.Logger, perm forge.Permission, service *lfs.Service) {
		logger = logger.Scoped("batch")
		ctx := c.Request().Context()

		body := http.MaxBytesReader(c.ResponseWriter(), c.Request().Body().ReadCloser(), maxJSONRequestBytes)
		defer func() { _ = body.Close() }()

		var req lfs.BatchRequest
		if err := json.NewDecoder(body).Decode(&req); err != nil {
			writeLFSError(c.ResponseWriter(), http.StatusBadRequest, fmt.Sprintf("invalid JSON body: %v", err))
			return
		}

		switch req.Operation {
		case lfs.OperationUpload:
			if perm != forge.PermissionWrite {
				writeLFSError(c.ResponseWriter(), http.StatusForbidden, "write permission required for upload")
				return
			}
		case lfs.OperationDownload:
			if perm != forge.PermissionRead && perm != forge.PermissionWrite {
				writeLFSError(c.ResponseWriter(), http.StatusForbidden, "read permission required for download")
				return
			}
		default:
			writeLFSError(c.ResponseWriter(), http.StatusBadRequest, `operation must be "upload" or "download"`)
			return
		}

		repoName := repoNameFromContext(c)
		endpoint := lfs.Endpoint{
			Href: fmt.Sprintf("%s/%s/info/lfs/objects", externalURL, repoName),
			// Every request passes the authorize middleware with Basic Auth, so
			// forwarding the header authenticates the actions.
			Authenticated: true,
		}
		if authz := c.Request().Header.Get("Authorization"); authz != "" {
			endpoint.Header = map[string]string{"Authorization": authz}
		}

		resp, err := service.Batch(ctx, logger.Logger, repoName, endpoint, req)
		if err != nil {
			writeServiceError(ctx, logger, c.ResponseWriter(), err, "Failed to process batch", "failed to process batch")
			return
		}

		c.ResponseWriter().Header().Set("Content-Type", mediaTypeLFS)
		if err := json.NewEncoder(c.ResponseWriter()).Encode(resp); err != nil {
			logger.ErrorContext(ctx, "Failed to encode batch response", "error", err)
		}
	}
}

func serveUpload(c flamego.Context, logger *logx.Logger, perm forge.Permission, service *lfs.Service) {
	logger = logger.Scoped("upload")
	ctx := c.Request().Context()

	if perm != forge.PermissionWrite {
		writeLFSError(c.ResponseWriter(), http.StatusForbidden, "write permission required for upload")
		return
	}

	body := c.Request().Body().ReadCloser()
	defer func() { _ = body.Close() }()

	oid := c.Param("oid")
	result, err := service.Upload(ctx, logger.Logger, repoNameFromContext(c), oid, c.Request().ContentLength, body)
	if err != nil {
		writeServiceError(ctx, logger.With("oid", oid), c.ResponseWriter(), err, "Failed to upload object", http.StatusText(http.StatusInternalServerError))
		return
	}

	w := c.ResponseWriter()
	switch {
	case result.Redirect != nil:
		for k, v := range result.Redirect.Header {
			w.Header().Set(k, v)
		}
		w.Header().Set("Location", result.Redirect.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
	case result.Existed:
		w.WriteHeader(http.StatusOK)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func serveDownload(c flamego.Context, logger *logx.Logger, perm forge.Permission, service *lfs.Service) {
	logger = logger.Scoped("download")
	ctx := c.Request().Context()

	if perm != forge.PermissionRead && perm != forge.PermissionWrite {
		writeLFSError(c.ResponseWriter(), http.StatusForbidden, "read permission required for download")
		return
	}

	oid := c.Param("oid")
	result, err := service.Download(ctx, repoNameFromContext(c), oid)
	if err != nil {
		writeServiceError(ctx, logger.With("oid", oid), c.ResponseWriter(), err, "Failed to download object", http.StatusText(http.StatusInternalServerError))
		return
	}

	w := c.ResponseWriter()
	if result.Redirect != nil {
		w.Header().Set("Location", result.Redirect.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
		return
	}

	defer func() { _ = result.Content.Close() }()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(result.Size, 10))
	w.WriteHeader(http.StatusOK)
	if _, err = io.Copy(w, result.Content); err != nil {
		logger.ErrorContext(ctx, "Failed to stream object", "oid", oid, "error", err)
	}
}

func serveVerify(c flamego.Context, logger *logx.Logger, perm forge.Permission, service *lfs.Service) {
	logger = logger.Scoped("verify")
	ctx := c.Request().Context()

	if perm != forge.PermissionWrite {
		writeLFSError(c.ResponseWriter(), http.StatusForbidden, "write permission required for verify")
		return
	}

	body := http.MaxBytesReader(c.ResponseWriter(), c.Request().Body().ReadCloser(), maxJSONRequestBytes)
	defer func() { _ = body.Close() }()

	var req lfs.VerifyRequest
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		writeLFSError(c.ResponseWriter(), http.StatusBadRequest, fmt.Sprintf("invalid JSON body: %v", err))
		return
	}

	if err := service.Verify(ctx, logger.Logger, repoNameFromContext(c), req); err != nil {
		writeServiceError(ctx, logger.With("oid", req.OID), c.ResponseWriter(), err, "Failed to verify object", http.StatusText(http.StatusInternalServerError))
		return
	}
	c.ResponseWriter().WriteHeader(http.StatusNoContent)
}

// writeServiceError responds with the client error carried by err as an
// *lfs.Error. Any other error is internal, so it is logged with logMessage and
// the response only carries internalMessage.
func writeServiceError(ctx context.Context, logger *logx.Logger, w http.ResponseWriter, err error, logMessage, internalMessage string) {
	var lfsErr *lfs.Error
	if errors.As(err, &lfsErr) {
		writeLFSError(w, lfsErr.Code, lfsErr.Message)
		return
	}
	logger.ErrorContext(ctx, logMessage, "error", err)
	writeLFSError(w, http.StatusInternalServerError, internalMessage)
}

// writeLFSError writes an error response in the format required by the LFS
// Batch API spec: a JSON body of {"message": "..."} with the
// application/vnd.git-lfs+json content type. All LFS endpoints (batch, upload,
// download, verify, and the auth middleware that fronts them) must use this
// helper instead of http.Error so clients get a parseable body and the right
// content type on every error.
func writeLFSError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", mediaTypeLFS)
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"message": msg})
}
