package s3

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/credentials/ec2rolecreds"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/andrianbdn/oddk/internal/store/offsite"
)

type Client struct {
	s3Client   *s3.Client
	bucket     string
	bucketPath string
}

// Target identifies a bucket (plus an optional key prefix) and how to reach
// it — the settings-independent half of what NewClient derives from
// OffsiteSettings. It exists so clients can also be built for buckets ODDK
// has no stored configuration for (a foreign deployment's snapshot, a DR
// host's own bucket).
type Target struct {
	Bucket     string
	BucketPath string // optional key prefix; "" for absolute-key use (s3:// URIs)
	Endpoint   string // "" = AWS S3; non-empty forces path-style addressing
	Region     string // "" = keep what the credential chain resolved, then us-east-1
}

// StaticCredentials is an explicit AWS credential triple. SessionToken must be
// set for STS/SSO-derived credentials (they do not authenticate without it)
// and is empty for long-lived access keys.
type StaticCredentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
}

// NewClientFromConfig is the single assembly point every constructor funnels
// through (and the unit-test seam: tests inject an aws.Config pointed at an
// httptest server). Region resolution: an explicit target region wins, else
// whatever the config's chain resolved, else us-east-1 — the same default the
// offsite path has always used.
func NewClientFromConfig(cfg aws.Config, t Target) *Client {
	if t.Region != "" {
		cfg.Region = t.Region
	} else if cfg.Region == "" {
		// A region is required for signing even with custom endpoints.
		cfg.Region = "us-east-1"
	}

	var s3Client *s3.Client
	if t.Endpoint != "" {
		// Custom endpoint (e.g., S3-compatible storage)
		endpoint := t.Endpoint
		s3Client = s3.NewFromConfig(cfg, func(o *s3.Options) {
			o.BaseEndpoint = &endpoint
			o.UsePathStyle = true // Required for most S3-compatible services
		})
	} else {
		// Standard AWS S3
		s3Client = s3.NewFromConfig(cfg)
	}

	bucketPath := strings.TrimSuffix(t.BucketPath, "/")
	if bucketPath != "" {
		bucketPath += "/"
	}

	return &Client{
		s3Client:   s3Client,
		bucket:     t.Bucket,
		bucketPath: bucketPath,
	}
}

// NewClient builds the S3 client used by every offsite code path backed by
// STORED settings (upload, download, delete, offsite test, cron cleanup).
// Settings must already be decrypted (GetActiveOffsiteSettingsDecrypted); the
// secret is empty in EC2 IAM-role mode.
func NewClient(ctx context.Context, settings *offsite.OffsiteSettings) (*Client, error) {
	if settings.Type != offsite.TypeS3 {
		return nil, fmt.Errorf("unsupported offsite type: %s", settings.Type)
	}

	var awsCfg aws.Config
	var err error

	if settings.EC2IAMRole {
		// Use EC2 IAM role credentials
		provider := ec2rolecreds.New()
		awsCfg, err = config.LoadDefaultConfig(ctx,
			config.WithCredentialsProvider(aws.NewCredentialsCache(provider)),
		)
	} else {
		// Use static credentials
		awsCfg, err = config.LoadDefaultConfig(ctx,
			config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
				settings.AccessKeyID,
				settings.SecretAccessKey,
				"", // session token (not needed for static credentials)
			)),
		)
	}
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}

	t := Target{Bucket: settings.Bucket, Region: "us-east-1"}
	if settings.Region != nil && *settings.Region != "" {
		t.Region = *settings.Region
	}
	if settings.Endpoint != nil {
		t.Endpoint = *settings.Endpoint
	}
	if settings.BucketPath != nil {
		t.BucketPath = *settings.BucketPath
	}
	return NewClientFromConfig(awsCfg, t), nil
}

// NewClientStatic builds a client from an explicit credential triple — the
// daemon side of client→daemon credential transport for restoring from a
// bucket the stored offsite settings do not cover. The credentials live only
// in this client; nothing persists or logs them.
func NewClientStatic(ctx context.Context, t Target, creds StaticCredentials) (*Client, error) {
	awsCfg, err := config.LoadDefaultConfig(ctx,
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			creds.AccessKeyID,
			creds.SecretAccessKey,
			creds.SessionToken,
		)),
	)
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}
	return NewClientFromConfig(awsCfg, t), nil
}

