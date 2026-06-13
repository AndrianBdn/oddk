//go:build oddk_debug

package daemon

// DebugSetRawKV writes a raw key-value pair straight into the KV store,
// bypassing the public-API guard that only permits modifying existing system
// parameters. Test-only: compiled in solely under the `oddk_debug` build tag,
// where the e2e harness uses it to seed debug knobs (e.g.
// backup.debug_time_machine.int) before the server starts. It never ships in
// production binaries and is reachable by no HTTP route.
func (s *Server) DebugSetRawKV(key, value string) error {
	return s.store.KV.SetRaw(key, value)
}
