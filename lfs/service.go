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
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

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
	// CrossRepoDedup allows an upload to be satisfied by an object that is
	// already stored for another repository, without the client sending the
	// content again. Without it, a repository only gains an object by uploading
	// its full content, so knowing an OID and its size is not enough to read the
	// object through a repository the client has write access to.
	CrossRepoDedup bool
	// TracerProvider creates the spans of LFS operations and of the calls to
	// storage backends. Nil uses the global provider of
	// go.opentelemetry.io/otel.
	TracerProvider trace.TracerProvider
}

// Endpoint describes how the client reaches the transfer endpoints of a
// repository: objects at Href/{oid} and verifications at Href/verify.
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
func (s *Service) Batch(ctx context.Context, logger *slog.Logger, repo string, endpoint Endpoint, req BatchRequest) (_ *BatchResponse, err error) {
	ctx, span := s.startSpan(ctx, "lfs.Batch",
		attribute.String("lfs.repo", repo),
		attribute.String("lfs.operation", req.Operation),
		attribute.Int("lfs.object_count", len(req.Objects)),
	)
	defer func() { endSpan(span, err) }()

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

	var existing map[string]Object
	switch req.Operation {
	case OperationDownload:
		existing, err = s.Store.GetRepoObjects(ctx, repo, oids)
	case OperationUpload:
		existing, err = s.uploadedObjects(ctx, repo, oids)
	}
	if err != nil {
		return nil, errors.Wrap(err, "look up objects")
	}

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
				// No actions tell the client the object needs no upload. With
				// cross-repo dedup, the object may only be linked to another
				// repository so far.
				if s.CrossRepoDedup {
					if _, err := s.Store.LinkObject(ctx, repo, in.OID, in.Size, obj.URI); err != nil {
						logger.ErrorContext(ctx, "Failed to link repo to existing object", "oid", in.OID, "error", err)
						out.Error = &ObjectError{Code: http.StatusInternalServerError, Message: "failed to link object to repo"}
					}
				}
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
				out.Actions = map[string]Action{
					"upload": {Href: objectHref(in.OID), Header: uploadHeader},
					"verify": {Href: endpoint.Href + "/verify", Header: endpoint.Header},
				}
			}
		}
		resp.Objects = append(resp.Objects, out)
	}
	return resp, nil
}

// uploadedObjects returns the verified objects among oids that an upload to
// repo does not need to send again.
func (s *Service) uploadedObjects(ctx context.Context, repo string, oids []string) (map[string]Object, error) {
	if s.CrossRepoDedup {
		return s.Store.GetObjects(ctx, oids)
	}
	return s.Store.GetRepoObjects(ctx, repo, oids)
}

// UploadResult is the outcome of a successful Service.Upload.
type UploadResult struct {
	// Redirect is set when the client must upload the content to another
	// location and then call the verify endpoint.
	Redirect *Redirect
	// Existed reports that the object was already stored and the content was not
	// read.
	Existed bool
}

// Upload handles an upload of size bytes for oid to repo. For a
// storage.Proxier, it stores the content read from body after verifying its
// size and hash. For a storage.Presigner, it does not read body.
func (s *Service) Upload(ctx context.Context, logger *slog.Logger, repo, oid string, size int64, body io.Reader) (_ *UploadResult, err error) {
	ctx, span := s.startSpan(ctx, "lfs.Upload",
		attribute.String("lfs.repo", repo),
		attribute.String("lfs.oid", oid),
		attribute.Int64("lfs.size", size),
	)
	defer func() { endSpan(span, err) }()

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
		return s.uploadPresign(ctx, b, repo, oid, size)
	default:
		return nil, errors.Newf("storage backend %T does not implement a known transfer mode", s.Storage)
	}
}

func (s *Service) uploadProxy(ctx context.Context, logger *slog.Logger, backend storage.Proxier, repo, oid string, size int64, body io.Reader) (*UploadResult, error) {
	reader := iox.NewSHA256Reader(iox.NewExactSizeReader(body, size), oid)
	putCtx, span := s.startStorageSpan(ctx, "Put", backend)
	uri, err := backend.Put(putCtx, oid, reader)
	endSpan(span, err)
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
		s.deleteOrphan(ctx, logger, backend, uri)
	}
	return &UploadResult{}, nil
}

