package iox

import (
	"fmt"
	"io"

	"github.com/cockroachdb/errors"
)

// SizeMismatchError is returned by ExactSizeReader when the stream length
// does not match the expected size, either because EOF arrived early or
// because the stream exceeded the expected size mid-read.
type SizeMismatchError struct {
	Expected int64
	Actual   int64
}

func (e *SizeMismatchError) Error() string {
	return fmt.Sprintf("size mismatch: expected %d bytes, got %d", e.Expected, e.Actual)
}

// ExactSizeReader wraps an io.Reader and verifies at EOF that exactly
// expected bytes were read. Reads that would push the running total past
// expected fail immediately with *SizeMismatchError, without waiting for
// EOF.
type ExactSizeReader struct {
	r        io.Reader
	expected int64
	read     int64
}

func NewExactSizeReader(r io.Reader, expected int64) *ExactSizeReader {
	return &ExactSizeReader{r: r, expected: expected}
}

func (s *ExactSizeReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	s.read += int64(n)

	if s.read > s.expected {
		return n, errors.WithStack(&SizeMismatchError{Expected: s.expected, Actual: s.read})
	}
	// io.ErrUnexpectedEOF is what http.Request.Body and io.ReadFull return when
	// a length-prefixed stream ends before the declared length. Treat it the
	// same as io.EOF for size verification; any other error propagates.
	if (errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)) && s.read != s.expected {
		return n, errors.WithStack(&SizeMismatchError{Expected: s.expected, Actual: s.read})
	}
	return n, err
}
