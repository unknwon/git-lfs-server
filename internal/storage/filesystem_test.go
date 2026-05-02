package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/cockroachdb/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testFilesystemScheme = "file://"

func newTestBackend(t *testing.T) *FilesystemBackend {
	t.Helper()
	root := t.TempDir()
	b, err := NewFilesystemBackend(testFilesystemScheme, root, filepath.Join(root, ".tmp"))
	require.NoError(t, err)
	return b
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func TestNewFilesystemBackend(t *testing.T) {
	t.Run("missing root", func(t *testing.T) {
		_, err := NewFilesystemBackend(testFilesystemScheme, "", t.TempDir())
		require.Error(t, err)
	})

	t.Run("missing temp dir", func(t *testing.T) {
		_, err := NewFilesystemBackend(testFilesystemScheme, t.TempDir(), "")
		require.Error(t, err)
	})
}

func TestFilesystemBackend_Put(t *testing.T) {
	t.Run("round trip", func(t *testing.T) {
		b := newTestBackend(t)
		body := "hello world"
		oid := sha256Hex(body)

		uri, err := b.Put(context.Background(), oid, strings.NewReader(body))
		require.NoError(t, err)
		assert.True(t, strings.HasPrefix(uri, testFilesystemScheme))
		assert.Contains(t, uri, oid)

		rc, err := b.Open(context.Background(), uri)
		require.NoError(t, err)
		defer rc.Close()

		got, err := io.ReadAll(rc)
		require.NoError(t, err)
		assert.Equal(t, body, string(got))
	})

	t.Run("skip if exists keeps file without reading body", func(t *testing.T) {
		b := newTestBackend(t)
		body := "duplicate"
		oid := sha256Hex(body)

		uri1, err := b.Put(context.Background(), oid, strings.NewReader(body))
		require.NoError(t, err)

		final := strings.TrimPrefix(uri1, testFilesystemScheme)
		mtimeBefore, err := os.Stat(final)
		require.NoError(t, err)

		counted := &countingReader{src: strings.NewReader(body)}
		uri2, err := b.Put(context.Background(), oid, counted)
		require.NoError(t, err)
		assert.Equal(t, uri1, uri2)
		assert.Equal(t, 0, counted.n, "body must not be read for existing object")

		mtimeAfter, err := os.Stat(final)
		require.NoError(t, err)
		assert.Equal(t, mtimeBefore.ModTime(), mtimeAfter.ModTime(), "final file must not be rewritten")
	})

	t.Run("concurrent same oid", func(t *testing.T) {
		b := newTestBackend(t)
		body := "concurrent"
		oid := sha256Hex(body)

		var wg sync.WaitGroup
		errs := make([]error, 4)
		uris := make([]string, 4)
		for i := range errs {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				uris[i], errs[i] = b.Put(context.Background(), oid, strings.NewReader(body))
			}(i)
		}
		wg.Wait()

		for _, err := range errs {
			require.NoError(t, err)
		}
		for i := 1; i < len(uris); i++ {
			assert.Equal(t, uris[0], uris[i])
		}

		entries, err := os.ReadDir(filepath.Join(b.tempDir))
		require.NoError(t, err)
		assert.Empty(t, entries, "no temp leftovers")
	})

	t.Run("failed reader leaves no orphan", func(t *testing.T) {
		b := newTestBackend(t)
		oid := sha256Hex("would-be")
		_, err := b.Put(context.Background(), oid, &failingReader{})
		require.Error(t, err)

		_, statErr := os.Stat(b.storagePath(oid))
		assert.True(t, os.IsNotExist(statErr))

		entries, err := os.ReadDir(b.tempDir)
		require.NoError(t, err)
		assert.Empty(t, entries)
	})
}

func TestFilesystemBackend_Open(t *testing.T) {
	t.Run("not found", func(t *testing.T) {
		b := newTestBackend(t)
		uri := testFilesystemScheme + b.storagePath(sha256Hex("missing"))
		_, err := b.Open(context.Background(), uri)
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrNotFound))
	})

	t.Run("rejects escape", func(t *testing.T) {
		b := newTestBackend(t)
		_, err := b.Open(context.Background(), "file:///etc/passwd")
		require.Error(t, err)
		assert.False(t, errors.Is(err, ErrNotFound))
	})

	t.Run("rejects bad scheme", func(t *testing.T) {
		b := newTestBackend(t)
		_, err := b.Open(context.Background(), "s3://bucket/key")
		require.Error(t, err)
	})
}

func TestFilesystemBackend_Delete(t *testing.T) {
	t.Run("removes existing", func(t *testing.T) {
		b := newTestBackend(t)
		body := "deleteme"
		oid := sha256Hex(body)

		uri, err := b.Put(context.Background(), oid, strings.NewReader(body))
		require.NoError(t, err)

		require.NoError(t, b.Delete(context.Background(), uri))
		_, err = os.Stat(strings.TrimPrefix(uri, testFilesystemScheme))
		assert.True(t, os.IsNotExist(err))
	})

	t.Run("idempotent on missing", func(t *testing.T) {
		b := newTestBackend(t)
		uri := testFilesystemScheme + b.storagePath(sha256Hex("never-existed"))
		require.NoError(t, b.Delete(context.Background(), uri))
	})
}

type countingReader struct {
	src io.Reader
	n   int
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.src.Read(p)
	r.n += n
	return n, err
}

type failingReader struct{}

func (failingReader) Read(p []byte) (int, error) {
	return 0, errors.New("simulated failure")
}
