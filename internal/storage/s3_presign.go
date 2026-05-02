package storage

import (
	"context"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go/middleware"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"github.com/cockroachdb/errors"
)

// presignURLTTL is the lifetime of presigned URLs handed to clients. Long
// enough for slow networks pushing multi-GB objects, short enough that a
// leaked URL has limited utility.
const presignURLTTL = 15 * time.Minute

var _ Presigner = (*S3PresignBackend)(nil)

type S3PresignBackend struct {
	scheme        string
	bucket        string
	client        *s3.Client
	presignClient *s3.PresignClient
}

// NewS3PresignBackend constructs a Presigner backed by an S3-compatible
// service (e.g. Cloudflare R2, AWS S3). Credentials are supplied directly to
// avoid implicit AWS_* env / ~/.aws/config lookups. The scheme is the URI
// prefix used to form canonical object locations and must end with "://".
func NewS3PresignBackend(scheme, bucket, accessKeyID, secretAccessKey, endpoint string) (*S3PresignBackend, error) {
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
	return newS3PresignBackendWithClient(client, scheme, bucket), nil
}

func newS3PresignBackendWithClient(client *s3.Client, scheme, bucket string) *S3PresignBackend {
	return &S3PresignBackend{
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

// PresignPut returns a presigned PUT URL with x-amz-content-sha256 = oid
// signed into the canonical request, so the backend server-side validates
// that the uploaded body's actual SHA-256 matches the OID and rejects
// mismatches with HTTP 400 BadDigest. Content-Length is also included so the
// size is signed.
func (b *S3PresignBackend) PresignPut(ctx context.Context, oid string, size int64) (string, map[string]string, error) {
	req, err := b.presignClient.PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(b.bucket),
		Key:           aws.String(oid),
		ContentLength: aws.Int64(size),
	}, func(o *s3.PresignOptions) {
		o.Expires = presignURLTTL
		o.ClientOptions = append(o.ClientOptions, func(co *s3.Options) {
			co.APIOptions = append(co.APIOptions, injectPayloadHashAPIOption(oid))
		})
	})
	if err != nil {
		return "", nil, errors.Wrap(err, "presign put object")
	}

	headers := map[string]string{
		"x-amz-content-sha256": oid,
		"Content-Length":       strconv.FormatInt(size, 10),
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

// injectPayloadHashAPIOption returns an APIOption that inserts a finalize
// middleware which both seeds the precomputed payload hash on the context
// and sets the x-amz-content-sha256 header on the request. The SDK's
// ComputePayloadSHA256 middleware is a no-op when the hash is already set,
// and the v4 signer reads x-amz-content-sha256 from req.Header to decide
// what to include in X-Amz-SignedHeaders. The presigner returns the URL
// covering both, so the backend server-side hashes the body and rejects
// mismatches with HTTP 400 BadDigest.
func injectPayloadHashAPIOption(hash string) func(*middleware.Stack) error {
	return func(stack *middleware.Stack) error {
		return stack.Finalize.Add(
			middleware.FinalizeMiddlewareFunc(
				"InjectPayloadHash",
				func(ctx context.Context, in middleware.FinalizeInput, next middleware.FinalizeHandler) (middleware.FinalizeOutput, middleware.Metadata, error) {
					ctx = v4.SetPayloadHash(ctx, hash)
					if req, ok := in.Request.(*smithyhttp.Request); ok {
						req.Header.Set("X-Amz-Content-Sha256", hash)
					} else {
						return middleware.FinalizeOutput{}, middleware.Metadata{}, errors.Newf("unexpected request type %T", in.Request)
					}
					return next.HandleFinalize(ctx, in)
				},
			),
			middleware.Before,
		)
	}
}
