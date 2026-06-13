package util

import (
	"crypto/rand"
	"encoding/base64"
)

func GenerateSecurePassword(length int) string {
	bytes := make([]byte, length)
	// crypto/rand.Read is documented to never fail as of Go 1.24 (it aborts
	// the program itself on an unrecoverable kernel failure), so this panic
	// branch is unreachable belt-and-braces rather than a real failure mode.
	if _, err := rand.Read(bytes); err != nil {
		panic("failed to generate random bytes for password: " + err.Error())
	}
	return base64.URLEncoding.EncodeToString(bytes)[:length]
}
