package storage

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/cockroachdb/errors"
)

// presignURLTTL bounds how long a client has to *initiate* the request.
// S3/R2 validate the signature at request start. Once the transfer begins
// in-window it can run as long as needed. Because we serve presigned URLs
// via a 307 redirect, the client follows immediately, so there is no idle
// gap to budget for. Keep this short so a leaked URL has limited utility.
const presignURLTTL = 60 * time.Second

// uploadsPrefix is the key prefix of upload locations.
const uploadsPrefix = "uploads/"

const (
	// maxCopyObjectSize is the largest object that a single CopyObject request
	// can copy. Larger objects are copied in parts.
	maxCopyObjectSize = 5 << 30
	// copyPartSize is the size of each part of a multipart copy. All parts but
	// the last have the same size, which Cloudflare R2 requires.
	copyPartSize = 512 << 20
	// copyPartConcurrency is the number of parts copied at the same time.
	copyPartConcurrency = 8
)

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

func (b *S3PresignBackend) Scheme() string { return b.scheme }

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

// URI returns the canonical {scheme}{bucket}/{oid} location. Pure function,
// safe to call without I/O.
func (b *S3PresignBackend) URI(oid string) string {
	return b.scheme + b.bucket + "/" + oid
}

// UploadURI returns the {scheme}{bucket}/uploads/{uploadID}/{oid} location.
// Pure function, safe to call without I/O.
func (b *S3PresignBackend) UploadURI(uploadID, oid string) string {
	return b.scheme + b.bucket + "/" + uploadsPrefix + uploadID + "/" + oid
}

// PresignPut returns a presigned PUT URL to uri that pins the body's SHA-256 to
// oid via x-amz-checksum-sha256, so the backend server-side validates the
// upload and rejects mismatches with HTTP 400 BadDigest. Content-Length is also
// included so the size is signed.
//
// Note: x-amz-content-sha256 cannot be used here. AWS sigv4 mandates
// UNSIGNED-PAYLOAD as the canonical-request HashedPayload for query-string
// presigned URLs (S3 sigv4-query-string-auth spec), so signing the body hash
// into x-amz-content-sha256 produces a signature that R2 rejects.
func (b *S3PresignBackend) PresignPut(ctx context.Context, uri, oid string, size int64) (string, map[string]string, error) {
	key, err := b.keyFromURI(uri)
	if err != nil {
		return "", nil, err
	}
	rawOID, err := hex.DecodeString(oid)
	if err != nil {
		return "", nil, errors.Wrapf(err, "decode oid %q", oid)
	}
	checksumB64 := base64.StdEncoding.EncodeToString(rawOID)

	req, err := b.presignClient.PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket:         aws.String(b.bucket),
		Key:            aws.String(key),
		ContentLength:  aws.Int64(size),
		ChecksumSHA256: aws.String(checksumB64),
	}, func(o *s3.PresignOptions) {
		o.Expires = presignURLTTL
		o.Presigner = v4.NewSigner(func(so *v4.SignerOptions) {
			so.DisableURIPathEscaping = true
			// DisableHeaderHoisting keeps x-amz-checksum-sha256 as a signed
			// header instead of hoisting it to a query parameter, which is
			// what makes S3/R2 enforce the checksum on PUT.
			// https://github.com/aws/aws-sdk-go-v2/issues/2610
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

// Copy copies the object with CopyObject, or in parts with UploadPartCopy when
// it is larger than what CopyObject supports.
func (b *S3PresignBackend) Copy(ctx context.Context, srcURI, dstURI string, size int64) error {
	srcKey, err := b.keyFromURI(srcURI)
	if err != nil {
		return err
	}
	dstKey, err := b.keyFromURI(dstURI)
	if err != nil {
		return err
	}
	// Keys are made of hex digits, UUIDs, and slashes, which need no escaping.
	copySource := b.bucket + "/" + srcKey
	if size <= maxCopyObjectSize {
		if _, err := b.client.CopyObject(ctx, &s3.CopyObjectInput{
			Bucket:     aws.String(b.bucket),
			Key:        aws.String(dstKey),
			CopySource: aws.String(copySource),
		}); err != nil {
			return errors.Wrapf(err, "copy object %q to %q", srcKey, dstKey)
		}
		return nil
	}
	if err := b.copyMultipart(ctx, copySource, dstKey, size); err != nil {
		return errors.Wrapf(err, "copy object %q to %q in parts", srcKey, dstKey)
	}
	return nil
}

func (b *S3PresignBackend) copyMultipart(ctx context.Context, copySource, dstKey string, size int64) error {
	created, err := b.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(dstKey),
	})
	if err != nil {
		return errors.Wrap(err, "create multipart upload")
	}
	abort := func(cause error) error {
		// Abort even when ctx is canceled, so the parts copied so far do not
		// linger in the storage.
		if _, err := b.client.AbortMultipartUpload(context.WithoutCancel(ctx), &s3.AbortMultipartUploadInput{
			Bucket:   aws.String(b.bucket),
			Key:      aws.String(dstKey),
			UploadId: created.UploadId,
		}); err != nil {
			return errors.WithSecondaryError(cause, errors.Wrap(err, "abort multipart upload"))
		}
		return cause
	}

	ranges := copyPartRanges(size, copyPartSize)
	parts := make([]s3types.CompletedPart, len(ranges))
	partsCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	slots := make(chan struct{}, copyPartConcurrency)
	var wg sync.WaitGroup
	for i, byteRange := range ranges {
		partNumber := aws.Int32(int32(i + 1))
		wg.Go(func() {
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-partsCtx.Done():
				return
			}
			out, err := b.client.UploadPartCopy(partsCtx, &s3.UploadPartCopyInput{
				Bucket:          aws.String(b.bucket),
				Key:             aws.String(dstKey),
				UploadId:        created.UploadId,
				PartNumber:      partNumber,
				CopySource:      aws.String(copySource),
				CopySourceRange: aws.String(byteRange),
			})
			if err != nil {
				cancel(errors.Wrapf(err, "copy part %d", *partNumber))
				return
			}
			parts[i] = s3types.CompletedPart{ETag: out.CopyPartResult.ETag, PartNumber: partNumber}
		})
	}
	wg.Wait()
	if err := context.Cause(partsCtx); err != nil {
		return abort(err)
	}

	if _, err := b.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:          aws.String(b.bucket),
		Key:             aws.String(dstKey),
		UploadId:        created.UploadId,
		MultipartUpload: &s3types.CompletedMultipartUpload{Parts: parts},
	}); err != nil {
		return abort(errors.Wrap(err, "complete multipart upload"))
	}
	return nil
}

