//go:build oddk_debug

package daemon

import "github.com/andrianbdn/oddk/internal/store/kvstore"

// debugAllowNewBackupPlansKey lets the e2e suite create per-instance backup
// schedules despite the deprecation — its legacy-backup and migration tests
// need schedules to exist. Deliberately NOT a registered system key, so
// `oddk customkv set` cannot reach it, and read only in oddk_debug builds:
// production binaries use the stub, which always refuses.
const debugAllowNewBackupPlansKey = kvstore.KeyInt("cron.debug_allow_new_backup_plans.int")

func (s *Server) allowNewBackupPlans() bool {
	return s.store.KV.GetIntWithDefault(debugAllowNewBackupPlansKey, 0) == 1
}
