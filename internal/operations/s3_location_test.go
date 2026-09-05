package operations

import "testing"

func TestParseS3Location(t *testing.T) {
	for _, tc := range []struct {
		location string
		key      string
	}{
		{"s3://bucket/archive.tar.zst", "archive.tar.zst"},
		{"s3://bucket/prefix/a%2Fb +?#.tar.zst", "prefix/a%2Fb +?#.tar.zst"},
		{"s3://bucket//key", "/key"},
	} {
		t.Run(tc.location, func(t *testing.T) {
			bucket, key, err := parseS3Location(tc.location)
			if err != nil || bucket != "bucket" || key != tc.key {
				t.Fatalf("got (%q, %q, %v)", bucket, key, err)
			}
			remoteKey, err := parseRemoteS3Location(tc.location, "bucket")
			if err != nil || remoteKey != tc.key {
				t.Fatalf("configured-bucket parser got (%q, %v)", remoteKey, err)
			}
			if _, err := parseRemoteS3Location(tc.location, "other-bucket"); err == nil {
				t.Fatal("expected bucket mismatch error")
			}
		})
	}
}

func TestParseS3LocationRejectsMalformed(t *testing.T) {
	for _, location := range []string{"", "https://bucket/key", "s3://", "s3://bucket", "s3://bucket/", "s3:///key"} {
		t.Run(location, func(t *testing.T) {
			if _, _, err := parseS3Location(location); err == nil {
				t.Fatal("expected parse error")
			}
			if _, err := parseRemoteS3Location(location, "bucket"); err == nil {
				t.Fatal("configured-bucket parser accepted malformed location")
			}
		})
	}
}
