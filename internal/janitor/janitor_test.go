package janitor

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"unknwon.dev/x/logx"

	"unknwon.dev/git-lfs-server/lfs/storage"
)

func TestJanitor_deleteUploads(t *testing.T) {
	clock := gofakes3.FixedTimeSource(time.Now())
	mem := s3mem.New(s3mem.WithTimeSource(clock))
	require.NoError(t, mem.CreateBucket("lfs-objects"))
	srv := httptest.NewServer(gofakes3.New(mem).Server())
	t.Cleanup(srv.Close)
	backend, err := storage.NewS3PresignBackend("s3", "s3://", "lfs-objects", "test", "test", srv.URL)
	require.NoError(t, err)

	put := func(key string) {
		_, err := mem.PutObject("lfs-objects", key, map[string]string{}, strings.NewReader("x"), 1, nil)
		require.NoError(t, err)
	}
	put("uploads/stale/oid")
	clock.Advance(orphanAge + time.Hour)
	put("uploads/recent/oid")

	filesystem, err := storage.NewFilesystemBackend("fs", "file://", t.TempDir(), t.TempDir())
	require.NoError(t, err)
	j := New(nil, map[string]storage.Backend{"a.example.com": backend, "b.example.com": filesystem})
	j.deleteUploads(t.Context(), logx.NewNoopLogger(), clock.Now().Add(-orphanAge))

	_, err = backend.Head(t.Context(), backend.UploadURI("stale", "oid"))
	assert.True(t, errors.Is(err, storage.ErrNotFound), "a stale upload is deleted")
	_, err = backend.Head(t.Context(), backend.UploadURI("recent", "oid"))
	assert.NoError(t, err, "a recent upload is kept")
}
