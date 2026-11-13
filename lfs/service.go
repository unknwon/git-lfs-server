package lfs

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/cockroachdb/errors"
	"github.com/google/uuid"

	"unknwon.dev/git-lfs-server/lfs/internal/iox"
	"unknwon.dev/git-lfs-server/lfs/storage"
)

// Service serves LFS operations for repositories. Callers authenticate and
// authorize each request, then either hand it to Serve, or call the operation
// methods and respond with the Code and Message of a returned *Error. Any other
// error returned by an operation method is an internal error.
type Service struct {
	Store Store
	// Storage receives uploads and decides the transfer mode: content is
	// proxied through the caller for a storage.Proxier, and transferred
	// directly by the client through redirects for a storage.Presigner.
	Storage storage.Backend
	// PreviousStorages are consulted for downloads of objects stored before
	// Storage was changed. An object is served by the first backend, starting
	// with Storage, whose scheme prefixes the object's URI.
	PreviousStorages []storage.Backend
	// MaxObjectSize is the maximum size in bytes of an uploaded object. Zero or
	// negative means no limit.
	MaxObjectSize int64
}

// Endpoint describes how the client reaches the transfer endpoints of a
// repository: objects at Href/{oid} and verifications at Href/verify. For a
// storage.Presigner, the upload and verify actions also carry an upload ID in
// the "upload" query parameter, which the caller passes to Service.Upload and
// Service.Verify.
type Endpoint struct {
	Href string
	// Header is included in every action, e.g., to forward the credentials of
	// the batch request.
	Header map[string]string
	// Authenticated tells the client that Header authenticates the actions, so
	// it does not look up credentials for them. It is required when Storage is a
	// storage.Presigner: otherwise the Git LFS client looks up credentials for
	// the host of the presigned URL it is redirected to, and fails.
	Authenticated bool
}

// Redirect sends the client to another location to transfer the content.
type Redirect struct {
	URL string
	// Header must be sent by the client verbatim.
	Header map[string]string
}

// Batch answers a batch request for repo. Callers must check that the actor is
// allowed to perform req.Operation before calling it.
func (s *Service) Batch(ctx context.Context, logger *slog.Logger, repo string, endpoint Endpoint, req BatchRequest) (*BatchResponse, error) {
	if req.Operation != OperationUpload && req.Operation != OperationDownload {
		return nil, &Error{Code: http.StatusBadRequest, Message: `operation must be "upload" or "download"`}
	}
	if len(req.Transfers) > 0 && !slices.Contains(req.Transfers, TransferBasic) {
		return nil, &Error{Code: http.StatusUnprocessableEntity, Message: `only "basic" transfer is supported`}
	}

	oids := make([]string, len(req.Objects))
	for i, o := range req.Objects {
		if !ValidOID(o.OID) {
			return nil, &Error{Code: http.StatusBadRequest, Message: fmt.Sprintf("invalid oid %q", o.OID)}
		}
		oids[i] = o.OID
	}

	existing, err := s.Store.GetRepoObjects(ctx, repo, oids)
	if err != nil {
		return nil, errors.Wrap(err, "look up objects")
	}
	_, presigned := s.Storage.(storage.Presigner)

	objectHref := func(oid string) string { return endpoint.Href + "/" + oid }
	resp := &BatchResponse{
		Transfer: TransferBasic,
		Objects:  make([]BatchResponseObject, 0, len(req.Objects)),
	}
	for _, in := range req.Objects {
		obj, ok := existing[in.OID]
		out := BatchResponseObject{OID: in.OID, Size: in.Size, Authenticated: endpoint.Authenticated}

		switch req.Operation {
		case OperationDownload:
			switch {
			case !ok:
				out.Error = &ObjectError{Code: http.StatusNotFound, Message: "object does not exist"}
			case obj.Size != in.Size:
				out.Error = &ObjectError{Code: http.StatusUnprocessableEntity, Message: "OID exists with different size"}
			default:
				out.Actions = map[string]Action{
					"download": {Href: objectHref(in.OID), Header: endpoint.Header},
				}
			}
		case OperationUpload:
			switch {
			case s.MaxObjectSize > 0 && in.Size > s.MaxObjectSize:
				out.Error = &ObjectError{
					Code:    http.StatusRequestEntityTooLarge,
					Message: fmt.Sprintf("object size %d exceeds limit %d", in.Size, s.MaxObjectSize),
				}
			case ok && obj.Size == in.Size:
				// No actions tell the client the object needs no upload.
			case ok && obj.Size != in.Size:
				out.Error = &ObjectError{Code: http.StatusConflict, Message: "OID exists with different size"}
			default:
				rawOID, _ := hex.DecodeString(in.OID) // Validated above.
				uploadHeader := make(map[string]string, len(endpoint.Header)+2)
				maps.Copy(uploadHeader, endpoint.Header)
				// The Git LFS client otherwise sends a Content-Type detected from
				// the file content.
				uploadHeader["Content-Type"] = "application/octet-stream"
				// Presigned uploads require the client to send the checksum that
				// the storage verifies the content against.
				uploadHeader["x-amz-checksum-sha256"] = base64.StdEncoding.EncodeToString(rawOID)
				uploadHref, verifyHref := objectHref(in.OID), endpoint.Href+"/verify"
				if presigned {
					uploadID, err := uuid.NewV7()
					if err != nil {
						return nil, errors.Wrap(err, "generate upload ID")
					}
					query := "?" + uploadQueryParam + "=" + uploadID.String()
					uploadHref += query
					verifyHref += query
				}
				out.Actions = map[string]Action{
					"upload": {Href: uploadHref, Header: uploadHeader},
					"verify": {Href: verifyHref, Header: endpoint.Header},
				}
			}
		}
		resp.Objects = append(resp.Objects, out)
	}
	return resp, nil
}

