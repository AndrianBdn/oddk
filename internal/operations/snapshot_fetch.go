package operations

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/andrianbdn/oddk/internal/operr"
	s3service "github.com/andrianbdn/oddk/internal/services/s3"
	"github.com/andrianbdn/oddk/internal/store/offsite"
)

// SnapshotDownloadsDirName is the managed area under the backup directory for
// snapshot archives fetched from S3 that no catalogue row references — a
// foreign deployment's archive pulled for restore-instance, or apply's own
// download on a DR host. Deliberately NOT dot-prefixed: the startup sweep
// deletes dot-prefixed backup-dir entries as orphaned staging. As a
// subdirectory it is invisible to the top-level unreferenced-archive warnings,
// to ReconcileLocalLocations, and to retention — which is the point: nothing
// in here is catalogued, because a foreign row in snapshot_history would drive
// every checklist coverage verdict and displace real archives from the
// retention newest-2 floor.
const SnapshotDownloadsDirName = "downloads"

// SnapshotDownloadsTTL is how long a fetched archive is kept. Everything in
// the downloads area is re-fetchable from S3 by construction, so pruning loses
// nothing; reuse bumps the file's mtime, so an archive an operator is actively
// restoring from does not age out mid-series.
const SnapshotDownloadsTTL = 7 * 24 * time.Hour

// staleDownloadTmpAge bounds .tmp-* partials in the downloads area. A live
// download keeps its temp file's mtime fresh by writing continuously, so one
// this old was orphaned by a crash. (The age matters because the daily sweep
// runs concurrently with the executor — it must not reap an in-flight fetch.)
const staleDownloadTmpAge = time.Hour

// SnapshotDownloadsDir returns the managed downloads area under backupDir.
func SnapshotDownloadsDir(backupDir string) string {
	return filepath.Join(backupDir, SnapshotDownloadsDirName)
}

// RemoteSnapshotSpec names one S3 object plus how to reach it.
type RemoteSnapshotSpec struct {
	URI      string // s3://bucket/key of a snapshot archive
	Region   string // "" = credential chain's region, then us-east-1
	Endpoint string // "" = AWS S3

	// Credentials is the CLIENT-resolved ambient triple, when the CLI's
	// environment had one. Nil means none were supplied. Never persisted,
	// never logged.
	Credentials *s3service.StaticCredentials

	// Profile selects a shared-config profile for the AMBIENT chain. Only
	// meaningful on daemon-less paths (apply, list-remote), where this process
	// resolves credentials itself.
	Profile string
}

// Credential source kinds for a daemon-side remote fetch, reported back to the
// operator so what authenticated the download is never a mystery.
const (
	credSourceOffsite      = "offsite-settings"
	credSourceRequest      = "request"
	credSourceInstanceRole = "instance-role"
)

// pickSnapshotCredsSource implements the documented rung order for a
// daemon-side s3Uri fetch:
//
//  1. active offsite settings — iff the URI's bucket matches AND the request
//     did not name a different endpoint. The endpoint-equality guard means the
//     stored offsite credentials can never be pointed at a substitute endpoint.
//  2. request-supplied credentials — the CLI's resolved ambient chain (env,
//     profile, SSO, its own instance role), carried in the request body.
//  3. the daemon host's own EC2 instance role (probed by the caller).
//
// The first AVAILABLE source wins; a source that then fails fails loudly
// rather than falling through, because falling through would mask
// misconfiguration ("my credentials were silently ignored").
func pickSnapshotCredsSource(settings *offsite.OffsiteSettings, bucket, reqEndpoint string, haveRequestCreds bool) string {
	if settings != nil && settings.Bucket == bucket {
		settingsEndpoint := ""
		if settings.Endpoint != nil {
			settingsEndpoint = *settings.Endpoint
		}
		if reqEndpoint == "" || reqEndpoint == settingsEndpoint {
			return credSourceOffsite
		}
	}
	if haveRequestCreds {
		return credSourceRequest
	}
	return credSourceInstanceRole
}