// NewClientEC2Role builds a client on the host's EC2 instance role via IMDS —
// the same provider the offsite ec2IamRole mode uses. The role is probed (one
// Retrieve) before returning, so "this host has no instance role" surfaces
// immediately; pass a short-deadline ctx to bound the probe on non-EC2 hosts.
// The returned client keeps the live cached provider, so credentials refresh
// themselves during long transfers.
func NewClientEC2Role(ctx context.Context, t Target) (*Client, error) {
	awsCfg, err := config.LoadDefaultConfig(ctx,
		config.WithCredentialsProvider(aws.NewCredentialsCache(ec2rolecreds.New())),
	)
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}
	if _, err := awsCfg.Credentials.Retrieve(ctx); err != nil {
		return nil, fmt.Errorf("no EC2 instance role available: %w", err)
	}
	return NewClientFromConfig(awsCfg, t), nil
}

// NewClientAmbient builds a client on the AWS default credential chain — env
// vars, shared config/profile (optionally overridden), ECS, and the EC2
// instance role — with no stored-settings involvement. This is the
// daemon-less path (snapshot apply --s3-uri, snapshot list-remote <uri>): the
// process's own environment is the credential authority.
//
// The chain is probed (one Retrieve) before returning so "no credentials
// found" fails fast with one actionable error instead of at the end of a long
// download. The returned client keeps the LIVE chain, so IMDS/SSO credentials
// refresh themselves during long transfers.
func NewClientAmbient(ctx context.Context, t Target, profile string) (*Client, error) {
	var opts []func(*config.LoadOptions) error
	if profile != "" {
		opts = append(opts, config.WithSharedConfigProfile(profile))
	}
	awsCfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}
	if _, err := awsCfg.Credentials.Retrieve(ctx); err != nil {
		return nil, fmt.Errorf("no AWS credentials found in this environment: %w", err)
	}
	return NewClientFromConfig(awsCfg, t), nil
}

// ResolveAmbientCredentials walks the AWS default chain IN THIS PROCESS (env
// vars, shared config/profile, ECS, EC2 instance role) and snapshots it into
// a static triple plus the chain's resolved region. This is the CLI side of
// credential transport: the daemon runs as a different user — often with a
// clean systemd environment — so the operator's profile or session can only
// reach it as resolved values, never by name.
func ResolveAmbientCredentials(ctx context.Context, profile string) (aws.Credentials, string, error) {
	var opts []func(*config.LoadOptions) error
	if profile != "" {
		opts = append(opts, config.WithSharedConfigProfile(profile))
	}
	cfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return aws.Credentials{}, "", fmt.Errorf("load AWS config: %w", err)
	}
	creds, err := cfg.Credentials.Retrieve(ctx)
	if err != nil {
		return aws.Credentials{}, "", err
	}
	return creds, cfg.Region, nil
}

// ParseS3URI splits an s3://bucket/key object URI. The key must be non-empty;
// use ParseS3BucketURI for a listing target where a bare bucket is valid.
func ParseS3URI(uri string) (bucket, key string, err error) {
	bucket, key, err = ParseS3BucketURI(uri)
	if err != nil {
		return "", "", err
	}
	if key == "" {
		return "", "", fmt.Errorf("no object key in %q (expected s3://bucket/path/to/object)", uri)
	}
	return bucket, key, nil
}

// ParseS3BucketURI splits s3://bucket[/prefix]; the prefix may be empty.
func ParseS3BucketURI(uri string) (bucket, prefix string, err error) {
	const scheme = "s3://"
	if !strings.HasPrefix(uri, scheme) {
		return "", "", fmt.Errorf("not an s3:// URI: %q", uri)
	}
	rest := strings.TrimPrefix(uri, scheme)
	bucket, prefix, _ = strings.Cut(rest, "/")
	if bucket == "" {
		return "", "", fmt.Errorf("no bucket in %q", uri)
	}
	return bucket, prefix, nil
}

// IsRegionMismatch reports whether an S3 error looks like a request signed for
// the wrong region: PermanentRedirect (301) or AuthorizationHeaderMalformed
// (400, whose message usually names the region the bucket actually lives in).
// Callers append the --region hint. Detection is string-based, matching the
// NotFound handling in FileExists.
func IsRegionMismatch(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "PermanentRedirect") || strings.Contains(msg, "AuthorizationHeaderMalformed")
}

func (c *Client) UploadFile(ctx context.Context, key string, content io.Reader) error {
	fullKey := c.bucketPath + key

	_, err := c.s3Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(fullKey),
		Body:   content,
	})
	if err != nil {
		return fmt.Errorf("upload to S3: %w", err)
	}

	return nil
}

