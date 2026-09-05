package operations

import (
	"fmt"
	"strings"
)

// parseS3Location preserves the literal object key, including URL-like characters.
func parseS3Location(location string) (bucket, key string, err error) {
	rest, ok := strings.CutPrefix(location, "s3://")
	if !ok {
		return "", "", fmt.Errorf("not an s3:// location")
	}
	bucket, key, found := strings.Cut(rest, "/")
	if !found || bucket == "" || key == "" {
		return "", "", fmt.Errorf("S3 location requires a bucket and key")
	}
	return bucket, key, nil
}

// parseRemoteS3Location also checks the active offsite configuration's bucket.
func parseRemoteS3Location(location, configuredBucket string) (string, error) {
	bucket, key, err := parseS3Location(location)
	if err != nil {
		return "", fmt.Errorf("invalid S3 location format: %s: %w", location, err)
	}
	if bucket != configuredBucket {
		return "", fmt.Errorf("backup S3 bucket (%s) doesn't match current configuration (%s)",
			bucket, configuredBucket)
	}
	return key, nil
}
