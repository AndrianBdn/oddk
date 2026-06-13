package offsite_test

import (
	"strings"
	"testing"

	"github.com/hypersequent/oddk/internal/store/offsite"
)

func TestValidateBucketPath(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		wantErr bool
		errMsg  string
	}{
		// Valid cases
		{
			name:    "empty string (root)",
			path:    "",
			wantErr: false,
		},
		{
			name:    "forward slash (root)",
			path:    "/",
			wantErr: false,
		},
		{
			name:    "simple folder with trailing slash",
			path:    "backups/",
			wantErr: false,
		},
		{
			name:    "nested folders with trailing slash",
			path:    "oddk/backups/",
			wantErr: false,
		},
		{
			name:    "deep nesting with trailing slash",
			path:    "company/department/oddk/backups/",
			wantErr: false,
		},

		// Invalid cases
		{
			name:    "leading slash (not root)",
			path:    "/backups/",
			wantErr: true,
			errMsg:  "must not start with '/'",
		},
		{
			name:    "no trailing slash",
			path:    "backups",
			wantErr: true,
			errMsg:  "must end with '/'",
		},
		{
			name:    "consecutive slashes",
			path:    "backups//folder/",
			wantErr: true,
			errMsg:  "must not contain consecutive slashes",
		},
		{
			name:    "dot component",
			path:    "./backups/",
			wantErr: true,
			errMsg:  "must not contain '.' or '..'",
		},
		{
			name:    "double dot component",
			path:    "backups/../",
			wantErr: true,
			errMsg:  "must not contain '.' or '..'",
		},
		{
			name:    "empty component between slashes",
			path:    "backups//",
			wantErr: true,
			errMsg:  "must not contain consecutive slashes",
		},
		{
			name:    "leading and no trailing slash",
			path:    "/backups",
			wantErr: true,
			errMsg:  "must not start with '/'",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := offsite.ValidateBucketPath(tt.path)
			if tt.wantErr {
				if err == nil {
					t.Errorf("ValidateBucketPath(%q) = nil, want error containing %q", tt.path, tt.errMsg)
				} else if tt.errMsg != "" && !strings.Contains(err.Error(), tt.errMsg) {
					t.Errorf("ValidateBucketPath(%q) = %v, want error containing %q", tt.path, err, tt.errMsg)
				}
			} else {
				if err != nil {
					t.Errorf("ValidateBucketPath(%q) = %v, want nil", tt.path, err)
				}
			}
		})
	}
}

func TestValidateOffsiteConfig(t *testing.T) {
	validEndpoint := "https://s3.us-east-1.amazonaws.com"
	validRegion := "us-east-1"
	validBucketPath := "oddk/backups/"

	tests := []struct {
		name    string
		config  offsite.OffsiteSettingsJSON
		wantErr bool
		errMsg  string
	}{
		{
			name: "valid config with credentials",
			config: offsite.OffsiteSettingsJSON{
				Type:            offsite.TypeS3,
				Bucket:          "my-backup-bucket",
				Region:          &validRegion,
				AccessKeyID:     "AKIAIOSFODNN7EXAMPLE",
				SecretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
				BucketPath:      &validBucketPath,
			},
			wantErr: false,
		},
		{
			name: "valid config with EC2 IAM role",
			config: offsite.OffsiteSettingsJSON{
				Type:       offsite.TypeS3,
				Bucket:     "my-backup-bucket",
				Region:     &validRegion,
				EC2IAMRole: true,
			},
			wantErr: false,
		},
		{
			name: "valid config with endpoint",
			config: offsite.OffsiteSettingsJSON{
				Type:            offsite.TypeS3,
				Bucket:          "my-backup-bucket",
				Endpoint:        &validEndpoint,
				AccessKeyID:     "AKIAIOSFODNN7EXAMPLE",
				SecretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
			},
			wantErr: false,
		},
		{
			name: "invalid bucket path - no trailing slash",
			config: offsite.OffsiteSettingsJSON{
				Type:            offsite.TypeS3,
				Bucket:          "my-backup-bucket",
				Region:          &validRegion,
				AccessKeyID:     "AKIAIOSFODNN7EXAMPLE",
				SecretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
				BucketPath:      new("backups"),
			},
			wantErr: true,
			errMsg:  "must end with '/'",
		},
		{
			name: "invalid bucket path - leading slash",
			config: offsite.OffsiteSettingsJSON{
				Type:            offsite.TypeS3,
				Bucket:          "my-backup-bucket",
				Region:          &validRegion,
				AccessKeyID:     "AKIAIOSFODNN7EXAMPLE",
				SecretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
				BucketPath:      new("/backups/"),
			},
			wantErr: true,
			errMsg:  "must not start with '/'",
		},
		{
			name: "invalid bucket path - consecutive slashes",
			config: offsite.OffsiteSettingsJSON{
				Type:            offsite.TypeS3,
				Bucket:          "my-backup-bucket",
				Region:          &validRegion,
				AccessKeyID:     "AKIAIOSFODNN7EXAMPLE",
				SecretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
				BucketPath:      new("backups//folder/"),
			},
			wantErr: true,
			errMsg:  "consecutive slashes",
		},
		{
			name: "missing bucket",
			config: offsite.OffsiteSettingsJSON{
				Type:            offsite.TypeS3,
				Region:          &validRegion,
				AccessKeyID:     "AKIAIOSFODNN7EXAMPLE",
				SecretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
			},
			wantErr: true,
			errMsg:  "bucket is required",
		},
		{
			name: "missing credentials without IAM role",
			config: offsite.OffsiteSettingsJSON{
				Type:   offsite.TypeS3,
				Bucket: "my-backup-bucket",
				Region: &validRegion,
			},
			wantErr: true,
			errMsg:  "access_key_id is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := offsite.ValidateOffsiteConfig(&tt.config)
			if tt.wantErr {
				if err == nil {
					t.Errorf("ValidateOffsiteConfig() = nil, want error containing %q", tt.errMsg)
				} else if tt.errMsg != "" && !strings.Contains(err.Error(), tt.errMsg) {
					t.Errorf("ValidateOffsiteConfig() = %v, want error containing %q", err, tt.errMsg)
				}
			} else {
				if err != nil {
					t.Errorf("ValidateOffsiteConfig() = %v, want nil", err)
				}
			}
		})
	}
}