// newSnapshotFetchClient builds the S3 client for a daemon-side remote fetch,
// returning the client, the object key to pass it, and which credential
// source served it. The instance-role rung is probed with a short deadline so
// a non-EC2 host refuses in seconds, not after an IMDS hang.
func newSnapshotFetchClient(ctx context.Context, deps *Dependencies, spec *RemoteSnapshotSpec) (*s3service.Client, string, string, error) {
	bucket, key, err := s3service.ParseS3URI(spec.URI)
	if err != nil {
		return nil, "", "", operr.Invalidf("invalid s3 URI: %v", err)
	}

	settings, err := GetActiveOffsiteSettingsDecrypted(deps)
	if err != nil {
		return nil, "", "", fmt.Errorf("get offsite settings: %w", err)
	}

	source := pickSnapshotCredsSource(settings, bucket, spec.Endpoint, spec.Credentials != nil)
	switch source {
	case credSourceOffsite:
		// The stored settings supply the CREDENTIALS, not the key layout: the
		// URI names an absolute bucket key that may live outside this
		// deployment's configured bucketPath (another deployment sharing the
		// bucket under a different prefix). Build the client prefix-free and
		// use the key verbatim — re-scoping it under our own prefix would stat
		// bucketPath+key and either refuse a perfectly real object or, worse,
		// fetch a different one.
		prefixFree := *settings
		emptyPath := ""
		prefixFree.BucketPath = &emptyPath
		client, err := s3service.NewClient(ctx, &prefixFree)
		if err != nil {
			return nil, "", "", fmt.Errorf("create S3 client from offsite settings: %w", err)
		}
		return client, key, source, nil

	case credSourceRequest:
		client, err := s3service.NewClientStatic(ctx, s3service.Target{
			Bucket:   bucket,
			Region:   spec.Region,
			Endpoint: spec.Endpoint,
		}, *spec.Credentials)
		if err != nil {
			return nil, "", "", fmt.Errorf("create S3 client from request credentials: %w", err)
		}
		return client, key, source, nil

	default:
		probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		client, err := s3service.NewClientEC2Role(probeCtx, s3service.Target{
			Bucket:   bucket,
			Region:   spec.Region,
			Endpoint: spec.Endpoint,
		})
		if err != nil {
			offsiteNote := "offsite is not configured"
			if settings != nil {
				offsiteNote = fmt.Sprintf("offsite is configured for bucket %q, not this one", settings.Bucket)
			}
			return nil, "", "", operr.Invalidf(
				"no credentials for s3://%s: %s, the request carried none, and this host has no EC2 instance role. "+
					"Run the CLI where AWS credentials are available (AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY, "+
					"a ~/.aws profile via --aws-profile, or an EC2 instance role) so it can pass them to the daemon",
				bucket, offsiteNote)
		}
		return client, key, credSourceInstanceRole, nil
	}
}

// streamToLocalFileAtomic streams an object into a dot-prefixed sibling temp
// file and renames it into place, so a daemon crash mid-download can never
// leave a partial file at the final name — which would make the next attempt
// refuse with "move it aside first". The temp name carries the ".tmp-" prefix
// the startup sweep already reclaims.
func streamToLocalFileAtomic(ctx context.Context, s3Client *s3service.Client, key, localPath string) (int64, error) {
	tmpPath := filepath.Join(filepath.Dir(localPath), ".tmp-s3-download-"+filepath.Base(localPath))
	written, err := streamToLocalFile(ctx, s3Client, key, tmpPath)
	if err != nil {
		return 0, err
	}
	if err := os.Rename(tmpPath, localPath); err != nil {
		_ = os.Remove(tmpPath)
		return 0, fmt.Errorf("finalize downloaded archive: %w", err)
	}
	return written, nil
}

// FetchResult describes a fetched (or reused) archive in the downloads area.
type FetchResult struct {
	Path   string
	Size   int64
	Reused bool
}

