package offsite

import (
	"fmt"
	"net/url"
	"strings"
)

// ValidateOffsiteConfig validates the offsite configuration
func ValidateOffsiteConfig(config *OffsiteSettingsJSON) error {
	// Type validation
	if config.Type != TypeS3 {
		return fmt.Errorf("unsupported offsite type: %s", config.Type)
	}

	// Bucket is always required
	if config.Bucket == "" {
		return fmt.Errorf("bucket is required")
	}

	// Validate bucket name format (basic S3 bucket naming rules)
	if err := validateBucketName(config.Bucket); err != nil {
		return fmt.Errorf("invalid bucket name: %w", err)
	}

	// Either endpoint or region must be provided
	if (config.Endpoint == nil || *config.Endpoint == "") && (config.Region == nil || *config.Region == "") {
		return fmt.Errorf("either endpoint or region must be provided")
	}

	// Validate endpoint URL format if provided
	if config.Endpoint != nil && *config.Endpoint != "" {
		if err := validateEndpoint(*config.Endpoint); err != nil {
			return fmt.Errorf("invalid endpoint: %w", err)
		}
	}

	if !config.EC2IAMRole {
		if config.AccessKeyID == "" {
			return fmt.Errorf("access_key_id is required when not using EC2IAMRole")
		}
		if config.SecretAccessKey == "" {
			return fmt.Errorf("secret_access_key is required when not using EC2IAMRole")
		}
	}

	// Validate bucket path if provided
	if config.BucketPath != nil && *config.BucketPath != "" {
		if err := ValidateBucketPath(*config.BucketPath); err != nil {
			return fmt.Errorf("invalid bucket_path: %w", err)
		}
	}

	return nil
}

// validateBucketName validates S3 bucket naming conventions (simplified)
func validateBucketName(bucket string) error {
	if len(bucket) < 3 {
		return fmt.Errorf("bucket name must be at least 3 characters long")
	}
	if len(bucket) > 63 {
		return fmt.Errorf("bucket name must be at most 63 characters long")
	}

	// Basic character validation - simplified S3 rules
	if strings.Contains(bucket, "..") || strings.Contains(bucket, ".-") || strings.Contains(bucket, "-.") {
		return fmt.Errorf("bucket name contains invalid character sequences")
	}

	// Must not start or end with hyphen or dot
	if strings.HasPrefix(bucket, "-") || strings.HasSuffix(bucket, "-") ||
		strings.HasPrefix(bucket, ".") || strings.HasSuffix(bucket, ".") {
		return fmt.Errorf("bucket name cannot start or end with hyphen or dot")
	}

	return nil
}

// validateEndpoint validates the endpoint URL format
func validateEndpoint(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("endpoint must be a valid URL: %w", err)
	}

	// Must have a scheme
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("endpoint must use http or https scheme")
	}

	// Must have a host
	if u.Host == "" {
		return fmt.Errorf("endpoint must have a valid host")
	}

	// Should not contain example domains
	if strings.Contains(u.Host, "example.com") || strings.Contains(u.Host, "example.org") {
		return fmt.Errorf("endpoint cannot use example domains")
	}

	return nil
}

// ValidateBucketPath validates the S3 bucket path format
func ValidateBucketPath(path string) error {
	// Empty string is allowed (means bucket root)
	if path == "" {
		return nil
	}

	// "/" is allowed (means bucket root)
	if path == "/" {
		return nil
	}

	// Should not start with slash (S3 keys don't start with /)
	if strings.HasPrefix(path, "/") {
		return fmt.Errorf("bucket path must not start with '/' (except for root '/')")
	}

	// Should not contain consecutive slashes
	if strings.Contains(path, "//") {
		return fmt.Errorf("bucket path must not contain consecutive slashes")
	}

	// Must end with slash (for non-empty, non-root paths)
	if !strings.HasSuffix(path, "/") {
		return fmt.Errorf("bucket path must end with '/' (e.g., 'backups/' or 'oddk/backups/')")
	}

	// Should not contain . or .. path components
	for part := range strings.SplitSeq(strings.TrimSuffix(path, "/"), "/") {
		if part == "." || part == ".." {
			return fmt.Errorf("bucket path must not contain '.' or '..' components")
		}
		if part == "" {
			return fmt.Errorf("bucket path must not have empty components between slashes")
		}
	}

	return nil
}
