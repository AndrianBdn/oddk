package operations

import (
	"strings"
	"testing"
)

// Default privileges only apply to objects created by the role that issued
// them, so grantStatements must emit each ALTER DEFAULT PRIVILEGES statement
// both as postgres and FOR ROLE the database owner (when that owner is a
// distinct regular role) — otherwise the new user silently loses access to
// tables the owner creates later.
func TestGrantStatementsDefaultPrivilegeRoles(t *testing.T) {
	cases := []struct {
		name         string
		readOnly     bool
		dbOwner      string
		wantForRole  int // FOR ROLE statements expected
		wantDefaults int // total ALTER DEFAULT PRIVILEGES statements expected
	}{
		{"readonly postgres-owned", true, "postgres", 0, 1},
		{"readonly service-owned", true, "svc", 1, 2},
		{"readwrite postgres-owned", false, "postgres", 0, 3},
		{"readwrite service-owned", false, "svc", 3, 6},
		{"owner is the new user", false, "reader", 0, 3},
		{"unknown owner", false, "", 0, 3},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var defaults, forRole int
			for _, step := range grantStatements("appdb", "reader", c.readOnly, c.dbOwner) {
				if strings.HasPrefix(step.sql, "ALTER DEFAULT PRIVILEGES") {
					defaults++
					if strings.Contains(step.sql, "FOR ROLE") {
						forRole++
					}
				}
			}
			if defaults != c.wantDefaults {
				t.Errorf("got %d ALTER DEFAULT PRIVILEGES statements, want %d", defaults, c.wantDefaults)
			}
			if forRole != c.wantForRole {
				t.Errorf("got %d FOR ROLE statements, want %d", forRole, c.wantForRole)
			}
		})
	}
}
