// Package s3cas is archifau's durable second level: an S3 bucket holding
// immutable content-addressed blobs plus small name-index objects that mirror
// the local metadata sidecars. Local disk is the hot cache in front of it —
// uploads write through after the local commit, and local read misses stream
// through while filling the cache.
//
// Layout under the configured prefix:
//
//	cas/sha256/<aa>/<bb>/<digest>                 content (immutable, dedup)
//	pypi-meta/<normalized-name>/<filename>.json   mirrors <data>/pypi-meta/...
//	files-meta/<rel-path>.json                    mirrors <data>/files-meta/...
//
// The meta objects carry the sha256 that points into the CAS; only they are
// ever rewritten or deleted, so bucket versioning on them is the recovery
// history while CAS keys never accrue versions.
//
// This package (plus cmd/archifau) is the only place the AWS SDK is imported,
// keeping it out of the roxxy proxy binary.
package s3cas

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/Delphos-Labs/roxxy/internal/util"
)

// ErrNotFound reports a missing CAS or meta object.
var ErrNotFound = errors.New("s3cas: not found")

// Store is the second-level content-addressed store. Handlers hold a nil
// Store when the feature is disabled and must nil-check before use.
type Store interface {
	// GetCAS opens the immutable CAS object for digest (64-char hex sha256).
	// The caller closes the reader. Returns ErrNotFound on a miss.
	GetCAS(ctx context.Context, digest string) (io.ReadCloser, error)

	// PutCAS writes content for digest unless it already exists (HeadObject
	// gated — CAS keys are immutable so an existing object is already
	// correct). The caller guarantees the bytes hash to digest.
	PutCAS(ctx context.Context, digest string, size int64, r io.Reader) error

	// GetMeta fetches a name-index JSON object by key suffix, e.g.
	// "pypi-meta/<name>/<file>.json". Returns ErrNotFound on a miss.
	GetMeta(ctx context.Context, key string) ([]byte, error)

	// PutMeta writes a name-index object. sha256 is the content digest the
	// meta points at, stored as object metadata: when the current object
	// already carries the same digest the write is skipped, so identical
	// re-uploads never accrue versions on a versioned bucket.
	PutMeta(ctx context.Context, key string, data []byte, sha256 string) error

	// DeleteMeta removes a name-index object (idempotent).
	DeleteMeta(ctx context.Context, key string) error

	// ListMeta returns the immediate child names under a meta "directory"
	// prefix (directories carry a trailing slash). Reserved for the future
	// index-union; unused on the serving path today.
	ListMeta(ctx context.Context, keyPrefix string) ([]string, error)
}

// PyPIMetaKey is the name-index key for a PyPI distribution file.
func PyPIMetaKey(name, filename string) string {
	return "pypi-meta/" + name + "/" + filename + ".json"
}

// FilesMetaKey is the name-index key for a bare-file path.
func FilesMetaKey(rel string) string {
	return "files-meta/" + rel + ".json"
}

// Client is the AWS-backed Store.
type Client struct {
	s3     *s3.Client
	bucket string
	prefix string // normalized: no leading/trailing slash, may be empty
}

// New builds a Client from the environment: ARCHIFAU_S3_BUCKET (unset =>
// returns nil, nil — feature disabled), ARCHIFAU_S3_PREFIX, the standard
// AWS_* credential/region chain, and test-only ARCHIFAU_S3_ENDPOINT /
// ARCHIFAU_S3_FORCE_PATH_STYLE overrides.
func New(ctx context.Context) (*Client, error) {
	bucket := os.Getenv("ARCHIFAU_S3_BUCKET")
	if bucket == "" {
		return nil, nil
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("s3cas: loading AWS config: %w", err)
	}
	cli := s3.NewFromConfig(cfg, func(o *s3.Options) {
		if ep := os.Getenv("ARCHIFAU_S3_ENDPOINT"); ep != "" {
			o.BaseEndpoint = aws.String(ep)
		}
		if os.Getenv("ARCHIFAU_S3_FORCE_PATH_STYLE") == "1" {
			o.UsePathStyle = true
		}
		// Disable the SDK's own retryer so the app-level withRetry is the single
		// source of retry truth. Otherwise the two compound (SDK x3 * app x4 ~=
		// 12 PutObject calls per job), amplifying load on an already-failing S3.
		o.RetryMaxAttempts = 1
	})
	return &Client{
		s3:     cli,
		bucket: bucket,
		prefix: strings.Trim(os.Getenv("ARCHIFAU_S3_PREFIX"), "/"),
	}, nil
}

