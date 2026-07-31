package daemon

import (
	"context"
	"log"
	"time"

	"github.com/andrianbdn/oddk/internal/store/kvstore"
)

// logRetentionSweepInterval is how often operational logs are swept. The
// retention window is measured in days, so checking daily is granular enough.
const logRetentionSweepInterval = 24 * time.Hour

// startLogRetentionSweeper trims cron_logs, notification_logs and offsite_logs
// to the window configured by logs.retention_days.int.
//
// cron_logs and offsite_logs are the evidence that backups ran and were shipped
// offsite, so this is a compliance control, not just housekeeping — it has to
// actually run, and it has to be visible in the journal that it did. It sweeps
// once at startup and then daily.
//
// health is deliberately NOT covered here: it is high-frequency telemetry
// (~1200 rows/day) already capped at 3 days by the health checker itself.
func (s *Server) startLogRetentionSweeper(ctx context.Context) {
	if days := s.store.KV.RequiredInt(kvstore.KeyLogsRetentionDays); days > 0 {
		log.Printf("Starting log retention sweeper (retention: %d days)", days)
	} else {
		log.Printf("Starting log retention sweeper (retention disabled - logs kept indefinitely)")
	}

	s.sweepOldLogs()

	ticker := time.NewTicker(logRetentionSweepInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("Log retention sweeper shutting down")
			return
		case <-ticker.C:
			s.sweepOldLogs()
		}
	}
}

// sweepOldLogs deletes log rows older than the configured retention window.
// The window is re-read every sweep so an operator can change the policy
// without restarting the daemon. A failure in one table does not stop the
// others.
func (s *Server) sweepOldLogs() {
	days := s.store.KV.RequiredInt(kvstore.KeyLogsRetentionDays)
	if days <= 0 {
		return
	}
	window := time.Duration(days) * 24 * time.Hour

	sweeps := []struct {
		name string
		fn   func(time.Duration) (int64, error)
	}{
		{"cron", s.store.Cron.CleanupOldLogs},
		{"notification", s.store.Notifications.CleanupOldLogs},
		{"offsite", s.store.Offsite.CleanupOldLogs},
	}

	var total int64
	for _, sweep := range sweeps {
		deleted, err := sweep.fn(window)
		if err != nil {
			log.Printf("Log retention: %s log cleanup failed: %v", sweep.name, err)
			continue
		}
		total += deleted
	}

	if total > 0 {
		log.Printf("Log retention: removed %d log records older than %d days", total, days)
	}
}
