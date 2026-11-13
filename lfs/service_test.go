package lfs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/cockroachdb/errors"
	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"unknwon.dev/git-lfs-server/lfs/storage"
)

// memStore is an in-memory Store.
type memStore struct {
	mu      sync.Mutex
	objects map[string]*memObject      // Keyed by OID.
	links   map[string]map[string]bool // Keyed by repo, then OID.
}

type memObject struct {
	size     int64
	uri      string
	verified bool
}

func newMemStore() *memStore {
	return &memStore{objects: map[string]*memObject{}, links: map[string]map[string]bool{}}
}

func (s *memStore) verifiedObjects(oids []string, keep func(oid string) bool) map[string]Object {
	out := map[string]Object{}
	for _, oid := range oids {
		if o := s.objects[oid]; o != nil && o.verified && keep(oid) {
			out[oid] = Object{OID: oid, Size: o.size, URI: o.uri}
		}
	}
	return out
}

func (s *memStore) GetRepoObjects(_ context.Context, repo string, oids []string) (map[string]Object, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.verifiedObjects(oids, func(oid string) bool { return s.links[repo][oid] }), nil
}

func (s *memStore) GetObjects(_ context.Context, oids []string) (map[string]Object, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.verifiedObjects(oids, func(string) bool { return true }), nil
}

func (s *memStore) link(repo, oid string) {
	if s.links[repo] == nil {
		s.links[repo] = map[string]bool{}
	}
	s.links[repo][oid] = true
}

func (s *memStore) LinkObject(_ context.Context, repo, oid string, size int64, uri string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.objects[oid]
	if o == nil {
		o = &memObject{}
		s.objects[oid] = o
	}
	if !o.verified {
		o.size, o.uri, o.verified = size, uri, true
	}
	s.link(repo, oid)
	return o.uri, nil
}

func (s *memStore) InsertPendingObject(_ context.Context, oid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.objects[oid] == nil {
		s.objects[oid] = &memObject{}
	}
	return nil
}

