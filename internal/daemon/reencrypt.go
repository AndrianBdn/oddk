package daemon

import (
	"log"

	"github.com/hypersequent/oddk/internal/crypto"
	"github.com/hypersequent/oddk/internal/store"
)

// reencryptLegacySecrets upgrades stored ciphertexts from the legacy
// pre-0.1.29 format to 3ncr.org/1. It runs once at daemon startup (before
// anything can submit operations) over the two encrypted field families:
// instance postgres passwords and offsite secret access keys (every row,
// since inactive rows back the '%SAME-AS-BEFORE%' apply flow). A row is only
// rewritten after its legacy ciphertext decrypted successfully, so a
// mismatched master key can never corrupt data — the row is left as-is and
// reported. DecryptPassword keeps a legacy fallback, so unswept rows still
// work; this sweep just converges storage to the new format.
func reencryptLegacySecrets(st *store.Store, masterKey []byte) {
	upgraded, failed := 0, 0

	instances, err := st.Instances.List()
	if err != nil {
		log.Printf("Warning: ciphertext upgrade sweep skipped for instances: %v", err)
	} else {
		for _, inst := range instances {
			if !crypto.IsLegacyCiphertext(inst.Password) {
				continue
			}
			plaintext, err := crypto.DecryptPassword(inst.Password, masterKey)
			if err != nil {
				failed++
				log.Printf("Warning: cannot upgrade ciphertext of instance %s password: %v", inst.Name, err)
				continue
			}
			reencrypted, err := crypto.EncryptPassword(plaintext, masterKey)
			if err != nil {
				failed++
				log.Printf("Warning: cannot re-encrypt instance %s password: %v", inst.Name, err)
				continue
			}
			if err := st.Instances.UpdatePassword(inst.Name, reencrypted); err != nil {
				failed++
				log.Printf("Warning: cannot store re-encrypted password of instance %s: %v", inst.Name, err)
				continue
			}
			upgraded++
		}
	}

	secrets, err := st.Offsite.ListAllSecrets()
	if err != nil {
		log.Printf("Warning: ciphertext upgrade sweep skipped for offsite secrets: %v", err)
	} else {
		for _, row := range secrets {
			// Empty = EC2 IAM-role mode; REDACTED is a sentinel, not ciphertext.
			if row.SecretAccessKey == "REDACTED" || !crypto.IsLegacyCiphertext(row.SecretAccessKey) {
				continue
			}
			plaintext, err := crypto.DecryptPassword(row.SecretAccessKey, masterKey)
			if err != nil {
				failed++
				log.Printf("Warning: cannot upgrade ciphertext of offsite secret (row %d): %v", row.ID, err)
				continue
			}
			reencrypted, err := crypto.EncryptPassword(plaintext, masterKey)
			if err != nil {
				failed++
				log.Printf("Warning: cannot re-encrypt offsite secret (row %d): %v", row.ID, err)
				continue
			}
			if err := st.Offsite.UpdateSecretAccessKey(row.ID, reencrypted); err != nil {
				failed++
				log.Printf("Warning: cannot store re-encrypted offsite secret (row %d): %v", row.ID, err)
				continue
			}
			upgraded++
		}
	}

	if upgraded > 0 || failed > 0 {
		log.Printf("Ciphertext upgrade sweep: %d secret(s) re-encrypted to 3ncr.org/1, %d failed", upgraded, failed)
	}
}
