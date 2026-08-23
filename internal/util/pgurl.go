package util

import (
	"net"
	"net/url"
	"strconv"
)

// PostgresURI builds a libpq/pgx URL with the password percent-encoded.
// Auto-generated secrets are base64url and happen to be URL-safe; passwords
// set via NEW_PGPASSWORD are not, and a raw %s interpolation breaks on
// @ : / and %.
func PostgresURI(password string, port int, database string) string {
	u := url.URL{
		Scheme: "postgres",
		User:   url.UserPassword("postgres", password),
		Host:   net.JoinHostPort(GatewayIP, strconv.Itoa(port)),
		Path:   "/" + database,
	}
	q := url.Values{}
	q.Set("sslmode", "disable")
	u.RawQuery = q.Encode()
	return u.String()
}
