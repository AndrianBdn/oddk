package store

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// LockFileName is the advisory lock guarding a data directory. Both the daemon
// and 'snapshot apply' take it, so the two can never operate on the same
// deployment concurrently.
const LockFileName = ".oddk.lock"

// DataDirLock is an exclusive advisory lock on a data directory.
type DataDirLock struct {
	file *os.File
}

// AcquireDataDirLock takes an exclusive, non-blocking flock on
// <dataDir>/.oddk.lock.
//
// This is what actually prevents two writers. A point-in-time port probe cannot:
// the systemd unit sets Restart=always/RestartSec=5, so a daemon that is killed
// rather than stopped returns within seconds — possibly mid-apply, at which
// point both processes drive Docker and write oddk.db, and the daemon's startup
// sweep force-removes the helper containers the restore is using.
//
// flock is released by the kernel when the process exits or the fd closes, so
// unlike a lock row in SQLite it cannot be left stale by a crash or a SIGKILL.
func AcquireDataDirLock(dataDir string) (*DataDirLock, error) {
	path := filepath.Join(dataDir, LockFileName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) // #nosec G304 - path is the daemon's own data dir
	if err != nil {
		return nil, fmt.Errorf("open lock file %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("another ODDK process holds the lock on %s (a running daemon, or another 'snapshot apply'): %w", dataDir, err)
	}
	return &DataDirLock{file: f}, nil
}

// Release drops the lock. Safe to call more than once.
func (l *DataDirLock) Release() {
	if l == nil || l.file == nil {
		return
	}
	_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	_ = l.file.Close()
	l.file = nil
}
