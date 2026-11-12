package lfs

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path"
	"strconv"

	"github.com/cockroachdb/errors"
)

const (
	// MediaType is the content type of LFS API requests and responses.
	MediaType = "application/vnd.git-lfs+json"

	// maxJSONRequestBytes caps JSON request bodies (batch, verify) to prevent
	// authenticated callers from exhausting memory with oversized payloads.
	maxJSONRequestBytes = 10 << 20
)

// Access is what the caller resolved for a request after authenticating it.
type Access struct {
	// Repo identifies the repository to the Store.
	Repo string
	// Href is the URL that the transfer endpoints of the repository are served
	// under, without a trailing slash: objects at Href/{oid} and verifications
	// at Href/verify.
	Href string
	// CanRead allows downloads.
	CanRead bool
	// CanWrite allows uploads and verifications.
	CanWrite bool
}

// Serve handles an LFS API request to the repository in access. It dispatches
// on the method and the last segment of the URL path, so it serves any URL
// layout whose endpoints end in "batch", "verify", and the OID:
//
//   - POST .../batch
//   - GET .../{oid}
//   - PUT .../{oid}
//   - POST .../verify
//
// The request's Authorization header is forwarded in the actions of batch
// responses, so the client authenticates transfers with the same credentials.
func (s *Service) Serve(logger *slog.Logger, w http.ResponseWriter, r *http.Request, access Access) {
	name := path.Base(r.URL.Path)
	switch {
	case r.Method == http.MethodPost && name == "batch":
		s.serveBatch(logger, w, r, access)
	case r.Method == http.MethodPost && name == "verify":
		s.serveVerify(logger, w, r, access)
	case r.Method == http.MethodGet:
		s.serveDownload(logger, w, r, access, name)
	case r.Method == http.MethodPut:
		s.serveUpload(logger, w, r, access, name)
	default:
		WriteError(w, http.StatusMethodNotAllowed, http.StatusText(http.StatusMethodNotAllowed))
	}
}

func (s *Service) serveBatch(logger *slog.Logger, w http.ResponseWriter, r *http.Request, access Access) {
	ctx := r.Context()

	body := http.MaxBytesReader(w, r.Body, maxJSONRequestBytes)
	defer func() { _ = body.Close() }()

	var req BatchRequest
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		WriteError(w, http.StatusBadRequest, fmt.Sprintf("invalid JSON body: %v", err))
		return
	}

	switch req.Operation {
	case OperationUpload:
		if !access.CanWrite {
			WriteError(w, http.StatusForbidden, "write permission required for upload")
			return
		}
	case OperationDownload:
		if !access.CanRead {
			WriteError(w, http.StatusForbidden, "read permission required for download")
			return
		}
	default:
		WriteError(w, http.StatusBadRequest, `operation must be "upload" or "download"`)
		return
	}

	endpoint := Endpoint{Href: access.Href}
	if authz := r.Header.Get("Authorization"); authz != "" {
		endpoint.Header = map[string]string{"Authorization": authz}
		endpoint.Authenticated = true
	}
	resp, err := s.Batch(ctx, logger, access.Repo, endpoint, req)
	if err != nil {
		writeServiceError(logger, w, r, err, "failed to process batch")
		return
	}

	w.Header().Set("Content-Type", MediaType)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		logger.ErrorContext(ctx, "Failed to encode batch response", "error", err)
	}
}

func (s *Service) serveUpload(logger *slog.Logger, w http.ResponseWriter, r *http.Request, access Access, oid string) {
	if !access.CanWrite {
		WriteError(w, http.StatusForbidden, "write permission required for upload")
		return
	}

	defer func() { _ = r.Body.Close() }()
	result, err := s.Upload(r.Context(), logger, access.Repo, oid, r.ContentLength, r.Body)
	if err != nil {
		writeServiceError(logger.With("oid", oid), w, r, err, http.StatusText(http.StatusInternalServerError))
		return
	}

	switch {
	case result.Redirect != nil:
		writeRedirect(w, result.Redirect)
	case result.Existed:
		w.WriteHeader(http.StatusOK)
	default:
		s.addProxiedBytes(r.Context(), OperationUpload, r.ContentLength)
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Service) serveDownload(logger *slog.Logger, w http.ResponseWriter, r *http.Request, access Access, oid string) {
	if !access.CanRead {
		WriteError(w, http.StatusForbidden, "read permission required for download")
		return
	}

	result, err := s.Download(r.Context(), access.Repo, oid)
	if err != nil {
		writeServiceError(logger.With("oid", oid), w, r, err, http.StatusText(http.StatusInternalServerError))
		return
	}
	if result.Redirect != nil {
		writeRedirect(w, result.Redirect)
		return
	}

	defer func() { _ = result.Content.Close() }()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(result.Size, 10))
	w.WriteHeader(http.StatusOK)
	// Counted here rather than by wrapping Content, which would stop io.Copy
	// from using sendfile for files.
	n, err := io.Copy(w, result.Content)
	s.addProxiedBytes(r.Context(), OperationDownload, n)
	if err != nil {
		logger.ErrorContext(r.Context(), "Failed to stream object", "oid", oid, "error", err)
	}
}

func (s *Service) serveVerify(logger *slog.Logger, w http.ResponseWriter, r *http.Request, access Access) {
	if !access.CanWrite {
		WriteError(w, http.StatusForbidden, "write permission required for verify")
		return
	}

	body := http.MaxBytesReader(w, r.Body, maxJSONRequestBytes)
	defer func() { _ = body.Close() }()

	var req VerifyRequest
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		WriteError(w, http.StatusBadRequest, fmt.Sprintf("invalid JSON body: %v", err))
		return
	}

	if err := s.Verify(r.Context(), logger, access.Repo, req); err != nil {
		writeServiceError(logger.With("oid", req.OID), w, r, err, http.StatusText(http.StatusInternalServerError))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeRedirect(w http.ResponseWriter, redirect *Redirect) {
	for k, v := range redirect.Header {
		w.Header().Set(k, v)
	}
	w.Header().Set("Location", redirect.URL)
	w.WriteHeader(http.StatusTemporaryRedirect)
}

// writeServiceError responds with the client error carried by err as an
// *Error. Any other error is internal, so it is logged and the response only
// carries internalMessage.
func writeServiceError(logger *slog.Logger, w http.ResponseWriter, r *http.Request, err error, internalMessage string) {
	var lfsErr *Error
	if errors.As(err, &lfsErr) {
		WriteError(w, lfsErr.Code, lfsErr.Message)
		return
	}
	logger.ErrorContext(r.Context(), "Failed to serve LFS request", "path", r.URL.Path, "error", err)
	WriteError(w, http.StatusInternalServerError, internalMessage)
}

// WriteError writes an error response in the format required by the LFS API: a
// JSON body of {"message": "..."} with the MediaType content type. Callers use
// it for the errors of their own middleware, e.g., authentication, so clients
// get a parseable body on every error.
func WriteError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", MediaType)
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"message": msg})
}
