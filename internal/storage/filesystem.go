package storage

import (
	"context"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/cockroachdb/errors"
)

var _ Proxier = (*FilesystemBackend)(nil)

type FilesystemBackend struct {
	name    string
	scheme  string
	root    string
	tempDir string
}

func NewFilesystemBackend(name, scheme, root, tempDir string) (*FilesystemBackend, error) {
	if root == "" {
		return nil, errors.New("ROOT is required")
	}
	if tempDir == "" {
		return nil, errors.New("TEMP_DIR is required")
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, errors.Wrap(err, "resolve filesystem storage root")
	}
	absTempDir, err := filepath.Abs(tempDir)
	if err != nil {
		return nil, errors.Wrap(err, "resolve filesystem storage temp dir")
	}
	return &FilesystemBackend{name: name, scheme: scheme, root: absRoot, tempDir: absTempDir}, nil
}

func (b *FilesystemBackend) Name() string { return b.name }

func (*FilesystemBackend) Type() Type { return TypeFilesystem }

func (b *FilesystemBackend) Scheme() string { return b.scheme }

func (b *FilesystemBackend) storagePath(oid string) string {
	return filepath.Join(b.root, oid[0:2], oid[2:4], oid)
}

func (b *FilesystemBackend) Put(ctx context.Context, oid string, r io.Reader) (string, error) {
	if len(oid) < 4 {
		return "", errors.Newf("invalid oid %q", oid)
	}
	final := b.storagePath(oid)
	uri := b.uriFromPath(final)

	if _, err := os.Stat(final); err == nil {
		return uri, nil
	}

	if err := os.MkdirAll(b.tempDir, 0o755); err != nil {
		return "", errors.Wrap(err, "create temp dir")
	}
	tmp, err := os.CreateTemp(b.tempDir, "upload-*")
	if err != nil {
		return "", errors.Wrap(err, "create temp file")
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if _, err := io.Copy(tmp, r); err != nil {
		_ = tmp.Close()
		return "", errors.Wrap(err, "copy to temp file")
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return "", errors.Wrap(err, "sync temp file")
	}
	if err := tmp.Close(); err != nil {
		return "", errors.Wrap(err, "close temp file")
	}

	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		return "", errors.Wrap(err, "create object dir")
	}
	if err := os.Rename(tmpPath, final); err != nil {
		return "", errors.Wrap(err, "rename to final path")
	}
	return uri, nil
}

func (b *FilesystemBackend) Open(ctx context.Context, uri string) (io.ReadCloser, error) {
	path, err := b.pathFromURI(uri)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errors.WithStack(ErrNotFound)
		}
		return nil, errors.Wrap(err, "open object file")
	}
	return f, nil
}

func (b *FilesystemBackend) Delete(ctx context.Context, uri string) error {
	path, err := b.pathFromURI(uri)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return errors.Wrap(err, "delete object file")
	}
	return nil
}

// uriFromPath builds an RFC 8089 file URI from an absolute filesystem path.
// On Windows, an extra leading slash is inserted so "C:\foo" becomes
// "file:///C:/foo" rather than "file://C:/foo" (where url.Parse would read
// "C" as the host and ":/foo" as a port).
func (b *FilesystemBackend) uriFromPath(path string) string {
	slashed := filepath.ToSlash(path)
	if !strings.HasPrefix(slashed, "/") {
		slashed = "/" + slashed
	}
	return b.scheme + slashed
}

func (b *FilesystemBackend) pathFromURI(uri string) (string, error) {
	wantScheme := strings.TrimSuffix(b.scheme, "://")
	u, err := url.Parse(uri)
	if err != nil {
		return "", errors.Wrapf(err, "parse storage uri %q", uri)
	}
	if u.Scheme != wantScheme {
		return "", errors.Newf("unsupported scheme %q in uri %q", u.Scheme, uri)
	}
	// On Windows, a URI like "file:///C:/foo" parses with Path="/C:/foo";
	// drop the leading slash so filepath.Abs sees a valid drive-rooted path.
	rawPath := u.Path
	if filepath.Separator == '\\' && len(rawPath) >= 3 && rawPath[0] == '/' && rawPath[2] == ':' {
		rawPath = rawPath[1:]
	}
	path, err := filepath.Abs(filepath.FromSlash(rawPath))
	if err != nil {
		return "", errors.Wrapf(err, "resolve storage uri path %q", u.Path)
	}
	if !strings.HasPrefix(path, b.root+string(filepath.Separator)) && path != b.root {
		return "", errors.Newf("uri path %q escapes root %q", path, b.root)
	}
	return path, nil
}
