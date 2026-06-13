package operations

import "testing"

func TestValidatePortableDBName(t *testing.T) {
	valid := []string{
		"mydb",
		"my_app",
		"my-app",
		"MixedCase",
		"my.app", // dots are legal PG identifiers
		"café",   // unicode
		"with space",
		"db123",
		"_leading_underscore",
	}
	for _, name := range valid {
		if err := validatePortableDBName(name); err != nil {
			t.Errorf("validatePortableDBName(%q) = %v, want nil (legal portable name)", name, err)
		}
	}

	invalid := []string{
		"",               // empty
		".",              // current dir
		"..",             // parent dir
		"foo/bar",        // forward slash
		"../evil",        // traversal
		"../../tmp/evil", // deeper traversal
		"a/b/c",          // nested
		`foo\bar`,        // backslash
		"foo/",           // trailing slash
		"/abs",           // leading slash
	}
	for _, name := range invalid {
		if err := validatePortableDBName(name); err == nil {
			t.Errorf("validatePortableDBName(%q) = nil, want error (path-unsafe name)", name)
		}
	}
}