func (s *memStore) VerifyObject(_ context.Context, repo, oid string, size int64, uri string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.objects[oid]
	if o == nil {
		return "", errors.New("object not found")
	}
	if !o.verified {
		o.size, o.uri, o.verified = size, uri, true
	}
	s.link(repo, oid)
	return o.uri, nil
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func newFilesystemBackend(t *testing.T, name, scheme string) *storage.FilesystemBackend {
	t.Helper()
	root := t.TempDir()
	b, err := storage.NewFilesystemBackend(name, scheme, root, filepath.Join(root, ".tmp"))
	require.NoError(t, err)
	return b
}

func newS3PresignBackend(t *testing.T) *storage.S3PresignBackend {
	t.Helper()
	mem := s3mem.New()
	require.NoError(t, mem.CreateBucket("lfs-objects"))
	srv := httptest.NewServer(gofakes3.New(mem).Server())
	t.Cleanup(srv.Close)
	b, err := storage.NewS3PresignBackend("s3", "s3://", "lfs-objects", "test", "test", srv.URL)
	require.NoError(t, err)
	return b
}

var discardLogger = slog.New(slog.DiscardHandler)

func requireLFSError(t *testing.T, err error, wantCode int) {
	t.Helper()
	var lfsErr *Error
	require.True(t, errors.As(err, &lfsErr), "want *Error, got %v", err)
	assert.Equal(t, wantCode, lfsErr.Code, lfsErr.Message)
}

func TestService_Proxy(t *testing.T) {
	ctx := t.Context()
	content := "proxied content"
	oid := sha256Hex(content)
	size := int64(len(content))

	t.Run("upload stores content and links it to the repository only", func(t *testing.T) {
		s := &Service{Store: newMemStore(), Storage: newFilesystemBackend(t, "fs", "file://")}
		result, err := s.Upload(ctx, discardLogger, "repo1", oid, size, strings.NewReader(content))
		require.NoError(t, err)
		assert.Equal(t, &UploadResult{}, result)

		require.NoError(t, s.Verify(ctx, discardLogger, "repo1", VerifyRequest{OID: oid, Size: size}))
		requireLFSError(t, s.Verify(ctx, discardLogger, "repo1", VerifyRequest{OID: oid, Size: size + 1}), http.StatusUnprocessableEntity)
		requireLFSError(t, s.Verify(ctx, discardLogger, "repo2", VerifyRequest{OID: oid, Size: size}), http.StatusNotFound)

		download, err := s.Download(ctx, "repo1", oid)
		require.NoError(t, err)
		got, err := io.ReadAll(download.Content)
		require.NoError(t, download.Content.Close())
		require.NoError(t, err)
		assert.Equal(t, content, string(got))
		assert.Equal(t, size, download.Size)

		_, err = s.Download(ctx, "repo2", oid)
		requireLFSError(t, err, http.StatusNotFound)
	})

	t.Run("upload of stored content still verifies the body", func(t *testing.T) {
		s := &Service{Store: newMemStore(), Storage: newFilesystemBackend(t, "fs", "file://")}
		_, err := s.Upload(ctx, discardLogger, "repo1", oid, size, strings.NewReader(content))
		require.NoError(t, err)

		forged := strings.Repeat("x", len(content))
		_, err = s.Upload(ctx, discardLogger, "repo2", oid, size, strings.NewReader(forged))
		requireLFSError(t, err, http.StatusUnprocessableEntity)
		_, err = s.Download(ctx, "repo2", oid)
		requireLFSError(t, err, http.StatusNotFound)

		_, err = s.Upload(ctx, discardLogger, "repo2", oid, size, strings.NewReader(content))
		require.NoError(t, err)
		download, err := s.Download(ctx, "repo2", oid)
		require.NoError(t, err)
		require.NoError(t, download.Content.Close())
	})

	t.Run("upload rejects invalid input", func(t *testing.T) {
		s := &Service{Store: newMemStore(), Storage: newFilesystemBackend(t, "fs", "file://"), MaxObjectSize: size - 1}
		_, err := s.Upload(ctx, discardLogger, "repo1", "not-an-oid", size, strings.NewReader(content))
		requireLFSError(t, err, http.StatusBadRequest)
		_, err = s.Upload(ctx, discardLogger, "repo1", oid, -1, strings.NewReader(content))
		requireLFSError(t, err, http.StatusLengthRequired)
		_, err = s.Upload(ctx, discardLogger, "repo1", oid, size, strings.NewReader(content))
		requireLFSError(t, err, http.StatusRequestEntityTooLarge)

		s.MaxObjectSize = 0
		_, err = s.Upload(ctx, discardLogger, "repo1", oid, size, strings.NewReader(content[1:]))
		requireLFSError(t, err, http.StatusUnprocessableEntity)
	})

	t.Run("download reads objects from previous storages", func(t *testing.T) {
		store := newMemStore()
		previous := newFilesystemBackend(t, "old", "old-file://")
		s := &Service{Store: store, Storage: previous}
		_, err := s.Upload(ctx, discardLogger, "repo1", oid, size, strings.NewReader(content))
		require.NoError(t, err)

		s = &Service{Store: store, Storage: newFilesystemBackend(t, "new", "file://"), PreviousStorages: []storage.Backend{previous}}
		download, err := s.Download(ctx, "repo1", oid)
		require.NoError(t, err)
		got, err := io.ReadAll(download.Content)
		require.NoError(t, download.Content.Close())
		require.NoError(t, err)
		assert.Equal(t, content, string(got))

		s.PreviousStorages = nil
		_, err = s.Download(ctx, "repo1", oid)
		require.Error(t, err)
		assert.False(t, errors.As(err, new(*Error)), "a recorded object without a backend is an internal error")
	})
}

func TestService_Batch(t *testing.T) {
	ctx := t.Context()
	content := "batched content"
	oid := sha256Hex(content)
	size := int64(len(content))
	endpoint := Endpoint{Href: "https://lfs.example.com/repo/info/lfs/objects"}

	newServiceWithObject := func(t *testing.T, crossRepoDedup bool) *Service {
		s := &Service{Store: newMemStore(), Storage: newFilesystemBackend(t, "fs", "file://"), CrossRepoDedup: crossRepoDedup}
		_, err := s.Upload(ctx, discardLogger, "repo1", oid, size, strings.NewReader(content))
		require.NoError(t, err)
		return s
	}
	upload := BatchRequest{Operation: OperationUpload, Objects: []BatchObject{{OID: oid, Size: size}}}
	download := BatchRequest{Operation: OperationDownload, Objects: []BatchObject{{OID: oid, Size: size}}}

	t.Run("upload of an object in the repository needs no transfer", func(t *testing.T) {
		s := newServiceWithObject(t, false)
		resp, err := s.Batch(ctx, discardLogger, "repo1", endpoint, upload)
		require.NoError(t, err)
		assert.Equal(t, []BatchResponseObject{{OID: oid, Size: size}}, resp.Objects)
	})

	t.Run("upload of an object in another repository", func(t *testing.T) {
		t.Run("needs a transfer without cross-repo dedup", func(t *testing.T) {
			s := newServiceWithObject(t, false)
			resp, err := s.Batch(ctx, discardLogger, "repo2", endpoint, upload)
			require.NoError(t, err)
			require.Len(t, resp.Objects, 1)
			assert.Contains(t, resp.Objects[0].Actions, "upload")

			resp, err = s.Batch(ctx, discardLogger, "repo2", endpoint, download)
			require.NoError(t, err)
			require.NotNil(t, resp.Objects[0].Error)
			assert.Equal(t, http.StatusNotFound, resp.Objects[0].Error.Code)
		})

		t.Run("links the object with cross-repo dedup", func(t *testing.T) {
			s := newServiceWithObject(t, true)
			resp, err := s.Batch(ctx, discardLogger, "repo2", endpoint, upload)
			require.NoError(t, err)
			assert.Equal(t, []BatchResponseObject{{OID: oid, Size: size}}, resp.Objects)

			resp, err = s.Batch(ctx, discardLogger, "repo2", endpoint, download)
			require.NoError(t, err)
			assert.Contains(t, resp.Objects[0].Actions, "download")
		})
	})

	t.Run("actions carry the endpoint", func(t *testing.T) {
		s := newServiceWithObject(t, false)
		endpoint := Endpoint{Href: endpoint.Href, Header: map[string]string{"Authorization": "Basic Zm9vOmJhcg=="}, Authenticated: true}
		newContent := "new content"
		newOID := sha256Hex(newContent)

		resp, err := s.Batch(ctx, discardLogger, "repo1", endpoint, BatchRequest{
			Operation: OperationUpload,
			Objects:   []BatchObject{{OID: newOID, Size: int64(len(newContent))}},
		})
		require.NoError(t, err)
		assert.Equal(t, []BatchResponseObject{{
			OID:           newOID,
			Size:          int64(len(newContent)),
			Authenticated: true,
			Actions: map[string]Action{
				"upload": {
					Href: endpoint.Href + "/" + newOID,
					Header: map[string]string{
						"Authorization":         "Basic Zm9vOmJhcg==",
						"Content-Type":          "application/octet-stream",
						"x-amz-checksum-sha256": "/jJgjJ71ts9+P5RkgCU/928k9OwGePPQ8H+YRMv/lgE=",
					},
				},
				"verify": {Href: endpoint.Href + "/verify", Header: endpoint.Header},
			},
		}}, resp.Objects)

		resp, err = s.Batch(ctx, discardLogger, "repo1", endpoint, download)
		require.NoError(t, err)
		assert.Equal(t, []BatchResponseObject{{
			OID:           oid,
			Size:          size,
			Authenticated: true,
			Actions:       map[string]Action{"download": {Href: endpoint.Href + "/" + oid, Header: endpoint.Header}},
		}}, resp.Objects)
	})

	t.Run("rejects invalid requests", func(t *testing.T) {
		s := newServiceWithObject(t, false)
		for _, req := range []BatchRequest{
			{Operation: "delete"},
			{Operation: OperationDownload, Objects: []BatchObject{{OID: "not-an-oid"}}},
		} {
			_, err := s.Batch(ctx, discardLogger, "repo1", endpoint, req)
			requireLFSError(t, err, http.StatusBadRequest)
		}
		_, err := s.Batch(ctx, discardLogger, "repo1", endpoint, BatchRequest{Operation: OperationUpload, Transfers: []string{"tus"}})
		requireLFSError(t, err, http.StatusUnprocessableEntity)
	})
}

func TestService_Presign(t *testing.T) {
	ctx := t.Context()
	content := "presigned content"
	oid := sha256Hex(content)
	size := int64(len(content))

	// uploadToRedirect sends the content to the presigned URL like a client.
	uploadToRedirect := func(t *testing.T, redirect *Redirect) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, redirect.URL, strings.NewReader(content))
		require.NoError(t, err)
		for k, v := range redirect.Header {
			req.Header.Set(k, v)
		}
		req.ContentLength = size
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		require.Equal(t, http.StatusOK, resp.StatusCode)
	}

	for _, crossRepoDedup := range []bool{false, true} {
		t.Run(fmt.Sprintf("crossRepoDedup=%v", crossRepoDedup), func(t *testing.T) {
			s := &Service{Store: newMemStore(), Storage: newS3PresignBackend(t), CrossRepoDedup: crossRepoDedup}

			result, err := s.Upload(ctx, discardLogger, "repo1", oid, size, nil)
			require.NoError(t, err)
			require.NotNil(t, result.Redirect)
			requireLFSError(t, s.Verify(ctx, discardLogger, "repo1", VerifyRequest{OID: oid, Size: size}), http.StatusUnprocessableEntity)
			_, err = s.Download(ctx, "repo1", oid)
			requireLFSError(t, err, http.StatusNotFound)

			uploadToRedirect(t, result.Redirect)
			requireLFSError(t, s.Verify(ctx, discardLogger, "repo1", VerifyRequest{OID: oid, Size: size + 1}), http.StatusUnprocessableEntity)
			require.NoError(t, s.Verify(ctx, discardLogger, "repo1", VerifyRequest{OID: oid, Size: size}))

			download, err := s.Download(ctx, "repo1", oid)
			require.NoError(t, err)
			require.NotNil(t, download.Redirect)
			assert.Contains(t, download.Redirect.URL, oid)

			result, err = s.Upload(ctx, discardLogger, "repo1", oid, size, nil)
			require.NoError(t, err)
			assert.Equal(t, &UploadResult{Existed: true}, result)

			// Without cross-repo dedup, another repository must upload the content
			// itself, which the storage checks against the OID.
			result, err = s.Upload(ctx, discardLogger, "repo2", oid, size, nil)
			require.NoError(t, err)
			assert.Equal(t, crossRepoDedup, result.Redirect == nil)
		})
	}
}
