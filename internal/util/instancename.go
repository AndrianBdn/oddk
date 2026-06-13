package util

import (
	"fmt"
	"regexp"
)

// Instance names become part of Docker artifact names (container "oddk-pg-<name>",
// volume "oddk-data-<name>"), URL path segments (/api/rdbms/{name}), and backup
// file names, so they are restricted to a portable allowlist. Docker itself
// accepts [a-zA-Z0-9_.-], but dots are excluded here to keep names unambiguous
// in file names and logs.
var instanceNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)

const maxInstanceNameLength = 63

// ValidateInstanceName checks that a new instance name is safe to embed in
// Docker names, URLs and file paths. Existing instances are not re-validated.
func ValidateInstanceName(name string) error {
	if name == "" {
		return fmt.Errorf("instance name is required")
	}
	if len(name) > maxInstanceNameLength {
		return fmt.Errorf("instance name is too long (%d chars, max %d)", len(name), maxInstanceNameLength)
	}
	if !instanceNameRe.MatchString(name) {
		return fmt.Errorf("invalid instance name %q: must start with a letter or digit and contain only letters, digits, '-' and '_'", name)
	}
	return nil
}
