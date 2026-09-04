package store_test

import (
	"strings"
	"testing"
)

// A token authenticates only in the exact form it was minted; any change to
// the id or the secret, a deleted token, or a malformed string is refused —
// and deletion takes effect on the next validation, with no cache to expire.
func TestAuthTokens(t *testing.T) {
	st := newTestStore(t)

	token, err := st.Auth.CreateToken()
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	id, secret, found := strings.Cut(token, ":")
	if !found || id == "" || secret == "" {
		t.Fatalf("token %q is not <id>:<secret>", token)
	}

	valid := func(tok string) bool {
		t.Helper()
		ok, err := st.Auth.ValidateToken(tok)
		if err != nil {
			t.Fatalf("ValidateToken(%q): %v", tok, err)
		}
		return ok
	}
	if !valid(token) {
		t.Fatal("freshly minted token does not validate")
	}

	flipped := secret[:len(secret)-1] + string('A'+(secret[len(secret)-1]+1)%26)
	for name, tok := range map[string]string{
		"tampered secret":  id + ":" + flipped,
		"truncated secret": id + ":" + secret[:len(secret)-1],
		"wrong id":         "999999:" + secret,
		"no separator":     id + secret,
		"empty":            "",
		"garbage":          "not-a-token",
	} {
		if valid(tok) {
			t.Errorf("%s validated: %q", name, tok)
		}
	}

	infos, err := st.Auth.ListTokens()
	if err != nil {
		t.Fatalf("ListTokens: %v", err)
	}
	if len(infos) != 1 || infos[0].TokenPrefix == "" || !strings.HasPrefix(secret, infos[0].TokenPrefix) {
		t.Fatalf("ListTokens = %+v, want one entry whose prefix starts the secret", infos)
	}
	if n, err := st.Auth.CountTokens(); err != nil || n != 1 {
		t.Fatalf("CountTokens = %d, %v; want 1", n, err)
	}

	if err := st.Auth.DeleteToken(infos[0].ID); err != nil {
		t.Fatalf("DeleteToken: %v", err)
	}
	if valid(token) {
		t.Fatal("deleted token still validates")
	}
	if n, _ := st.Auth.CountTokens(); n != 0 {
		t.Fatalf("CountTokens after delete = %d, want 0", n)
	}
}
