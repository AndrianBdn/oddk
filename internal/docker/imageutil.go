package docker

import (
	"regexp"
	"strings"
)

var (
	pgTagVersionRe         = regexp.MustCompile(`\bpg(\d+)`)
	leadingDigitsWithSepRe = regexp.MustCompile(`^(\d+)[-.]`)
)

// DetectPGVersionFromImage tries to parse the PostgreSQL major version from an image name.
// Handles:
//
//	"postgres:17"                    → "17"
//	"postgres:17.2"                  → "17"
//	"pgvector/pgvector:pg18-trixie"  → "18"
//	"pgvector/pgvector:0.8.2-pg18"   → "18"
//	"postgis/postgis:18-3.6"         → "18"
//
// Returns major version string and whether detection succeeded.
func DetectPGVersionFromImage(imageName string) (string, bool) {
	// 1. Official postgres image: postgres:<tag>
	if strings.HasPrefix(imageName, "postgres:") {
		tag := imageName[len("postgres:"):]
		if tag == "" {
			return "", false
		}
		end := 0
		for end < len(tag) && tag[end] >= '0' && tag[end] <= '9' {
			end++
		}
		if end > 0 {
			return tag[:end], true
		}
		return "", false
	}

	tag := ""
	if idx := strings.LastIndex(imageName, ":"); idx >= 0 {
		tag = imageName[idx+1:]
	}

	// 2. Look for pg(\d+) in the tag portion (e.g., pg18-trixie, 0.8.2-pg18)
	if tag != "" {
		if m := pgTagVersionRe.FindStringSubmatch(tag); m != nil {
			return m[1], true
		}
	}

	// 3. Look for ^(\d+)[-.]  at the start of the tag (e.g., 18-3.6)
	if tag != "" {
		if m := leadingDigitsWithSepRe.FindStringSubmatch(tag); m != nil {
			return m[1], true
		}
	}

	return "", false
}