// FetchRemoteSnapshot downloads bucket-root key into destDir (created lazily).
//
//   - a missing object refuses with a list-remote hint before anything streams;
//   - an existing file with the object's exact size is REUSED (mtime bumped so
//     the TTL sweep cannot reap an archive mid-restore-series) — retrying a
//     failed restore or restoring several instances out of one archive must
//     not download it again;
//   - an existing file with a different size is atomically replaced: ODDK owns
//     the downloads area exclusively and everything in it is re-fetchable, so a
//     refusal here would only break retry-after-corruption;
//   - the stream lands in a dot-prefixed temp name and is renamed into place,
//     so a crash can never leave a partial file at the final name.
//
// uri is the operator's original s3:// form, used verbatim in messages (key
// may have had a configured bucket path stripped for the client to re-add).
func FetchRemoteSnapshot(ctx context.Context, client *s3service.Client, uri, key, destDir string, progress io.Writer) (*FetchResult, error) {
	bucket, _, _ := s3service.ParseS3URI(uri)
	name := path.Base(key)
	if !strings.HasSuffix(name, ".tar.zst") || name == ".tar.zst" {
		return nil, operr.Invalidf("%s does not name a .tar.zst archive; find one with 'oddk snapshot list-remote s3://%s'", uri, bucket)
	}

	info, err := client.StatFile(ctx, key)
	if err != nil {
		if s3service.IsRegionMismatch(err) {
			return nil, operr.Invalidf("%v\nThe bucket appears to live in a different region — re-run with --region <region> (or set AWS_REGION)", err)
		}
		return nil, fmt.Errorf("check %s: %w", uri, err)
	}
	if info == nil {
		return nil, operr.NotFoundf("%s does not exist; list available snapshots with 'oddk snapshot list-remote s3://%s'", uri, bucket)
	}
	size := info.Size

	if err := os.MkdirAll(destDir, 0o750); err != nil {
		return nil, fmt.Errorf("create downloads dir: %w", err)
	}

	finalPath := filepath.Join(destDir, name)
	sidecarPath := finalPath + downloadSourceSuffix
	if st, statErr := os.Stat(finalPath); statErr == nil && st.Size() == size &&
		downloadSourceMatches(sidecarPath, uri, info.ETag) {
		now := time.Now()
		_ = os.Chtimes(finalPath, now, now)
		_ = os.Chtimes(sidecarPath, now, now)
		return &FetchResult{Path: finalPath, Size: size, Reused: true}, nil
	}

	emitLine(progress, "Downloading %s (%s)...", uri, humanBytes(size))
	tmpPath := filepath.Join(destDir, ".tmp-"+name)
	tmpFile, err := os.Create(tmpPath) // #nosec G304 - path is constructed from safe components
	if err != nil {
		return nil, fmt.Errorf("create temp download file: %w", err)
	}

	written, err := client.DownloadFileTo(ctx, key, &downloadProgressWriter{w: tmpFile, total: size, progress: progress})
	closeErr := tmpFile.Close()
	if err == nil && closeErr != nil {
		err = fmt.Errorf("finish temp download file: %w", closeErr)
	}
	if err != nil {
		_ = os.Remove(tmpPath)
		return nil, fmt.Errorf("download %s: %w", uri, err)
	}
	// Invalidate any stale sidecar before the archive lands: a crash between
	// the rename and the sidecar write must leave "unknown provenance" (which
	// re-downloads), never a wrong claim.
	_ = os.Remove(sidecarPath)
	if err := os.Rename(tmpPath, finalPath); err != nil {
		_ = os.Remove(tmpPath)
		return nil, fmt.Errorf("finalize downloaded archive: %w", err)
	}
	if err := writeDownloadSource(sidecarPath, uri, info.ETag); err != nil {
		// Best-effort: a missing sidecar only costs a re-download next time.
		emitLine(progress, "  (warning: could not record download provenance: %v)", err)
	}

	return &FetchResult{Path: finalPath, Size: written}, nil
}

// downloadSourceSuffix names the provenance sidecar written next to each
// downloaded archive: the source URI and the object's ETag at download time.
const downloadSourceSuffix = ".src"