// DownloadFile loads an object fully into memory. Only use for small objects
// (e.g. the offsite test file); stream backups with DownloadFileTo instead.
func (c *Client) DownloadFile(ctx context.Context, key string) ([]byte, error) {
	var buf bytes.Buffer
	if _, err := c.DownloadFileTo(ctx, key, &buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// DownloadFileTo streams an object into w without buffering it in memory and
// returns the number of bytes written. When the response carries a
// ContentLength it is verified against the byte count.
func (c *Client) DownloadFileTo(ctx context.Context, key string, w io.Writer) (int64, error) {
	fullKey := c.bucketPath + key

	result, err := c.s3Client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(fullKey),
	})
	if err != nil {
		return 0, fmt.Errorf("download from S3: %w", err)
	}
	defer func() { _ = result.Body.Close() }()

	written, err := io.Copy(w, result.Body)
	if err != nil {
		return written, fmt.Errorf("read S3 object body: %w", err)
	}
	if result.ContentLength != nil && written != *result.ContentLength {
		return written, fmt.Errorf("size mismatch: downloaded %d bytes, expected %d", written, *result.ContentLength)
	}
	return written, nil
}

func (c *Client) DeleteFile(ctx context.Context, key string) error {
	fullKey := c.bucketPath + key

	_, err := c.s3Client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(fullKey),
	})
	if err != nil {
		return fmt.Errorf("delete from S3: %w", err)
	}

	return nil
}

func (c *Client) GetBucketPath() string {
	return c.bucketPath
}

// RelativeKey strips the configured bucket-path prefix from a key taken out
// of a stored s3://bucket/<key> location, yielding the key to pass to this
// client's methods (which re-add the prefix). Keys recorded under a different
// bucket path pass through unchanged.
func (c *Client) RelativeKey(fullKey string) string {
	if c.bucketPath == "" {
		return fullKey
	}
	if trimmed, ok := strings.CutPrefix(fullKey, c.bucketPath); ok {
		return trimmed
	}
	return fullKey
}

// ObjectInfo describes one listed S3 object. Key is the full bucket-root key
// (NOT relative to the client's bucket path), so it renders directly into an
// s3://bucket/key URI. ETag identifies the CONTENT (for a single-PutObject
// upload it is the body's MD5), which is what lets the download cache prove a
// reuse candidate is the same object rather than a same-sized impostor.
type ObjectInfo struct {
	Key          string    `json:"key"`
	Size         int64     `json:"size"`
	LastModified time.Time `json:"lastModified"`
	ETag         string    `json:"etag,omitempty"`
}

// ListObjects pages ListObjectsV2 under bucketPath+prefix, returning up to
// maxKeys objects; truncated reports whether more exist beyond that.
func (c *Client) ListObjects(ctx context.Context, prefix string, maxKeys int) ([]ObjectInfo, bool, error) {
	fullPrefix := c.bucketPath + prefix

	paginator := s3.NewListObjectsV2Paginator(c.s3Client, &s3.ListObjectsV2Input{
		Bucket: aws.String(c.bucket),
		Prefix: aws.String(fullPrefix),
	})

	var objs []ObjectInfo
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, false, fmt.Errorf("list S3 objects: %w", err)
		}
		for _, obj := range page.Contents {
			if len(objs) >= maxKeys {
				return objs, true, nil
			}
			info := ObjectInfo{}
			if obj.Key != nil {
				info.Key = *obj.Key
			}
			if obj.Size != nil {
				info.Size = *obj.Size
			}
			if obj.LastModified != nil {
				info.LastModified = *obj.LastModified
			}
			if obj.ETag != nil {
				info.ETag = *obj.ETag
			}
			objs = append(objs, info)
		}
	}
	return objs, false, nil
}

// StatFile HeadObjects bucketPath+key. A nil ObjectInfo with a nil error means
// the object does not exist — mirroring FileExists's NotFound tolerance — so a
// fetch can fail fast with an actionable "no such object" before any long
// download starts, and show the size (and content ETag) up front when it does.
func (c *Client) StatFile(ctx context.Context, key string) (*ObjectInfo, error) {
	fullKey := c.bucketPath + key

	result, err := c.s3Client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(fullKey),
	})
	if err != nil {
		if strings.Contains(err.Error(), "NotFound") || strings.Contains(err.Error(), "404") {
			return nil, nil
		}
		return nil, fmt.Errorf("check S3 object: %w", err)
	}
	info := &ObjectInfo{Key: fullKey}
	if result.ContentLength != nil {
		info.Size = *result.ContentLength
	}
	if result.LastModified != nil {
		info.LastModified = *result.LastModified
	}
	if result.ETag != nil {
		info.ETag = *result.ETag
	}
	return info, nil
}

func (c *Client) FileExists(ctx context.Context, key string) (bool, error) {
	fullKey := c.bucketPath + key

	_, err := c.s3Client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(fullKey),
	})
	if err != nil {
		// Check if it's a "not found" error
		if strings.Contains(err.Error(), "NotFound") || strings.Contains(err.Error(), "404") {
			return false, nil
		}
		return false, fmt.Errorf("check S3 object existence: %w", err)
	}

	return true, nil
}
