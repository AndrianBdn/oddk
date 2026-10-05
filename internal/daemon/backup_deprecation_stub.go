//go:build !oddk_debug

package daemon

// allowNewBackupPlans is always false in production builds: per-instance
// backups are deprecated and new schedules are refused. The oddk_debug build
// (backup_deprecation_debug.go) lets the e2e suite opt out.
func (s *Server) allowNewBackupPlans() bool { return false }
