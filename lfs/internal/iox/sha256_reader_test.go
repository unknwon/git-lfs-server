package iox

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"strings"
	"testing"

	"github.com/cockroachdb/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func TestSHA256Reader(t *testing.T) {
	t.Run("matching digest returns EOF", func(t *testing.T) {
		body := "hello world"
		r := NewSHA256Reader(strings.NewReader(body), sha256Hex(body))
		got, err := io.ReadAll(r)
		require.NoError(t, err)
		assert.Equal(t, body, string(got))
	})

	t.Run("mismatched digest errors at EOF", func(t *testing.T) {
		body := "hello world"
		bogus := sha256Hex("something else")
		r := NewSHA256Reader(strings.NewReader(body), bogus)
		_, err := io.ReadAll(r)
		require.Error(t, err)

		var mismatch *SHA256MismatchError
		require.True(t, errors.As(err, &mismatch))
		assert.Equal(t, bogus, mismatch.Expected)
		assert.Equal(t, sha256Hex(body), mismatch.Actual)
	})

	t.Run("empty stream against empty digest succeeds", func(t *testing.T) {
		emptyDigest := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
		r := NewSHA256Reader(strings.NewReader(""), emptyDigest)
		got, err := io.ReadAll(r)
		require.NoError(t, err)
		assert.Empty(t, got)
	})

	t.Run("ErrUnexpectedEOF still verifies digest of bytes seen", func(t *testing.T) {
		// When a length-prefixed stream ends early, the hash is still
		// meaningful for the bytes that did flow through.
		expected := sha256Hex("ab")
		r := NewSHA256Reader(&errReader{data: []byte("ab"), err: io.ErrUnexpectedEOF}, expected)
		_, err := io.ReadAll(r)
		// The bytes "ab" hash to the expected digest, so even though the
		// stream ended early, the digest matches and only the unexpected-EOF
		// itself surfaces (no SHA256MismatchError wrap).
		require.Error(t, err)
		assert.True(t, errors.Is(err, io.ErrUnexpectedEOF))

		var mismatch *SHA256MismatchError
		assert.False(t, errors.As(err, &mismatch))
	})

	t.Run("ErrUnexpectedEOF surfaces SHA256MismatchError when digest differs", func(t *testing.T) {
		bogus := sha256Hex("something else")
		r := NewSHA256Reader(&errReader{data: []byte("ab"), err: io.ErrUnexpectedEOF}, bogus)
		_, err := io.ReadAll(r)
		require.Error(t, err)

		var mismatch *SHA256MismatchError
		require.True(t, errors.As(err, &mismatch))
		assert.Equal(t, bogus, mismatch.Expected)
		assert.Equal(t, sha256Hex("ab"), mismatch.Actual)
	})

	t.Run("underlying error propagates unchanged", func(t *testing.T) {
		sentinel := errors.New("read blew up")
		r := NewSHA256Reader(&errReader{data: []byte("ab"), err: sentinel}, sha256Hex("ab"))
		_, err := io.ReadAll(r)
		require.Error(t, err)
		assert.True(t, errors.Is(err, sentinel))

		var mismatch *SHA256MismatchError
		assert.False(t, errors.As(err, &mismatch))
	})

	t.Run("composes inside ExactSizeReader", func(t *testing.T) {
		body := "hello world"
		r := NewSHA256Reader(
			NewExactSizeReader(strings.NewReader(body), int64(len(body))),
			sha256Hex(body),
		)
		got, err := io.ReadAll(r)
		require.NoError(t, err)
		assert.Equal(t, body, string(got))
	})
}
