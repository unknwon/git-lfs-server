package storage

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/cockroachdb/errors"
)

// presignURLTTL bounds how long a client has to *initiate* the request.
// S3/R2 validate the signature at request start; once the transfer begins
// in-window it can run as long as needed. Because we serve presigned URLs
// via a 307 redirect, the client follows immediately, so there is no idle
// gap to budget for. Keep this short so a leaked URL has limited utility.
const presignURLTTL = 60 * time.Second

var _ Presigner = (*S3PresignBackend)(nil)

type S3PresignBackend struct {
	name          string
	scheme        string
	bucket        string
	client        *s3.Client
	presignClient *s3.PresignClient
}

func (b *S3PresignBackend) Name() string { return b.name }

func (*S3PresignBackend) Type() Type { return TypeS3Presign }

// NewS3PresignBackend constructs a Presigner backed by an S3-compatible
// service (e.g. Cloudflare R2, AWS S3). Credentials are supplied directly to
// avoid implicit AWS_* env / ~/.aws/config lookups. The scheme is the URI
// prefix used to form canonical object locations and must end with "://".
func NewS3PresignBackend(name, scheme, bucket, accessKeyID, secretAccessKey, endpoint string) (*S3PresignBackend, error) {
	if bucket == "" {
		return nil, errors.New("BUCKET is required")
	}
	if accessKeyID == "" {
		return nil, errors.New("ACCESS_KEY_ID is required")
	}
	if secretAccessKey == "" {
		return nil, errors.New("SECRET_ACCESS_KEY is required")
	}
	if endpoint == "" {
		return nil, errors.New("ENDPOINT is required")
	}
	if _, err := url.Parse(endpoint); err != nil {
		return nil, errors.Wrapf(err, "parse endpoint %q", endpoint)
	}

	client := s3.NewFromConfig(aws.Config{
		// R2 ignores region but the SDK requires one.
		Region:      "auto",
		Credentials: credentials.NewStaticCredentialsProvider(accessKeyID, secretAccessKey, ""),
	}, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
	})
	return newS3PresignBackendWithClient(client, name, scheme, bucket), nil
}

func newS3PresignBackendWithClient(client *s3.Client, name, scheme, bucket string) *S3PresignBackend {
	return &S3PresignBackend{
		name:          name,
		scheme:        scheme,
		bucket:        bucket,
		client:        client,
		presignClient: s3.NewPresignClient(client),
	}
}

// URI returns the canonical {scheme}{bucket}/{oid} location. Pure function;
// safe to call without I/O.
func (b *S3PresignBackend) URI(oid string) string {
	return b.scheme + b.bucket + "/" + oid
}

// PresignPut returns a presigned PUT URL that pins the body's SHA-256 to oid
// via x-amz-checksum-sha256, so the backend server-side validates the upload
// and rejects mismatches with HTTP 400 BadDigest. Content-Length is also
// included so the size is signed.
//
// Note: x-amz-content-sha256 cannot be used here. AWS sigv4 mandates
// UNSIGNED-PAYLOAD as the canonical-request HashedPayload for query-string
// presigned URLs (S3 sigv4-query-string-auth spec), so signing the body hash
// into x-amz-content-sha256 produces a signature that R2 rejects.
func (b *S3PresignBackend) PresignPut(ctx context.Context, oid string, size int64) (string, map[string]string, error) {
	rawOID, err := hex.DecodeString(oid)
	if err != nil {
		return "", nil, errors.Wrapf(err, "decode oid %q", oid)
	}
	checksumB64 := base64.StdEncoding.EncodeToString(rawOID)

	req, err := b.presignClient.PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket:         aws.String(b.bucket),
		Key:            aws.String(oid),
		ContentLength:  aws.Int64(size),
		ChecksumSHA256: aws.String(checksumB64),
	}, func(o *s3.PresignOptions) {
		o.Expires = presignURLTTL
		// DisableHeaderHoisting keeps x-amz-checksum-sha256 as a signed
		// header instead of hoisting it to a query parameter, which is
		// what makes S3/R2 enforce the checksum on PUT.
		// https://github.com/aws/aws-sdk-go-v2/issues/2610
		o.Presigner = v4.NewSigner(func(so *v4.SignerOptions) {
			so.DisableURIPathEscaping = true
			so.DisableHeaderHoisting = true
		})
	})
	if err != nil {
		return "", nil, errors.Wrap(err, "presign put object")
	}

	headers := map[string]string{
		"x-amz-checksum-sha256": checksumB64,
		"Content-Length":        strconv.FormatInt(size, 10),
	}
	return req.URL, headers, nil
}

// PresignGet returns a presigned GET URL. No headers are required: the URL
// alone is sufficient for retrieval.
func (b *S3PresignBackend) PresignGet(ctx context.Context, uri string) (string, error) {
	key, err := b.keyFromURI(uri)
	if err != nil {
		return "", err
	}
	req, err := b.presignClient.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(key),
	}, func(o *s3.PresignOptions) {
		o.Expires = presignURLTTL
	})
	if err != nil {
		return "", errors.Wrap(err, "presign get object")
	}
	return req.URL, nil
}

// Head returns the object's size, or ErrNotFound if the object does not
// exist. Used at verify time to confirm the client's presigned upload landed.
func (b *S3PresignBackend) Head(ctx context.Context, uri string) (int64, error) {
	key, err := b.keyFromURI(uri)
	if err != nil {
		return 0, err
	}
	out, err := b.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		var notFound *s3types.NotFound
		if errors.As(err, &notFound) {
			return 0, errors.WithStack(ErrNotFound)
		}
		return 0, errors.Wrapf(err, "head object %q", key)
	}
	if out.ContentLength == nil {
		return 0, errors.Newf("missing Content-Length on head response for %q", key)
	}
	return *out.ContentLength, nil
}

// Delete removes the object. Returns nil if already absent.
func (b *S3PresignBackend) Delete(ctx context.Context, uri string) error {
	key, err := b.keyFromURI(uri)
	if err != nil {
		return err
	}
	// S3 DeleteObject is idempotent: deleting a missing key returns success.
	if _, err := b.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(key),
	}); err != nil {
		return errors.Wrapf(err, "delete object %q", key)
	}
	return nil
}

// keyFromURI extracts the object key from a {scheme}{bucket}/{key} URI. The
// bucket is asserted to match the configured backend's bucket so a stale URI
// from a different bucket can't accidentally read or write the wrong bytes.
func (b *S3PresignBackend) keyFromURI(uri string) (string, error) {
	rest, ok := strings.CutPrefix(uri, b.scheme)
	if !ok {
		return "", errors.Newf("uri %q does not have scheme %q", uri, b.scheme)
	}
	bucket, key, ok := strings.Cut(rest, "/")
	if !ok || key == "" {
		return "", errors.Newf("uri %q is missing a key", uri)
	}
	if bucket != b.bucket {
		return "", errors.Newf("uri %q references bucket %q but backend is configured for %q", uri, bucket, b.bucket)
	}
	return key, nil
}