// UploadResult is the outcome of a successful Service.Upload.
type UploadResult struct {
	// Redirect is set when the client must upload the content to another
	// location and then call the verify endpoint.
	Redirect *Redirect
}

// uploadQueryParam is the query parameter of the upload and verify actions that
// carries the upload ID for a storage.Presigner.
const uploadQueryParam = "upload"

// validateUploadID checks that uploadID has the form of the upload IDs issued
// by Service.Batch, so that it is safe to use in storage locations.
func validateUploadID(uploadID string) error {
	id, err := uuid.Parse(uploadID)
	if err != nil || id.Version() != 7 || uploadID != id.String() {
		return &Error{Code: http.StatusBadRequest, Message: "invalid upload ID"}
	}
	return nil
}

// Upload handles an upload of size bytes for oid to repo. For a
// storage.Proxier, it stores the content read from body after verifying its
// size and hash, and ignores uploadID. For a storage.Presigner, it does not
// read body and redirects the client to the upload location of uploadID, the
// upload ID from the action that Service.Batch returned.
func (s *Service) Upload(ctx context.Context, logger *slog.Logger, repo, oid, uploadID string, size int64, body io.Reader) (*UploadResult, error) {
	if !ValidOID(oid) {
		return nil, &Error{Code: http.StatusBadRequest, Message: "invalid oid"}
	}
	if size < 0 {
		return nil, &Error{Code: http.StatusLengthRequired, Message: "Content-Length is required"}
	}
	if s.MaxObjectSize > 0 && size > s.MaxObjectSize {
		return nil, &Error{
			Code:    http.StatusRequestEntityTooLarge,
			Message: fmt.Sprintf("object size %d exceeds limit %d", size, s.MaxObjectSize),
		}
	}

	switch b := s.Storage.(type) {
	case storage.Proxier:
		return s.uploadProxy(ctx, logger, b, repo, oid, size, body)
	case storage.Presigner:
		return uploadPresign(ctx, b, oid, uploadID, size)
	default:
		return nil, errors.Newf("storage backend %T does not implement a known transfer mode", s.Storage)
	}
}

func (s *Service) uploadProxy(ctx context.Context, logger *slog.Logger, backend storage.Proxier, repo, oid string, size int64, body io.Reader) (*UploadResult, error) {
	reader := iox.NewSHA256Reader(iox.NewExactSizeReader(body, size), oid)
	uri, err := backend.Put(ctx, oid, reader)
	if err == nil {
		// Put skips reading when the content is already stored, but the client
		// must still prove it has the content before the object is linked to
		// repo.
		_, err = io.Copy(io.Discard, reader)
	}
	if err != nil {
		var sizeErr *iox.SizeMismatchError
		var hashErr *iox.SHA256MismatchError
		switch {
		case errors.As(err, &sizeErr):
			return nil, &Error{Code: http.StatusUnprocessableEntity, Message: sizeErr.Error()}
		case errors.As(err, &hashErr):
			return nil, &Error{Code: http.StatusUnprocessableEntity, Message: hashErr.Error()}
		default:
			return nil, errors.Wrap(err, "store object")
		}
	}

	storedURI, err := s.Store.LinkObject(ctx, repo, oid, size, uri)
	if err != nil {
		return nil, errors.Wrap(err, "link object")
	}
	if storedURI != uri {
		if err := backend.Delete(ctx, uri); err != nil {
			logger.ErrorContext(ctx, "Failed to delete orphan object", "uri", uri, "error", err)
		}
	}
	return &UploadResult{}, nil
}

func uploadPresign(ctx context.Context, backend storage.Presigner, oid, uploadID string, size int64) (*UploadResult, error) {
	if err := validateUploadID(uploadID); err != nil {
		return nil, err
	}
	url, header, err := backend.PresignPut(ctx, backend.UploadURI(uploadID, oid), oid, size)
	if err != nil {
		return nil, errors.Wrap(err, "presign upload")
	}
	return &UploadResult{Redirect: &Redirect{URL: url, Header: header}}, nil
}

// DownloadResult is the outcome of a successful Service.Download. Exactly one
// of Redirect and Content is set.
type DownloadResult struct {
	// Redirect is set when the client must download the content from another
	// location.
	Redirect *Redirect
	// Content is the object content, which the caller must close.
	Content io.ReadCloser
	// Size is the size of Content in bytes.
	Size int64
}

