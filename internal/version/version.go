package version

import (
	"fmt"
	"runtime/debug"
	"strings"
)

// Version is the semantic version of ODDK
// During development (0.1.x), PATCH increments for any change regardless of type
const Version = "0.1.42"

// BuildInfo contains version and build information
type BuildInfo struct {
	Version     string
	GitCommit   string
	GitTime     string
	GitModified bool
	GoVersion   string
}

// GetBuildInfo returns version and build information from embedded VCS data
func GetBuildInfo() BuildInfo {
	info := BuildInfo{
		Version:     Version,
		GitCommit:   "unknown",
		GitTime:     "unknown",
		GitModified: false,
		GoVersion:   "unknown",
	}

	buildInfo, ok := debug.ReadBuildInfo()
	if !ok {
		return info
	}

	info.GoVersion = buildInfo.GoVersion

	for _, setting := range buildInfo.Settings {
		switch setting.Key {
		case "vcs.revision":
			// Use short commit hash (first 7 chars)
			if len(setting.Value) >= 7 {
				info.GitCommit = setting.Value[:7]
			} else {
				info.GitCommit = setting.Value
			}
		case "vcs.time":
			info.GitTime = setting.Value
		case "vcs.modified":
			info.GitModified = setting.Value == "true"
		}
	}

	return info
}

// String returns a human-readable version string
func (bi BuildInfo) String() string {
	var parts []string

	parts = append(parts, fmt.Sprintf("ODDK v%s", bi.Version))

	if bi.GitCommit != "unknown" {
		commitInfo := bi.GitCommit
		if bi.GitModified {
			commitInfo += " (dirty)"
		}
		parts = append(parts, fmt.Sprintf("commit %s", commitInfo))
	}

	if bi.GitTime != "unknown" {
		parts = append(parts, fmt.Sprintf("built %s", bi.GitTime))
	}

	if bi.GoVersion != "unknown" {
		parts = append(parts, fmt.Sprintf("with %s", bi.GoVersion))
	}

	return strings.Join(parts, ", ")
}

// Short returns a short version string (version + commit)
func (bi BuildInfo) Short() string {
	if bi.GitCommit != "unknown" {
		dirty := ""
		if bi.GitModified {
			dirty = "-dirty"
		}
		return fmt.Sprintf("%s-%s%s", bi.Version, bi.GitCommit, dirty)
	}
	return bi.Version
}
