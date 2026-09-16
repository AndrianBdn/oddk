package s3_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"

	s3service "github.com/andrianbdn/oddk/internal/services/s3"
)

func TestUploadFile_SmallObjects(t *testing.T) {
	for _, body := range []string{"", "small archive"} {
		t.Run(body, func(t *testing.T) {
			var puts atomic.Int32
			client := newStubClient(t, "prefix/", func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPut || r.URL.Path != "/test-bucket/prefix/archive" || r.URL.Query().Has("uploadId") {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				got, err := io.ReadAll(r.Body)
				if err != nil || string(got) != body {
					t.Errorf("body = %q, err = %v", got, err)
				}
				puts.Add(1)
			})
			if err := client.UploadFile(t.Context(), "archive", strings.NewReader(body)); err != nil {
				t.Fatal(err)
			}
			if puts.Load() != 1 {
				t.Fatalf("PUT calls = %d, want 1", puts.Load())
			}
		})
	}
}

// Exercise the production part size against a real S3 protocol implementation:
// three parts, an uneven final part, retries after consuming a request body,
// and replacement of an existing object only after successful completion.
func TestUploadFile_Multipart(t *testing.T) {
	payload := bytes.Repeat([]byte("multipart archive data\n"), 1600000)
	for _, mode := range []string{"success", "retry", "part failure", "complete failure", "abort failure", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			file, err := os.Create(filepath.Join(t.TempDir(), "archive"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = file.Close() }()
			// Upload must start at the caller's current offset, including on retry.
			if _, err := file.Write(append([]byte("skip"), payload...)); err != nil {
				t.Fatal(err)
			}
			if _, err := file.Seek(4, io.SeekStart); err != nil {
				t.Fatal(err)
			}

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var creates, parts, completes, aborts, firstPartAttempts atomic.Int32
			backend := gofakes3.New(s3mem.New(), gofakes3.WithAutoBucket(true)).Server()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/test-bucket/prefix/archive" {
					t.Errorf("wrong key: %s", r.URL.Path)
				}
				q := r.URL.Query()
				switch {
				case r.Method == http.MethodPost && q.Has("uploads"):
					creates.Add(1)
				case r.Method == http.MethodPut && q.Has("uploadId"):
					parts.Add(1)
					if q.Get("partNumber") == "1" {
						attempt := firstPartAttempts.Add(1)
						if mode == "cancel" {
							_, _ = io.Copy(io.Discard, r.Body)
							cancel()
							// Wait for the client to cancel this request before
							// returning; an HTTP error would race context.Canceled.
							<-r.Context().Done()
							return
						}
						if mode == "part failure" || mode == "abort failure" || (mode == "retry" && attempt == 1) {
							_, _ = io.Copy(io.Discard, r.Body)
							code, status := "AccessDenied", http.StatusForbidden
							if mode == "retry" {
								code, status = "InternalError", http.StatusInternalServerError
							}
							w.WriteHeader(status)
							_, _ = io.WriteString(w, "<Error><Code>"+code+"</Code></Error>")
							return
						}
					}
				case r.Method == http.MethodPost && q.Has("uploadId"):
					completes.Add(1)
					if mode == "complete failure" {
						w.WriteHeader(http.StatusForbidden)
						_, _ = io.WriteString(w, "<Error><Code>AccessDenied</Code></Error>")
						return
					}
				case r.Method == http.MethodDelete && q.Has("uploadId"):
					aborts.Add(1)
					if mode == "abort failure" {
						w.WriteHeader(http.StatusForbidden)
						_, _ = io.WriteString(w, "<Error><Code>AccessDenied</Code></Error>")
						return
					}
				}
				backend.ServeHTTP(w, r)
			}))
			defer srv.Close()
			client := s3service.NewClientFromConfig(aws.Config{
				Region:      "us-east-1",
				Credentials: credentials.NewStaticCredentialsProvider("key", "secret", ""),
			}, s3service.Target{Bucket: "test-bucket", BucketPath: "prefix", Endpoint: srv.URL})
			if err := client.UploadFile(t.Context(), "archive", strings.NewReader("previous good copy")); err != nil {
				t.Fatal(err)
			}
			err = client.UploadFile(ctx, "archive", file)
			success := mode == "success" || mode == "retry"
			if success && err != nil {
				t.Fatal(err)
			}
			if !success && err == nil {
				t.Fatal("failed multipart upload returned success")
			}
			if mode == "abort failure" && !strings.Contains(err.Error(), "abort multipart upload") {
				t.Errorf("cleanup error missing: %v", err)
			}
			if mode == "cancel" && !errors.Is(err, context.Canceled) {
				t.Errorf("expected cancellation, got %v", err)
			}
			if creates.Load() != 1 {
				t.Errorf("creates = %d, want 1", creates.Load())
			}
			if success {
				if aborts.Load() != 0 || completes.Load() != 1 {
					t.Errorf("completes=%d aborts=%d", completes.Load(), aborts.Load())
				}
				expectedParts := int32(3)
				if mode == "retry" {
					expectedParts++
				}
				if parts.Load() != expectedParts {
					t.Errorf("part requests = %d, want %d", parts.Load(), expectedParts)
				}
			} else if aborts.Load() != 1 {
				t.Errorf("aborts = %d, want 1 even after cancellation", aborts.Load())
			}
			got, err := client.DownloadFile(t.Context(), "archive")
			if err != nil {
				t.Fatal(err)
			}
			want := payload
			if !success {
				want = []byte("previous good copy")
			}
			if !bytes.Equal(got, want) {
				t.Errorf("remote object changed incorrectly: got %d bytes, want %d", len(got), len(want))
			}
		})
	}
}