// Download handles a download of oid from repo.
func (s *Service) Download(ctx context.Context, repo, oid string) (*DownloadResult, error) {
	if !ValidOID(oid) {
		return nil, &Error{Code: http.StatusBadRequest, Message: "invalid oid"}
	}

	objects, err := s.Store.GetRepoObjects(ctx, repo, []string{oid})
	if err != nil {
		return nil, errors.Wrap(err, "look up object")
	}
	object, ok := objects[oid]
	if !ok {
		return nil, &Error{Code: http.StatusNotFound, Message: "object does not exist"}
	}

	backend := s.backendFor(object.URI)
	switch b := backend.(type) {
	case storage.Proxier:
		rc, err := b.Open(ctx, object.URI)
		if err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				return nil, errors.Wrapf(err, "object %q is recorded but missing in storage", object.URI)
			}
			return nil, errors.Wrap(err, "open object")
		}
		return &DownloadResult{Content: rc, Size: object.Size}, nil
	case storage.Presigner:
		url, err := b.PresignGet(ctx, object.URI)
		if err != nil {
			return nil, errors.Wrap(err, "presign download")
		}
		return &DownloadResult{Redirect: &Redirect{URL: url}}, nil
	case nil:
		return nil, errors.Newf("no storage backend for uri %q", object.URI)
	default:
		return nil, errors.Newf("storage backend %T does not implement a known transfer mode", backend)
	}
}

// backendFor returns the backend that stores the object at uri, or nil when
// none does.
func (s *Service) backendFor(uri string) storage.Backend {
	if strings.HasPrefix(uri, s.Storage.Scheme()) {
		return s.Storage
	}
	for _, b := range s.PreviousStorages {
		if strings.HasPrefix(uri, b.Scheme()) {
			return b
		}
	}
	return nil
}

// Verify handles the verification of an upload to repo. A presigned upload
// becomes downloadable only after it is verified, which requires the content
// at the upload location of uploadID, like Service.Upload. Each upload
// location is only used once, so a repository only gains an object by
// uploading its content, even when the object is already stored for another
// repository.
func (s *Service) Verify(ctx context.Context, logger *slog.Logger, repo, uploadID string, req VerifyRequest) error {
	if !ValidOID(req.OID) {
		return &Error{Code: http.StatusBadRequest, Message: "invalid oid"}
	}

	switch b := s.Storage.(type) {
	case storage.Proxier:
		return s.verifyProxy(ctx, repo, req)
	case storage.Presigner:
		return s.verifyPresign(ctx, logger, b, repo, uploadID, req)
	default:
		return errors.Newf("storage backend %T does not implement a known transfer mode", s.Storage)
	}
}

func (s *Service) verifyProxy(ctx context.Context, repo string, req VerifyRequest) error {
	objects, err := s.Store.GetRepoObjects(ctx, repo, []string{req.OID})
	if err != nil {
		return errors.Wrap(err, "look up object")
	}
	object, ok := objects[req.OID]
	if !ok {
		return &Error{Code: http.StatusNotFound, Message: "object does not exist"}
	}
	if object.Size != req.Size {
		return &Error{Code: http.StatusUnprocessableEntity, Message: "size mismatch"}
	}
	return nil
}

func (s *Service) verifyPresign(ctx context.Context, logger *slog.Logger, backend storage.Presigner, repo, uploadID string, req VerifyRequest) error {
	if err := validateUploadID(uploadID); err != nil {
		return err
	}
	uploadURI := backend.UploadURI(uploadID, req.OID)
	uploadSize, err := backend.Head(ctx, uploadURI)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return &Error{Code: http.StatusUnprocessableEntity, Message: "object does not exist in storage"}
		}
		return errors.Wrap(err, "head upload")
	}
	// The presigned upload signs Content-Length, which should make this
	// impossible, but treat any mismatch as a hard error rather than trusting
	// either side.
	if uploadSize != req.Size {
		return &Error{Code: http.StatusUnprocessableEntity, Message: "size mismatch"}
	}

	// Record the object before its content reaches the canonical location, so
	// that the content is cleaned up if verification fails from here on.
	if err = s.Store.InsertPendingObject(ctx, req.OID); err != nil {
		return errors.Wrap(err, "insert pending object")
	}
	uri := backend.URI(req.OID)
	if _, err = backend.Head(ctx, uri); err != nil {
		if !errors.Is(err, storage.ErrNotFound) {
			return errors.Wrap(err, "head object")
		}
		if err = backend.Copy(ctx, uploadURI, uri, req.Size); err != nil {
			return errors.Wrap(err, "copy upload to object")
		}
	}

	storedURI, err := s.Store.VerifyObject(ctx, repo, req.OID, req.Size, uri)
	if err != nil {
		return errors.Wrap(err, "mark object verified")
	}
	if storedURI != uri {
		if err := backend.Delete(ctx, uri); err != nil {
			logger.ErrorContext(ctx, "Failed to delete orphan object", "uri", uri, "error", err)
		}
	}
	if err := backend.Delete(ctx, uploadURI); err != nil {
		logger.ErrorContext(ctx, "Failed to delete upload", "uri", uploadURI, "error", err)
	}
	return nil
}
