package s3_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"

	s3service "github.com/andrianbdn/oddk/internal/services/s3"
)

// newStubClient builds a real Client against an httptest S3 stub through
// NewClientFromConfig — the seam that exists precisely so these tests need no
// AWS environment, no gofakes3, and no network.
func newStubClient(t *testing.T, bucketPath string, handler http.HandlerFunc) *s3service.Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	cfg := aws.Config{
		Region:      "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider("test-key", "test-secret", ""),
	}
	return s3service.NewClientFromConfig(cfg, s3service.Target{
		Bucket:     "test-bucket",
		BucketPath: bucketPath,
		Endpoint:   srv.URL,
	})
}

func TestParseS3URI(t *testing.T) {
	tests := []struct {
		uri     string
		bucket  string
		key     string
		wantErr bool
	}{
		{uri: "s3://bucket/path/to/snap.tar.zst", bucket: "bucket", key: "path/to/snap.tar.zst"},
		{uri: "s3://bucket/k", bucket: "bucket", key: "k"},
		{uri: "s3://bucket", wantErr: true},  // no key at all
		{uri: "s3://bucket/", wantErr: true}, // empty key
		{uri: "s3:///key", wantErr: true},    // empty bucket
		{uri: "http://bucket/key", wantErr: true},
		{uri: "bucket/key", wantErr: true},
		{uri: "", wantErr: true},
	}
	for _, tt := range tests {
		bucket, key, err := s3service.ParseS3URI(tt.uri)
		if tt.wantErr {
			if err == nil {
				t.Errorf("ParseS3URI(%q): expected error, got %q/%q", tt.uri, bucket, key)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseS3URI(%q): %v", tt.uri, err)
			continue
		}
		if bucket != tt.bucket || key != tt.key {
			t.Errorf("ParseS3URI(%q) = %q/%q, want %q/%q", tt.uri, bucket, key, tt.bucket, tt.key)
		}
	}
}

func TestParseS3BucketURI(t *testing.T) {
	tests := []struct {
		uri     string
		bucket  string
		prefix  string
		wantErr bool
	}{
		{uri: "s3://bucket", bucket: "bucket", prefix: ""},
		{uri: "s3://bucket/", bucket: "bucket", prefix: ""},
		{uri: "s3://bucket/pre/fix", bucket: "bucket", prefix: "pre/fix"},
		{uri: "s3://", wantErr: true},
		{uri: "gs://bucket", wantErr: true},
	}
	for _, tt := range tests {
		bucket, prefix, err := s3service.ParseS3BucketURI(tt.uri)
		if tt.wantErr {
			if err == nil {
				t.Errorf("ParseS3BucketURI(%q): expected error", tt.uri)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseS3BucketURI(%q): %v", tt.uri, err)
			continue
		}
		if bucket != tt.bucket || prefix != tt.prefix {
			t.Errorf("ParseS3BucketURI(%q) = %q/%q, want %q/%q", tt.uri, bucket, prefix, tt.bucket, tt.prefix)
		}
	}
}

func TestStatFile(t *testing.T) {
	client := newStubClient(t, "", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			t.Errorf("unexpected method %s", r.Method)
		}
		switch r.URL.Path {
		case "/test-bucket/present.tar.zst":
			w.Header().Set("Content-Length", "42")
			w.Header().Set("ETag", `"abc123"`)
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	info, err := client.StatFile(context.Background(), "present.tar.zst")
	if err != nil || info == nil {
		t.Fatalf("StatFile(present) = %+v/%v, want info/nil", info, err)
	}
	if info.Size != 42 || info.ETag != `"abc123"` {
		t.Errorf("StatFile(present) = size %d etag %q, want 42/%q", info.Size, info.ETag, `"abc123"`)
	}

	// A missing object is a nil info with a NIL error — the caller turns it
	// into an actionable refusal, not an opaque SDK message.
	info, err = client.StatFile(context.Background(), "missing.tar.zst")
	if err != nil || info != nil {
		t.Errorf("StatFile(missing) = %+v/%v, want nil/nil", info, err)
	}
}

func TestListObjects(t *testing.T) {
	listBody := `<?xml version="1.0" encoding="UTF-8"?>
<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
  <Name>test-bucket</Name>
  <IsTruncated>false</IsTruncated>
  <Contents>
    <Key>base/*snapshots*/2026-08-01/snap-a.tar.zst</Key>
    <Size>10</Size>
    <LastModified>2026-08-01T00:00:00Z</LastModified>
  </Contents>
  <Contents>
    <Key>base/*snapshots*/2026-08-02/snap-b.tar.zst</Key>
    <Size>20</Size>
    <LastModified>2026-08-02T00:00:00Z</LastModified>
  </Contents>
  <Contents>
    <Key>base/*snapshots*/2026-08-03/snap-c.tar.zst</Key>
    <Size>30</Size>
    <LastModified>2026-08-03T00:00:00Z</LastModified>
  </Contents>
</ListBucketResult>`

	var gotPrefix string
	client := newStubClient(t, "base/", func(w http.ResponseWriter, r *http.Request) {
		gotPrefix = r.URL.Query().Get("prefix")
		w.Header().Set("Content-Type", "application/xml")
		_, _ = fmt.Fprint(w, listBody)
	})

	objs, truncated, err := client.ListObjects(context.Background(), "*snapshots*/", 10)
	if err != nil {
		t.Fatalf("ListObjects: %v", err)
	}
	// The client prepends its bucket path, exactly like every other method.
	if gotPrefix != "base/*snapshots*/" {
		t.Errorf("request prefix = %q, want %q", gotPrefix, "base/*snapshots*/")
	}
	if truncated || len(objs) != 3 {
		t.Fatalf("got %d objects (truncated=%v), want 3/false", len(objs), truncated)
	}
	if objs[0].Key != "base/*snapshots*/2026-08-01/snap-a.tar.zst" || objs[0].Size != 10 {
		t.Errorf("unexpected first object: %+v", objs[0])
	}

	// maxKeys is a hard bound reported as truncation, never a silent cut.
	objs, truncated, err = client.ListObjects(context.Background(), "*snapshots*/", 2)
	if err != nil {
		t.Fatalf("ListObjects (capped): %v", err)
	}
	if !truncated || len(objs) != 2 {
		t.Errorf("capped listing: got %d objects (truncated=%v), want 2/true", len(objs), truncated)
	}
}

func TestIsRegionMismatch(t *testing.T) {
	if s3service.IsRegionMismatch(nil) {
		t.Error("nil error must not be a region mismatch")
	}
	if s3service.IsRegionMismatch(errors.New("NoSuchKey: not found")) {
		t.Error("NoSuchKey must not be a region mismatch")
	}
	if !s3service.IsRegionMismatch(errors.New("api error AuthorizationHeaderMalformed: the region 'us-east-1' is wrong; expecting 'eu-central-1'")) {
		t.Error("AuthorizationHeaderMalformed must be a region mismatch")
	}
	if !s3service.IsRegionMismatch(errors.New("api error PermanentRedirect")) {
		t.Error("PermanentRedirect must be a region mismatch")
	}
}