// downloadSourceMatches reports whether the sidecar proves a cached archive
// came from THIS object: same URI and, when S3 reported one, the same content
// ETag. Size alone is not identity — two deployments' archives can share a
// basename and a size, and an object can be overwritten in place with
// same-sized different content. A missing or unreadable sidecar means
// "unknown provenance": re-download rather than guess.
func downloadSourceMatches(sidecarPath, uri, etag string) bool {
	data, err := os.ReadFile(sidecarPath) // #nosec G304 - path is constructed from safe components
	if err != nil {
		return false
	}
	recordedURI, recordedETag, _ := strings.Cut(strings.TrimSpace(string(data)), "\n")
	if recordedURI != uri {
		return false
	}
	if etag == "" {
		return true
	}
	return strings.TrimSpace(recordedETag) == etag
}

// writeDownloadSource records a downloaded archive's provenance next to it.
func writeDownloadSource(sidecarPath, uri, etag string) error {
	return os.WriteFile(sidecarPath, []byte(uri+"\n"+etag+"\n"), 0o600)
}

// downloadProgressWriter emits a line at each 10% step, so a multi-gigabyte
// fetch reads as movement rather than a hang. Nil progress writers no-op via
// emitLine.
type downloadProgressWriter struct {
	w        io.Writer
	total    int64
	written  int64
	lastStep int64
	progress io.Writer
}

func (p *downloadProgressWriter) Write(b []byte) (int, error) {
	n, err := p.w.Write(b)
	p.written += int64(n)
	if p.total > 0 {
		if step := p.written * 10 / p.total; step > p.lastStep {
			p.lastStep = step
			emitLine(p.progress, "  ... %d%% (%s of %s)", step*10, humanBytes(p.written), humanBytes(p.total))
		}
	}
	return n, err
}

// humanBytes matches the CLI's humanSize rendering (1024-based, one decimal).
func humanBytes(size int64) string {
	const unit = 1024
	if size < unit {
		return fmt.Sprintf("%d B", size)
	}
	div, exp := int64(unit), 0
	for n := size / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(size)/float64(div), "KMGTPE"[exp])
}

// SweepSnapshotDownloads prunes the managed downloads area under backupDir:
// finished archives older than SnapshotDownloadsTTL (everything here is
// re-fetchable from S3 by construction, and reuse bumps mtime so an archive in
// active use stays), and .tmp-* partials older than an hour (orphaned by a
// crash — a live download keeps its mtime fresh). A missing directory is a
// no-op. The directory itself is left in place; delete/create churn buys
// nothing.
func SweepSnapshotDownloads(backupDir string) (int, error) {
	dir := SnapshotDownloadsDir(backupDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}

	removed := 0
	now := time.Now()
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		age := now.Sub(info.ModTime())
		stale := age > SnapshotDownloadsTTL ||
			(strings.HasPrefix(entry.Name(), ".tmp-") && age > staleDownloadTmpAge)
		if !stale {
			continue
		}
		if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil {
			log.Printf("Warning: prune downloaded snapshot %s: %v", entry.Name(), err)
			continue
		}
		removed++
	}
	return removed, nil
}

// RestoreArchiveSource selects where restore-instance's archive comes from.
// Exactly one field must be set (the HTTP handler enforces it; this layer
// trusts but verifies).
type RestoreArchiveSource struct {
	ArchivePath string              // a path on this host's filesystem
	SnapshotID  int                 // a row in this host's snapshot catalogue
	Remote      *RemoteSnapshotSpec // an s3:// object, possibly foreign
}

// Archive origin kinds, reported to the operator.
const (
	ArchiveOriginFile                = "file"
	ArchiveOriginCatalogue           = "catalogue"
	ArchiveOriginCatalogueDownloaded = "catalogue-downloaded"
	ArchiveOriginS3Download          = "s3-download"
	ArchiveOriginS3Cached            = "s3-cached"
)

