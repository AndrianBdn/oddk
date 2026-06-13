package operations

import "testing"

func TestQuotePostgresLiteral(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", "''"},
		{"abc", "'abc'"},
		{"with space", "'with space'"},
		{"O'Brien", "'O''Brien'"},
		{"'", "''''"},
		{"''", "''''''"},
		{"a'b'c", "'a''b''c'"},
		{`back\slash`, `'back\slash'`}, // standard_conforming_strings=on, no escaping
		{"AbC123_-", "'AbC123_-'"},     // base64url charset, the actual input today
	}
	for _, c := range cases {
		got := quotePostgresLiteral(c.in)
		if got != c.want {
			t.Errorf("quotePostgresLiteral(%q): got %q, want %q", c.in, got, c.want)
		}
	}
}