// Bucket returns the configured bucket name (for startup logging).
func (c *Client) Bucket() string { return c.bucket }

// Prefix returns the configured key prefix (for startup logging).
func (c *Client) Prefix() string { return c.prefix }

// key joins the prefix with a key suffix.
func (c *Client) key(sub string) string {
	if c.prefix == "" {
		return sub
	}
	return c.prefix + "/" + sub
}

// casKey shards a digest into the house-style cas/sha256/<aa>/<bb>/<digest>.
func casKey(digest string) (string, error) {
	if len(digest) != 64 || strings.ToLower(digest) != digest {
		return "", fmt.Errorf("s3cas: malformed digest %q", digest)
	}
	return "cas/sha256/" + digest[:2] + "/" + digest[2:4] + "/" + digest, nil
}

func (c *Client) GetCAS(ctx context.Context, digest string) (io.ReadCloser, error) {
	k, err := casKey(digest)
	if err != nil {
		return nil, err
	}
	out, err := c.s3.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(c.key(k)),
	})
	if err != nil {
		return nil, translateNotFound(err)
	}
	return out.Body, nil
}

func (c *Client) PutCAS(ctx context.Context, digest string, size int64, r io.Reader) error {
	k, err := casKey(digest)
	if err != nil {
		return err
	}
	full := c.key(k)
	if c.exists(ctx, full) {
		return nil // immutable: already-present content is already correct
	}
	_, err = c.s3.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(c.bucket),
		Key:           aws.String(full),
		Body:          r,
		ContentLength: aws.Int64(size),
	})
	return err
}

func (c *Client) GetMeta(ctx context.Context, key string) ([]byte, error) {
	out, err := c.s3.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(c.key(key)),
	})
	if err != nil {
		return nil, translateNotFound(err)
	}
	defer util.DeferClose(out.Body)
	return io.ReadAll(out.Body)
}

func (c *Client) PutMeta(ctx context.Context, key string, data []byte, sha256 string) error {
	full := c.key(key)
	head, err := c.s3.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(full),
	})
	if err == nil && head.Metadata["sha256"] == sha256 {
		return nil // unchanged: don't accrue a redundant version
	}
	_, err = c.s3.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(c.bucket),
		Key:         aws.String(full),
		Body:        strings.NewReader(string(data)),
		ContentType: aws.String("application/json"),
		Metadata:    map[string]string{"sha256": sha256},
	})
	return err
}

func (c *Client) DeleteMeta(ctx context.Context, key string) error {
	_, err := c.s3.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(c.key(key)),
	})
	return err
}

func (c *Client) ListMeta(ctx context.Context, keyPrefix string) ([]string, error) {
	full := c.key(strings.TrimSuffix(keyPrefix, "/")) + "/"
	var names []string
	p := s3.NewListObjectsV2Paginator(c.s3, &s3.ListObjectsV2Input{
		Bucket:    aws.String(c.bucket),
		Prefix:    aws.String(full),
		Delimiter: aws.String("/"),
	})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, o := range page.Contents {
			names = append(names, strings.TrimPrefix(aws.ToString(o.Key), full))
		}
		for _, cp := range page.CommonPrefixes {
			names = append(names, strings.TrimPrefix(aws.ToString(cp.Prefix), full))
		}
	}
	return names, nil
}

func (c *Client) exists(ctx context.Context, fullKey string) bool {
	_, err := c.s3.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(fullKey),
	})
	return err == nil
}

// translateNotFound maps S3's miss shapes onto ErrNotFound.
func translateNotFound(err error) error {
	var noKey *types.NoSuchKey
	if errors.As(err, &noKey) {
		return ErrNotFound
	}
	var notFound *types.NotFound
	if errors.As(err, &notFound) {
		return ErrNotFound
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NoSuchKey", "NotFound":
			return ErrNotFound
		}
	}
	return err
}
