package auth

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
	"golang.org/x/crypto/argon2"
)

// simplified argon2 complexity - normally this token lives on the same computer or transmited over ssh
const (
	argon2Time      = 1
	argon2Memory    = 32
	argon2Threads   = 1
	argon2KeyLength = 32
	saltLength      = 16
	tokenPrefixLen  = 8
)

type AuthStore struct {
	db *sqlx.DB
}

func NewAuthStore(db *sqlx.DB) *AuthStore {
	return &AuthStore{db: db}
}

func (s *AuthStore) CreateToken() (string, error) {
	// Generate 32 bytes of random data
	randomData := make([]byte, 32)
	if _, err := rand.Read(randomData); err != nil {
		return "", fmt.Errorf("generate random data: %w", err)
	}

	// Base64url encode the random data
	encodedData := base64.URLEncoding.EncodeToString(randomData)

	// Take first characters as prefix
	tokenPrefix := encodedData[:tokenPrefixLen]

	// Hash the full random data (binary) with Argon2
	salt := make([]byte, saltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}

	hash := argon2.IDKey(randomData, salt, argon2Time, argon2Memory, argon2Threads, argon2KeyLength)

	// Store salt + hash together
	saltAndHash := make([]byte, 0, len(salt)+len(hash))
	saltAndHash = append(saltAndHash, salt...)
	saltAndHash = append(saltAndHash, hash...)
	hashStr := base64.URLEncoding.EncodeToString(saltAndHash)

	// Insert into database
	result, err := s.db.Exec(`
		INSERT INTO auth_tokens (token_prefix, token_hash, created_at)
		VALUES (?, ?, ?)
	`, tokenPrefix, hashStr, time.Now().Format(time.RFC3339))
	if err != nil {
		return "", fmt.Errorf("insert auth token: %w", err)
	}

	tokenID, err := result.LastInsertId()
	if err != nil {
		return "", fmt.Errorf("get token id: %w", err)
	}

	return fmt.Sprintf("%d:%s", tokenID, encodedData), nil
}

func (s *AuthStore) ValidateToken(token string) (bool, error) {
	parts := strings.SplitN(token, ":", 2)
	if len(parts) != 2 {
		return false, nil // Invalid format
	}

	tokenIDStr, encodedData := parts[0], parts[1]
	tokenID, err := strconv.ParseInt(tokenIDStr, 10, 64)
	if err != nil {
		return false, nil // Invalid ID format
	}

	var storedToken struct {
		ID          int64  `db:"id"`
		TokenPrefix string `db:"token_prefix"`
		TokenHash   string `db:"token_hash"`
		CreatedAt   string `db:"created_at"`
	}
	err = s.db.Get(&storedToken, `
		SELECT * FROM auth_tokens WHERE id = ?
	`, tokenID)

	if err == sql.ErrNoRows {
		return false, nil // Token not found
	}
	if err != nil {
		return false, fmt.Errorf("get auth token: %w", err)
	}

	// Quick prefix check for early rejection
	if len(encodedData) < tokenPrefixLen || encodedData[:tokenPrefixLen] != storedToken.TokenPrefix {
		return false, nil // Prefix doesn't match
	}

	// Decode the base64url data back to binary
	randomData, err := base64.URLEncoding.DecodeString(encodedData)
	if err != nil {
		return false, nil // Invalid base64url encoding
	}

	// Decode the stored salt + hash
	saltAndHash, err := base64.URLEncoding.DecodeString(storedToken.TokenHash)
	expectedLen := saltLength + argon2KeyLength
	if err != nil || len(saltAndHash) != expectedLen {
		return false, fmt.Errorf("invalid stored hash format")
	}

	salt := saltAndHash[:saltLength]
	storedHash := saltAndHash[saltLength:]

	// Hash the binary random data with the same salt
	computedHash := argon2.IDKey(randomData, salt, argon2Time, argon2Memory, argon2Threads, argon2KeyLength)

	// Constant time comparison
	if len(computedHash) != len(storedHash) {
		return false, nil
	}

	var result byte
	for i := range computedHash {
		result |= computedHash[i] ^ storedHash[i]
	}

	return result == 0, nil
}

func (s *AuthStore) GetOrCreateToken() (string, error) {
	var count int
	err := s.db.Get(&count, "SELECT COUNT(*) FROM auth_tokens")
	if err != nil {
		return "", fmt.Errorf("count auth tokens: %w", err)
	}

	if count == 0 {
		return s.CreateToken()
	}

	// Token already exists, cannot retrieve plaintext
	return "", fmt.Errorf("token already exists, cannot retrieve plaintext")
}
