package iox

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"

	"github.com/cockroachdb/errors"
)

// SHA256MismatchError is returned by SHA256Reader when the computed digest
// at EOF does not match the expected one.
type SHA256MismatchError struct {
	Expected string
	Actual   string
}

func (e *SHA256MismatchError) Error() string {
	return fmt.Sprintf("SHA256 checksum mismatch: expected %s, got %s",
		e.Expected, e.Actual)
}

// SHA256Reader wraps an io.Reader and verifies at EOF that the SHA256 of
// all bytes read matches the expected lowercase-hex digest. The digest is
// hashed incrementally as bytes flow through, with no buffering.
type SHA256Reader struct {
	r        io.Reader
	hasher   hash.Hash
	expected string
}

func NewSHA256Reader(r io.Reader, expected string) *SHA256Reader {
	return &SHA256Reader{r: r, hasher: sha256.New(), expected: expected}
}

func (h *SHA256Reader) Read(p []byte) (int, error) {
	n, err := h.r.Read(p)
	if n > 0 {
		_, _ = h.hasher.Write(p[:n])
	}
	// io.ErrUnexpectedEOF is what length-prefixed readers (http.Request.Body,
	// io.ReadFull) return when a stream ends early. The hash is meaningful for
	// whatever bytes flowed through, so verify it the same as on a clean EOF.
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		actual := hex.EncodeToString(h.hasher.Sum(nil))
		if actual != h.expected {
			return n, errors.WithStack(&SHA256MismatchError{
				Expected: h.expected,
				Actual:   actual,
			})
		}
	}
	return n, err
}