// copyPartRanges splits size bytes into consecutive HTTP byte ranges of
// partSize bytes, except the last which holds the remainder.
func copyPartRanges(size, partSize int64) []string {
	ranges := make([]string, 0, (size+partSize-1)/partSize)
	for start := int64(0); start < size; start += partSize {
		end := min(start+partSize, size) - 1
		ranges = append(ranges, "bytes="+strconv.FormatInt(start, 10)+"-"+strconv.FormatInt(end, 10))
	}
	return ranges
}

// DeleteUploads deletes the objects under the uploads/ prefix that were last
// modified before the cutoff.
func (b *S3PresignBackend) DeleteUploads(ctx context.Context, before time.Time) (int, error) {
	var deleted int
	pages := s3.NewListObjectsV2Paginator(b.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(b.bucket),
		Prefix: aws.String(uploadsPrefix),
	})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return deleted, errors.Wrap(err, "list uploads")
		}
		var stale []s3types.ObjectIdentifier
		for _, o := range page.Contents {
			if o.LastModified != nil && o.LastModified.Before(before) {
				stale = append(stale, s3types.ObjectIdentifier{Key: o.Key})
			}
		}
		if len(stale) == 0 {
			continue
		}
		out, err := b.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(b.bucket),
			Delete: &s3types.Delete{Objects: stale, Quiet: aws.Bool(true)},
		})
		if err != nil {
			return deleted, errors.Wrap(err, "delete uploads")
		}
		deleted += len(stale) - len(out.Errors)
		if len(out.Errors) > 0 {
			e := out.Errors[0]
			return deleted, errors.Newf("delete upload %q: %s", aws.ToString(e.Key), aws.ToString(e.Message))
		}
	}
	return deleted, nil
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
