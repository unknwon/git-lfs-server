package storage

import (
	"encoding/base64"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/cockroachdb/errors"
	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testBucket   = "lfs-objects"
	testS3Scheme = "r2://"
)

// newTestS3PresignBackend spins up an in-process gofakes3 server, creates the
// test bucket, and returns an S3PresignBackend pointed at the fake. The
// fake's Server http.Handler accepts S3 API calls from any aws-sdk-go-v2
// client configured with path-style and a custom BaseEndpoint.
func newTestS3PresignBackend(t *testing.T) (*S3PresignBackend, *httptest.Server) {
	t.Helper()

	mem := s3mem.New()
	require.NoError(t, mem.CreateBucket(testBucket))

	srv := httptest.NewServer(gofakes3.New(mem).Server())
	t.Cleanup(srv.Close)

	client := s3.NewFromConfig(aws.Config{
		Region:      "auto",
		Credentials: credentials.NewStaticCredentialsProvider("test", "test", ""),
	}, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(srv.URL)
		o.UsePathStyle = true
	})
	return newS3PresignBackendWithClient(client, "test", testS3Scheme, testBucket), srv
}

func TestS3PresignBackend_URI(t *testing.T) {
	b, _ := newTestS3PresignBackend(t)
	oid := sha256Hex("uri-test")
	assert.Equal(t, "r2://"+testBucket+"/"+oid, b.URI(oid))
}

func TestS3PresignBackend_PresignPut(t *testing.T) {
	b, srv := newTestS3PresignBackend(t)
	body := "hello r2 presign"
	oid := sha256Hex(body)
	size := int64(len(body))

	url, headers, err := b.PresignPut(t.Context(), oid, size)
	require.NoError(t, err)

	rawOID, err := hex.DecodeString(oid)
	require.NoError(t, err)
	expectedChecksum := base64.StdEncoding.EncodeToString(rawOID)
	assert.Equal(t, expectedChecksum, headers["x-amz-checksum-sha256"], "OID must be pinned via x-amz-checksum-sha256")
	assert.Equal(t, "16", headers["Content-Length"])

	t.Run("URL host matches configured endpoint", func(t *testing.T) {
		assertURLHasEndpointHost(t, url, srv.URL)
	})

	t.Run("URL signs x-amz-checksum-sha256", func(t *testing.T) {
		signed := signedHeadersOf(t, url)
		assert.Contains(t, signed, "x-amz-checksum-sha256",
			"x-amz-checksum-sha256 must be in X-Amz-SignedHeaders so R2 enforces it")
	})

	t.Run("client PUT to presigned URL stores the bytes", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodPut, url, strings.NewReader(body))
		require.NoError(t, err)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)

		gotSize, err := b.Head(t.Context(), b.URI(oid))
		require.NoError(t, err)
		assert.Equal(t, size, gotSize)
	})
}

func TestS3PresignBackend_PresignGet(t *testing.T) {
	t.Run("round trip", func(t *testing.T) {
		b, srv := newTestS3PresignBackend(t)

		body := "hello r2 download"
		oid := sha256Hex(body)
		uri := b.URI(oid)

		// Seed the bucket directly via the underlying client so the test doesn't
		// depend on PresignPut working.
		_, err := b.client.PutObject(t.Context(), &s3.PutObjectInput{
			Bucket: aws.String(testBucket),
			Key:    aws.String(oid),
			Body:   strings.NewReader(body),
		})
		require.NoError(t, err)

		presignedURL, err := b.PresignGet(t.Context(), uri)
		require.NoError(t, err)

		assertURLHasEndpointHost(t, presignedURL, srv.URL)

		resp, err := http.Get(presignedURL)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
		got, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		assert.Equal(t, body, string(got))
	})

	t.Run("bad URI", func(t *testing.T) {
		b, _ := newTestS3PresignBackend(t)
		cases := map[string]string{
			"missing scheme":   "no-scheme",
			"wrong scheme":     "file:///foo",
			"missing key":      "r2://" + testBucket + "/",
			"different bucket": "r2://other-bucket/" + sha256Hex("x"),
		}
		for name, uri := range cases {
			t.Run(name, func(t *testing.T) {
				_, err := b.PresignGet(t.Context(), uri)
				require.Error(t, err)
			})
		}
	})
}

