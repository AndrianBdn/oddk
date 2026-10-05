package operations

import "testing"

// Every legal database name must reach pg_dump/pg_restore as exactly that one
// database. Verified against real libpq (psql/pg_dump 17) for each of these:
// the quoted form connects to the literal name.
func TestPgConninfoDBName(t *testing.T) {
	cases := map[string]string{
		"sales":                 `dbname='sales'`,
		"dbname=postgres":       `dbname='dbname=postgres'`, // would otherwise be a conninfo string
		"postgres://x/postgres": `dbname='postgres://x/postgres'`,
		"-v":                    `dbname='-v'`, // would otherwise be a pg_dump option
		"it's":                  `dbname='it\'s'`,
		`back\slash`:            `dbname='back\\slash'`,
	}
	for name, want := range cases {
		if got := pgConninfoDBName(name); got != want {
			t.Errorf("pgConninfoDBName(%q) = %s, want %s", name, got, want)
		}
	}
}

func TestContainerNameSafe(t *testing.T) {
	cases := map[string]string{
		"sales":           "sales",
		"my db":           "my_db",
		"dbname=postgres": "dbname_postgres",
		"Ünïcode.v2-x_y":  "_n_code.v2-x_y",
	}
	for name, want := range cases {
		if got := containerNameSafe(name); got != want {
			t.Errorf("containerNameSafe(%q) = %q, want %q", name, got, want)
		}
	}
}
