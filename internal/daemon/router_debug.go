//go:build oddk_debug

package daemon

import "net/http"

// registerDebugRoutes wires the debug-only endpoints. Compiled in only under the
// `oddk_debug` build tag (used by the e2e suite); production builds get the
// no-op stub in router_debug_stub.go, so these routes never ship.
func (s *Server) registerDebugRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/debug/backup/time-shift", s.withAuth(s.handleDebugBackupTimeShift))
}
