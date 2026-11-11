package iox

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/cockroachdb/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// errReader returns the given bytes followed by a non-EOF error mid-stream.
type errReader struct {
	data []byte
	err  error
	pos  int
}

func (e *errReader) Read(p []byte) (int, error) {
	if e.pos >= len(e.data) {
		return 0, e.err
	}
	n := copy(p, e.data[e.pos:])
	e.pos += n
	return n, nil
}

func TestExactSizeReader(t *testing.T) {
	t.Run("exact match returns EOF", func(t *testing.T) {
		r := NewExactSizeReader(strings.NewReader("hello"), 5)
		got, err := io.ReadAll(r)
		require.NoError(t, err)
		assert.Equal(t, "hello", string(got))
	})

	t.Run("short stream errors at EOF", func(t *testing.T) {
		r := NewExactSizeReader(strings.NewReader("hi"), 5)
		_, err := io.ReadAll(r)
		require.Error(t, err)

		var mismatch *SizeMismatchError
		require.True(t, errors.As(err, &mismatch))
		assert.Equal(t, int64(5), mismatch.Expected)
		assert.Equal(t, int64(2), mismatch.Actual)
	})

	t.Run("long stream errors before EOF", func(t *testing.T) {
		r := NewExactSizeReader(strings.NewReader("hello world"), 5)
		_, err := io.ReadAll(r)
		require.Error(t, err)

		var mismatch *SizeMismatchError
		require.True(t, errors.As(err, &mismatch))
		assert.Equal(t, int64(5), mismatch.Expected)
		assert.Greater(t, mismatch.Actual, int64(5))
	})

	t.Run("zero expected zero read succeeds", func(t *testing.T) {
		r := NewExactSizeReader(strings.NewReader(""), 0)
		got, err := io.ReadAll(r)
		require.NoError(t, err)
		assert.Empty(t, got)
	})

	t.Run("ErrUnexpectedEOF surfaces as SizeMismatchError", func(t *testing.T) {
		// http.Request.Body and io.ReadFull return ErrUnexpectedEOF when a
		// length-prefixed stream ends early. Treat the same as EOF.
		r := NewExactSizeReader(&errReader{data: []byte("ab"), err: io.ErrUnexpectedEOF}, 5)
		_, err := io.ReadAll(r)
		require.Error(t, err)

		var mismatch *SizeMismatchError
		require.True(t, errors.As(err, &mismatch))
		assert.Equal(t, int64(5), mismatch.Expected)
		assert.Equal(t, int64(2), mismatch.Actual)
	})

	t.Run("underlying error propagates unchanged", func(t *testing.T) {
		sentinel := errors.New("network blew up")
		r := NewExactSizeReader(&errReader{data: []byte("ab"), err: sentinel}, 100)
		_, err := io.ReadAll(r)
		require.Error(t, err)
		assert.True(t, errors.Is(err, sentinel))

		var mismatch *SizeMismatchError
		assert.False(t, errors.As(err, &mismatch))
	})

	t.Run("read into empty buffer does not error", func(t *testing.T) {
		r := NewExactSizeReader(bytes.NewReader([]byte("hello")), 5)
		n, err := r.Read(nil)
		require.NoError(t, err)
		assert.Equal(t, 0, n)
	})
}