func (s *Service) uploadPresign(ctx context.Context, backend storage.Presigner, repo, oid string, size int64) (*UploadResult, error) {
	existing, err := s.uploadedObjects(ctx, repo, []string{oid})
	if err != nil {
		return nil, errors.Wrap(err, "look up object")
	}
	if _, ok := existing[oid]; ok {
		return &UploadResult{Existed: true}, nil
	}

	if err = s.Store.InsertPendingObject(ctx, oid); err != nil {
		return nil, errors.Wrap(err, "insert pending object")
	}
	presignCtx, span := s.startStorageSpan(ctx, "PresignPut", backend)
	url, header, err := backend.PresignPut(presignCtx, oid, size)
	endSpan(span, err)
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
func (s *Service) Download(ctx context.Context, repo, oid string) (_ *DownloadResult, err error) {
	ctx, span := s.startSpan(ctx, "lfs.Download",
		attribute.String("lfs.repo", repo),
		attribute.String("lfs.oid", oid),
	)
	defer func() { endSpan(span, err) }()

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
		openCtx, span := s.startStorageSpan(ctx, "Open", b)
		rc, err := b.Open(openCtx, object.URI)
		endSpan(span, err)
		if err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				return nil, errors.Wrapf(err, "object %q is recorded but missing in storage", object.URI)
			}
			return nil, errors.Wrap(err, "open object")
		}
		return &DownloadResult{Content: rc, Size: object.Size}, nil
	case storage.Presigner:
		presignCtx, span := s.startStorageSpan(ctx, "PresignGet", b)
		url, err := b.PresignGet(presignCtx, object.URI)
		endSpan(span, err)
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
// becomes downloadable only after it is verified.
func (s *Service) Verify(ctx context.Context, logger *slog.Logger, repo string, req VerifyRequest) (err error) {
	ctx, span := s.startSpan(ctx, "lfs.Verify",
		attribute.String("lfs.repo", repo),
		attribute.String("lfs.oid", req.OID),
		attribute.Int64("lfs.size", req.Size),
	)
	defer func() { endSpan(span, err) }()

	if !ValidOID(req.OID) {
		return &Error{Code: http.StatusBadRequest, Message: "invalid oid"}
	}

	switch b := s.Storage.(type) {
	case storage.Proxier:
		return s.verifyProxy(ctx, repo, req)
	case storage.Presigner:
		return s.verifyPresign(ctx, logger, b, repo, req)
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

func (s *Service) verifyPresign(ctx context.Context, logger *slog.Logger, backend storage.Presigner, repo string, req VerifyRequest) error {
	uri := backend.URI(req.OID)
	headCtx, span := s.startStorageSpan(ctx, "Head", backend)
	headSize, err := backend.Head(headCtx, uri)
	endSpan(span, err)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return &Error{Code: http.StatusUnprocessableEntity, Message: "object does not exist in storage"}
		}
		return errors.Wrap(err, "head object")
	}
	// The presigned upload signs Content-Length, which should make this
	// impossible, but treat any mismatch as a hard error rather than trusting
	// either side.
	if headSize != req.Size {
		return &Error{Code: http.StatusUnprocessableEntity, Message: "size mismatch"}
	}

	storedURI, err := s.Store.VerifyObject(ctx, repo, req.OID, req.Size, uri)
	if err != nil {
		return errors.Wrap(err, "mark object verified")
	}
	if storedURI != uri {
		s.deleteOrphan(ctx, logger, backend, uri)
	}
	return nil
}

// deleteOrphan deletes the content at uri that was stored again while the
// object was already recorded with another URI.
func (s *Service) deleteOrphan(ctx context.Context, logger *slog.Logger, backend storage.Backend, uri string) {
	ctx, span := s.startStorageSpan(ctx, "Delete", backend)
	err := backend.Delete(ctx, uri)
	endSpan(span, err)
	if err != nil {
		logger.ErrorContext(ctx, "Failed to delete orphan object", "uri", uri, "error", err)
	}
}
