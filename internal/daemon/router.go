package daemon

import "net/http"

// setupRoutes configures the HTTP routes using Go 1.22 HTTP verb routing
func (s *Server) setupRoutes() *http.ServeMux {
	mux := http.NewServeMux()

	// Health endpoints (authenticated)
	mux.HandleFunc("GET /api/health/status", s.withAuth(s.handleHealthStatus))
	mux.HandleFunc("GET /api/health/history", s.withAuth(s.handleHealthHistory))

	// Image management
	mux.HandleFunc("POST /api/pull", s.withAuth(s.handlePullImage))

	// RDBMS instance management
	mux.HandleFunc("GET /api/rdbms", s.withAuth(s.handleListRDBMS))
	mux.HandleFunc("POST /api/rdbms", s.withAuth(s.handleCreateRDBMS))
	mux.HandleFunc("GET /api/rdbms/{name}", s.withAuth(s.handleGetRDBMS))
	mux.HandleFunc("GET /api/rdbms/{name}/logs", s.withAuth(s.handleGetRDBMSLogs))
	mux.HandleFunc("DELETE /api/rdbms/{name}", s.withAuth(s.handleDeleteRDBMS))
	mux.HandleFunc("PUT /api/rdbms/{name}/state", s.withAuth(s.handleUpdateRDBMSState))
	mux.HandleFunc("PUT /api/rdbms/{name}/config", s.withAuth(s.handleReconfigureRDBMS))
	mux.HandleFunc("PUT /api/rdbms/{name}/image", s.withAuth(s.handleSwitchRDBMS))
	mux.HandleFunc("POST /api/rdbms/{name}/major-upgrade", s.withAuth(s.handleMajorUpgradeRDBMS))

	// Database management within instances
	mux.HandleFunc("GET /api/rdbms/{name}/databases", s.withAuth(s.handleListDatabases))
	mux.HandleFunc("POST /api/rdbms/{name}/databases", s.withAuth(s.handleCreateDatabase))
	mux.HandleFunc("POST /api/rdbms/{name}/databases/{database}/users", s.withAuth(s.handleAddDatabaseUser))
	mux.HandleFunc("DELETE /api/rdbms/{name}/users/{username}", s.withAuth(s.handleDeleteDatabaseUser))
	mux.HandleFunc("PUT /api/rdbms/{name}/users/{username}/password", s.withAuth(s.handleResetDatabaseUserPassword))

	// Backup management
	mux.HandleFunc("POST /api/rdbms/{name}/backup", s.withAuth(s.handleRDBMSBackup))
	mux.HandleFunc("GET /api/rdbms/{name}/backups", s.withAuth(s.handleListBackups))
	mux.HandleFunc("POST /api/rdbms/{name}/backup/{id}/upload", s.withAuth(s.handleUploadBackup))
	mux.HandleFunc("POST /api/rdbms/{name}/backup/{id}/download", s.withAuth(s.handleDownloadBackup))
	mux.HandleFunc("DELETE /api/rdbms/{name}/backup/{id}/local", s.withAuth(s.handleRemoveLocalBackup))
	mux.HandleFunc("DELETE /api/rdbms/{name}/backup/{id}/remote", s.withAuth(s.handleRemoveRemoteBackup))
	mux.HandleFunc("GET /api/backups", s.withAuth(s.handleListAllBackups))
	mux.HandleFunc("POST /api/rdbms/{name}/restore", s.withAuth(s.handleRDBMSRestore))

	// Password management
	mux.HandleFunc("GET /api/rdbms/{name}/password", s.withAuth(s.handleGetPassword))
	mux.HandleFunc("PUT /api/rdbms/{name}/password", s.withAuth(s.handleSetPassword))

	// Notification management
	mux.HandleFunc("POST /api/notifications", s.withAuth(s.handleNotificationAdd))
	mux.HandleFunc("GET /api/notifications", s.withAuth(s.handleNotificationList))
	mux.HandleFunc("GET /api/notifications/{name}", s.withAuth(s.handleNotificationGet))
	mux.HandleFunc("PUT /api/notifications/{name}", s.withAuth(s.handleNotificationEdit))
	mux.HandleFunc("DELETE /api/notifications/{name}", s.withAuth(s.handleNotificationRemove))
	mux.HandleFunc("POST /api/notifications/test", s.withAuth(s.handleNotificationTest))
	mux.HandleFunc("GET /api/notifications/logs", s.withAuth(s.handleNotificationLogs))

	// Cron management
	mux.HandleFunc("POST /api/cron/backup", s.withAuth(s.handleCronBackupCreate))
	mux.HandleFunc("GET /api/cron/backup", s.withAuth(s.handleCronBackupList))
	mux.HandleFunc("DELETE /api/cron/backup/{instance}", s.withAuth(s.handleCronBackupDelete))

	// Parameter groups
	mux.HandleFunc("GET /api/parameters", s.withAuth(s.handleParameterGroupList))
	mux.HandleFunc("GET /api/parameters/{name}", s.withAuth(s.handleParameterGroupGet))
	mux.HandleFunc("PUT /api/parameters/{name}", s.withAuth(s.handleParameterGroupPut))
	mux.HandleFunc("DELETE /api/parameters/{name}", s.withAuth(s.handleParameterGroupDelete))

	// Custom Key-Value store
	mux.HandleFunc("GET /api/customkv", s.withAuth(s.handleListCustomKV))
	mux.HandleFunc("GET /api/customkv/{key}", s.withAuth(s.handleGetCustomKV))
	mux.HandleFunc("PUT /api/customkv/{key}", s.withAuth(s.handleSetCustomKV))

	// Offsite backup management
	mux.HandleFunc("GET /api/offsite", s.withAuth(s.handleOffsiteInfo))
	mux.HandleFunc("GET /api/offsite/config", s.withAuth(s.handleOffsiteGet))
	mux.HandleFunc("PUT /api/offsite/config", s.withAuth(s.handleOffsiteApply))
	mux.HandleFunc("DELETE /api/offsite/config", s.withAuth(s.handleOffsiteRemove))
	mux.HandleFunc("GET /api/offsite/logs", s.withAuth(s.handleOffsiteLogs))
	mux.HandleFunc("POST /api/offsite/test", s.withAuth(s.handleOffsiteTest))

	// Debug-only endpoints — registered only in `oddk_debug` builds (e2e);
	// a no-op stub in production builds (see router_debug.go / _stub.go).
	s.registerDebugRoutes(mux)

	return mux
}
