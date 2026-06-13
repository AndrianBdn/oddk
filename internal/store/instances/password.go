package instances

import (
	"fmt"

	"github.com/hypersequent/oddk/internal/crypto"
)

// GetDecryptedPassword returns the decrypted password for an instance
func (s *InstanceStore) GetDecryptedPassword(name string, masterKey []byte) (string, error) {
	instance, err := s.Get(name)
	if err != nil {
		return "", err
	}

	password, err := crypto.DecryptPassword(instance.Password, masterKey)
	if err != nil {
		return "", fmt.Errorf("decrypt password: %w", err)
	}

	return password, nil
}
