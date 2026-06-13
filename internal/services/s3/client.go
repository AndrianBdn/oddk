package s3

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/credentials/ec2rolecreds"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/hypersequent/oddk/internal/store/offsite"
)

type Client struct {
	s3Client   *s3.Client
	bucket     string
	bucketPath string
}

// NewClient builds the single S3 client used by every offsite code path
// (upload, download, delete, offsite test, cron cleanup). Settings must
// already be decrypted (GetActiveOffsiteSettingsDecrypted); the secret is
// empty in EC2 IAM-role mode.
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

	// Region is required for signing even with custom endpoints; default to
	// us-east-1 when the configuration doesn't specify one.
	if settings.Region != nil && *settings.Region != "" {
		awsCfg.Region = *settings.Region
	} else {
		awsCfg.Region = "us-east-1"
	}

	// Set endpoint if provided
	var s3Client *s3.Client
	if settings.Endpoint != nil && *settings.Endpoint != "" {
		// Custom endpoint (e.g., S3-compatible storage)
		s3Client = s3.NewFromConfig(awsCfg, func(o *s3.Options) {
			o.BaseEndpoint = settings.Endpoint
			o.UsePathStyle = true // Required for most S3-compatible services
		})
	} else {
		// Standard AWS S3
		s3Client = s3.NewFromConfig(awsCfg)
	}

	bucketPath := ""
	if settings.BucketPath != nil {
		bucketPath = strings.TrimSuffix(*settings.BucketPath, "/")
		if bucketPath != "" {
			bucketPath += "/"
		}
	}

	return &Client{
		s3Client:   s3Client,
		bucket:     settings.Bucket,
		bucketPath: bucketPath,
	}, nil
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
