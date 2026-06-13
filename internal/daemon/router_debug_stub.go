//go:build !oddk_debug

package daemon

import "net/http"

// registerDebugRoutes is a no-op in production builds. The debug endpoints are
// compiled in only under the `oddk_debug` build tag (see router_debug.go).
func (s *Server) registerDebugRoutes(_ *http.ServeMux) {}
