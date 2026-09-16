package operations

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andrianbdn/oddk/internal/crypto"
	"github.com/andrianbdn/oddk/internal/rfc3339time"
	"github.com/andrianbdn/oddk/internal/store/offsite"
	snapshotstore "github.com/andrianbdn/oddk/internal/store/snapshot"
)

func TestUploadSnapshot_AboveFiveGiBReachesMultipart(t *testing.T) {
	st, dir := newTestStore(t)
	key, err := crypto.GetOrCreateKeyFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := crypto.EncryptPassword("secret", key)
	if err != nil {
		t.Fatal(err)
	}
	var creates atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Query().Has("uploads") {
			creates.Add(1)
		} else {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
		}
		// Refuse initiation so this regression test needs neither 5 GiB of disk nor
		// a multi-gigabyte network transfer. The sparse file must reach multipart.
		w.WriteHeader(http.StatusForbidden)
		_, _ = fmt.Fprint(w, "<Error><Code>AccessDenied</Code></Error>")
	}))
	defer server.Close()
	endpoint := server.URL
	if err := st.Offsite.Create(&offsite.OffsiteSettings{Type: offsite.TypeS3, Bucket: "bucket", Endpoint: &endpoint, AccessKeyID: "key", SecretAccessKey: secret}); err != nil {
		t.Fatal(err)
	}
	file, err := os.Create(filepath.Join(dir, "large.tar.zst"))
	if err != nil {
		t.Fatal(err)
	}
	const size = 5*1024*1024*1024 + 1
	if err := file.Truncate(size); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	rec := &snapshotstore.Record{Filename: "large.tar.zst", LocalPath: file.Name(), Size: size, Status: "completed", CreatedAt: rfc3339time.Now()}
	if err := st.Snapshot.RecordSnapshot(rec); err != nil {
		t.Fatal(err)
	}
	_, err = UploadSnapshot(context.Background(), &Dependencies{Store: st, MasterKey: key}, rec.ID)
	if err == nil || !strings.Contains(err.Error(), "AccessDenied") || creates.Load() != 1 {
		t.Fatalf("large snapshot did not reach S3 multipart: creates=%d err=%v", creates.Load(), err)
	}
	got, err := st.Snapshot.Get(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RemotePath != "" || got.LocalPath != file.Name() {
		t.Fatalf("failed upload changed locations: %+v", got)
	}
}

func TestSnapshotRetention_LargeOnlyCopyProtectedUntilUploaded(t *testing.T) {
	st, dir := newTestStore(t)
	if err := st.Snapshot.SetPlan(3, 24, 1, 7, "physical"); err != nil {
		t.Fatal(err)
	}
	if err := st.Offsite.Create(&offsite.OffsiteSettings{Type: offsite.TypeS3, Bucket: "bucket"}); err != nil {
		t.Fatal(err)
	}
	var oldest *snapshotstore.Record
	for i := range 3 {
		path := filepath.Join(dir, fmt.Sprintf("%d.tar.zst", i))
		if err := os.WriteFile(path, []byte("archive"), 0o600); err != nil {
			t.Fatal(err)
		}
		rec := &snapshotstore.Record{Filename: filepath.Base(path), LocalPath: path, Size: 6 * 1024 * 1024 * 1024, Status: "completed", CreatedAt: rfc3339time.Time{Time: time.Now().AddDate(0, 0, -10-i)}}
		if err := st.Snapshot.RecordSnapshot(rec); err != nil {
			t.Fatal(err)
		}
		oldest = rec
	}
	op := NewSnapshotCronTaskOp(&Dependencies{Store: st}, dir)
	if err := op.runLocalCleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldest.LocalPath); err != nil {
		t.Fatalf("retention deleted the large archive's only copy: %v", err)
	}
	if err := st.Snapshot.SetRemoteLocation(oldest.ID, "s3://bucket/oldest"); err != nil {
		t.Fatal(err)
	}
	if err := op.runLocalCleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldest.LocalPath); !os.IsNotExist(err) {
		t.Fatalf("uploaded archive should be pruned, got %v", err)
	}
	got, err := st.Snapshot.Get(oldest.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.LocalPath != "" || got.RemotePath == "" {
		t.Fatalf("retention lost remote location: %+v", got)
	}
}