// ArchiveOrigin reports where the restore's archive actually came from, so
// the CLI can say honestly what happened (downloaded vs reused vs local) and
// where the file now lives.
type ArchiveOrigin struct {
	Kind            string `json:"kind"`
	Path            string `json:"path"`
	DownloadedBytes int64  `json:"downloadedBytes,omitempty"`

	// CredentialSource says what authenticated an S3 fetch:
	// "offsite-settings", "request", or "instance-role". Empty for local kinds.
	CredentialSource string `json:"credentialSource,omitempty"`
}

// ResolveRestoreInstanceArchive turns a restore source into a local archive
// path. The returned foreign flag is true when the archive did NOT come from
// this host's own catalogue — such a restore must never backdate created_at
// for checklist coverage, because this host's snapshots do not hold that data.
//
//   - ArchivePath passes through untouched (existing --file behavior).
//   - SnapshotID uses the row's surviving local copy, else downloads it via
//     DownloadSnapshot — whose refusals (no offsite config, no remote copy, an
//     unreferenced file squatting on the target path) pass through unchanged,
//     and whose SetLocalLocation makes the download the row's managed local
//     copy, subject to normal retention.
//   - Remote first checks whether the URI IS some catalogue row's remote copy
//     and treats that as the SnapshotID case (one archive should not exist in
//     two places); otherwise it fetches into the managed downloads area and
//     the archive stays uncatalogued.
func ResolveRestoreInstanceArchive(ctx context.Context, deps *Dependencies, src *RestoreArchiveSource, backupDir string, progress io.Writer) (string, bool, *ArchiveOrigin, error) {
	sources := 0
	if src.ArchivePath != "" {
		sources++
	}
	if src.SnapshotID != 0 {
		sources++
	}
	if src.Remote != nil {
		sources++
	}
	if sources != 1 {
		return "", false, nil, operr.Invalidf("exactly one of filePath, snapshotId or s3Uri is required")
	}

	switch {
	case src.ArchivePath != "":
		return src.ArchivePath, false, &ArchiveOrigin{Kind: ArchiveOriginFile, Path: src.ArchivePath}, nil

	case src.SnapshotID != 0:
		return resolveCatalogueArchive(ctx, deps, src.SnapshotID, backupDir, progress)

	default:
		record, err := deps.Store.Snapshot.FindByRemoteLocation(src.Remote.URI)
		if err != nil {
			return "", false, nil, err
		}
		if record != nil {
			return resolveCatalogueArchive(ctx, deps, record.ID, backupDir, progress)
		}

		client, key, source, err := newSnapshotFetchClient(ctx, deps, src.Remote)
		if err != nil {
			return "", false, nil, err
		}
		fetch, err := FetchRemoteSnapshot(ctx, client, src.Remote.URI, key, SnapshotDownloadsDir(backupDir), progress)
		if err != nil {
			return "", false, nil, err
		}
		origin := &ArchiveOrigin{Kind: ArchiveOriginS3Download, Path: fetch.Path, CredentialSource: source}
		if fetch.Reused {
			origin.Kind = ArchiveOriginS3Cached
		} else {
			origin.DownloadedBytes = fetch.Size
			// URI, size and credential-source KIND only — never key ids, never
			// secrets. There is no offsite_logs row for a non-offsite fetch:
			// those rows reference a settings row this download may not have.
			log.Printf("restore-instance: downloaded %s (%d bytes) using %s credentials", src.Remote.URI, fetch.Size, source)
		}
		return fetch.Path, true, origin, nil
	}
}

// resolveCatalogueArchive materializes a catalogue row's archive: the
// surviving local copy if there is one, else a download that becomes the
// row's local copy.
func resolveCatalogueArchive(ctx context.Context, deps *Dependencies, id int, backupDir string, progress io.Writer) (string, bool, *ArchiveOrigin, error) {
	record, err := deps.Store.Snapshot.Get(id)
	if err != nil {
		return "", false, nil, err
	}
	if record == nil {
		return "", false, nil, operr.NotFoundf("snapshot %d not found", id)
	}
	if record.LocalPath != "" {
		if _, statErr := os.Stat(record.LocalPath); statErr == nil {
			return record.LocalPath, false, &ArchiveOrigin{Kind: ArchiveOriginCatalogue, Path: record.LocalPath}, nil
		}
	}

	emitLine(progress, "Snapshot %d has no local copy; downloading it from S3...", id)
	result, err := DownloadSnapshot(ctx, deps, id, backupDir)
	if err != nil {
		return "", false, nil, err
	}
	return result.LocalPath, false, &ArchiveOrigin{
		Kind:             ArchiveOriginCatalogueDownloaded,
		Path:             result.LocalPath,
		DownloadedBytes:  result.Size,
		CredentialSource: credSourceOffsite,
	}, nil
}

