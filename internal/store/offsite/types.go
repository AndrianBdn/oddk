package offsite

import (
	"github.com/hypersequent/oddk/internal/rfc3339time"
)

type OffsiteType string

const (
	TypeS3 OffsiteType = "s3"
)

type OffsiteSettings struct {
	ID              int64            `db:"id" json:"id"`
	Active          bool             `db:"active" json:"active"`
	Type            OffsiteType      `db:"type" json:"type"`
	Bucket          string           `db:"bucket" json:"bucket"`
	Endpoint        *string          `db:"endpoint" json:"endpoint,omitempty"`
	Region          *string          `db:"region" json:"region,omitempty"`
	AccessKeyID     string           `db:"access_key_id" json:"accessKeyId"`
	SecretAccessKey string           `db:"secret_access_key" json:"-"`
	BucketPath      *string          `db:"bucket_path" json:"bucketPath,omitempty"`
	EC2IAMRole      bool             `db:"ec2_iam_role" json:"ec2IamRole"`
	CreatedAt       rfc3339time.Time `db:"created_at" json:"createdAt"`
	UpdatedAt       rfc3339time.Time `db:"updated_at" json:"updatedAt"`
}

type OffsiteSettingsJSON struct {
	Type            OffsiteType `json:"type"`
	Bucket          string      `json:"bucket"`
	Endpoint        *string     `json:"endpoint,omitempty"`
	Region          *string     `json:"region,omitempty"`
	AccessKeyID     string      `json:"accessKeyId"`
	SecretAccessKey string      `json:"secretAccessKey"`
	BucketPath      *string     `json:"bucketPath,omitempty"`
	EC2IAMRole      bool        `json:"ec2IamRole"`
}

type OffsiteLog struct {
	ID                int64            `db:"id" json:"id"`
	Event             string           `db:"event" json:"event"`
	OffsiteSettingsID int64            `db:"offsite_settings_id" json:"offsiteSettingsId"`
	Object            string           `db:"object" json:"object"`
	Success           bool             `db:"success" json:"success"`
	ErrorDetails      *string          `db:"error_details" json:"errorDetails,omitempty"`
	CreatedAt         rfc3339time.Time `db:"created_at" json:"createdAt"`
}

const (
	EventUpload = "upload"
)

const PlaceholderSecretKey = "%SAME-AS-BEFORE%" // #nosec G101 - not a hardcoded credential, just a placeholder