func TestS3PresignBackend_Head(t *testing.T) {
	t.Run("missing returns ErrNotFound", func(t *testing.T) {
		b, _ := newTestS3PresignBackend(t)
		uri := b.URI(sha256Hex("missing"))
		_, err := b.Head(t.Context(), uri)
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrNotFound), "expected ErrNotFound, got %T: %v", err, err)
	})

	t.Run("returns size", func(t *testing.T) {
		b, _ := newTestS3PresignBackend(t)
		body := "size me"
		oid := sha256Hex(body)
		_, err := b.client.PutObject(t.Context(), &s3.PutObjectInput{
			Bucket: aws.String(testBucket),
			Key:    aws.String(oid),
			Body:   strings.NewReader(body),
		})
		require.NoError(t, err)

		got, err := b.Head(t.Context(), b.URI(oid))
		require.NoError(t, err)
		assert.Equal(t, int64(len(body)), got)
	})
}

func TestS3PresignBackend_Delete(t *testing.T) {
	t.Run("removes existing", func(t *testing.T) {
		b, _ := newTestS3PresignBackend(t)
		body := "delete me"
		oid := sha256Hex(body)
		uri := b.URI(oid)

		_, err := b.client.PutObject(t.Context(), &s3.PutObjectInput{
			Bucket: aws.String(testBucket),
			Key:    aws.String(oid),
			Body:   strings.NewReader(body),
		})
		require.NoError(t, err)

		require.NoError(t, b.Delete(t.Context(), uri))

		_, err = b.Head(t.Context(), uri)
		assert.True(t, errors.Is(err, ErrNotFound))
	})

	t.Run("idempotent on missing", func(t *testing.T) {
		b, _ := newTestS3PresignBackend(t)
		uri := b.URI(sha256Hex("never-existed"))
		require.NoError(t, b.Delete(t.Context(), uri))
	})
}

func TestNewS3PresignBackend(t *testing.T) {
	t.Run("required fields", func(t *testing.T) {
		cases := []struct {
			name                                                 string
			bucket, accessKey, secretKey, endpoint, missingField string
		}{
			{"missing bucket", "", "k", "s", "https://x", "BUCKET"},
			{"missing access key", "b", "", "s", "https://x", "ACCESS_KEY_ID"},
			{"missing secret key", "b", "k", "", "https://x", "SECRET_ACCESS_KEY"},
			{"missing endpoint", "b", "k", "s", "", "ENDPOINT"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				_, err := NewS3PresignBackend("test", testS3Scheme, tc.bucket, tc.accessKey, tc.secretKey, tc.endpoint)
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.missingField)
			})
		}
	})
}

// signedHeadersOf parses a presigned URL and returns the lowercase set of
// header names listed in X-Amz-SignedHeaders.
func signedHeadersOf(t *testing.T, presignedURL string) map[string]struct{} {
	t.Helper()
	u, err := url.Parse(presignedURL)
	require.NoError(t, err)
	signed := strings.Split(u.Query().Get("X-Amz-SignedHeaders"), ";")
	out := make(map[string]struct{}, len(signed))
	for _, h := range signed {
		out[strings.ToLower(h)] = struct{}{}
	}
	return out
}

func assertURLHasEndpointHost(t *testing.T, presignedURL, endpoint string) {
	t.Helper()
	pu, err := url.Parse(presignedURL)
	require.NoError(t, err)
	eu, err := url.Parse(endpoint)
	require.NoError(t, err)
	assert.Equal(t, eu.Host, pu.Host, "presigned URL host must match the configured endpoint host")
}