// RemoteSnapshotObject is one list-remote entry; URI is pre-formatted for
// copy-paste into restore-instance/apply.
type RemoteSnapshotObject struct {
	URI          string    `json:"uri"`
	Key          string    `json:"key"`
	Size         int64     `json:"size"`
	LastModified time.Time `json:"lastModified"`
}

// MaxRemoteSnapshotListing bounds a bucket listing; truncation is reported,
// never silent.
const MaxRemoteSnapshotListing = 1000

// ListRemoteSnapshots lists objects under the client-relative prefix, newest
// first, rendered as bucket-root URIs.
func ListRemoteSnapshots(ctx context.Context, client *s3service.Client, bucket, prefix string) ([]RemoteSnapshotObject, bool, error) {
	objs, truncated, err := client.ListObjects(ctx, prefix, MaxRemoteSnapshotListing)
	if err != nil {
		if s3service.IsRegionMismatch(err) {
			return nil, false, operr.Invalidf("%v\nThe bucket appears to live in a different region — re-run with --region <region> (or set AWS_REGION)", err)
		}
		return nil, false, err
	}

	out := make([]RemoteSnapshotObject, 0, len(objs))
	for _, o := range objs {
		out = append(out, RemoteSnapshotObject{
			URI:          "s3://" + bucket + "/" + o.Key,
			Key:          o.Key,
			Size:         o.Size,
			LastModified: o.LastModified,
		})
	}
	// Newest first: the archive the operator is looking for is almost always
	// the most recent one.
	for i, j := 0, len(out)-1; i < j; {
		if out[i].LastModified.Before(out[j].LastModified) {
			out[i], out[j] = out[j], out[i]
		}
		i++
		j--
	}
	return out, truncated, nil
}

// FetchSnapshotForApply downloads the archive for a daemon-less apply using
// this process's own ambient AWS credential chain (env vars, shared
// config/profile, ECS, EC2 instance role). Stored offsite settings cannot
// serve here even in principle: they live encrypted inside the SNAPSHOT's
// oddk.db, which is exactly what a DR host does not have yet.
//
// Every failure ends with the no-changes reassurance, matching preflight
// refusals — the download is additive and nothing of ODDK's state has been
// touched.
func FetchSnapshotForApply(ctx context.Context, spec *RemoteSnapshotSpec, backupDir string, progress io.Writer) (*FetchResult, error) {
	bucket, key, err := s3service.ParseS3URI(spec.URI)
	if err != nil {
		return nil, appendNoChanges(fmt.Errorf("invalid s3 URI: %w", err))
	}

	client, err := s3service.NewClientAmbient(ctx, s3service.Target{
		Bucket:   bucket,
		Region:   spec.Region,
		Endpoint: spec.Endpoint,
	}, spec.Profile)
	if err != nil {
		return nil, appendNoChanges(fmt.Errorf(
			"no AWS credentials found: set AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY, configure a profile "+
				"(--aws-profile or AWS_PROFILE), or run on a host with an EC2 instance role. "+
				"Note: sudo strips AWS_* variables — use sudo --preserve-env=AWS_ACCESS_KEY_ID,AWS_SECRET_ACCESS_KEY,AWS_SESSION_TOKEN,AWS_REGION -u oddk ... (%v)",
			err))
	}

	fetch, err := FetchRemoteSnapshot(ctx, client, spec.URI, key, SnapshotDownloadsDir(backupDir), progress)
	if err != nil {
		return nil, appendNoChanges(err)
	}
	return fetch, nil
}
