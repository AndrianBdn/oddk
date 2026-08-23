package util_test

import (
	"net/url"
	"testing"

	"github.com/andrianbdn/oddk/internal/util"
)

func TestPostgresURI_EncodesPasswordSpecialChars(t *testing.T) {
	got := util.PostgresURI("p@ss:word/x%y", 5432, "postgres")
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse %q: %v", got, err)
	}
	pass, ok := u.User.Password()
	if !ok || pass != "p@ss:word/x%y" {
		t.Fatalf("decoded password = %q (ok=%v), want the original", pass, ok)
	}
	if u.Host != "10.88.0.1:5432" {
		t.Fatalf("host = %q", u.Host)
	}
	if u.Path != "/postgres" {
		t.Fatalf("path = %q", u.Path)
	}
}

func TestPostgresURI_Base64URLPasswordRoundTrip(t *testing.T) {
	// Auto-generated secrets look like this; they must still authenticate.
	secret := "abcdefghijABCDEFGHIJ0123"
	got := util.PostgresURI(secret, 15432, "app")
	u, err := url.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	pass, _ := u.User.Password()
	if pass != secret {
		t.Fatalf("password = %q, want %q", pass, secret)
	}
}
