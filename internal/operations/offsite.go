package operations

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/hypersequent/oddk/internal/crypto"
	"github.com/hypersequent/oddk/internal/services/s3"
	"github.com/hypersequent/oddk/internal/store/offsite"
)

type OffsiteInfoResult struct {
	Active bool                     `json:"active"`
	Config *offsite.OffsiteSettings `json:"config,omitempty"`
}

func OffsiteInfo(deps *Dependencies) (*OffsiteInfoResult, error) {
	settings, err := deps.Store.Offsite.GetActive()
	if err != nil {
		return nil, fmt.Errorf("get active offsite settings: %w", err)
	}

	if settings == nil {
		return &OffsiteInfoResult{Active: false}, nil
	}

	// Clear the secret key for display
	settings.SecretAccessKey = ""

	return &OffsiteInfoResult{
		Active: true,
		Config: settings,
	}, nil
}

type OffsiteLogsParams struct {
	Limit int
}

func OffsiteLogs(deps *Dependencies, params *OffsiteLogsParams) ([]offsite.OffsiteLog, error) {
	logs, err := deps.Store.Offsite.GetLogs(params.Limit)
	if err != nil {
		return nil, fmt.Errorf("get offsite logs: %w", err)
	}
	return logs, nil
}

type OffsiteGetResult struct {
	Config offsite.OffsiteSettingsJSON `json:"config"`
}

func OffsiteGet(deps *Dependencies) (*OffsiteGetResult, error) {
	settings, err := deps.Store.Offsite.GetActive()
	if err != nil {
		return nil, fmt.Errorf("get active offsite settings: %w", err)
	}

	// If no active config, return a template
	if settings == nil {
		return &OffsiteGetResult{
			Config: offsite.OffsiteSettingsJSON{
				Type:            offsite.TypeS3,
				Bucket:          "my-backup-bucket",
				Region:          new("us-east-1"),
				AccessKeyID:     "YOUR_ACCESS_KEY_ID",
				SecretAccessKey: "YOUR_SECRET_ACCESS_KEY",
				BucketPath:      new("oddk-backups/"),
				EC2IAMRole:      false,
			},
		}, nil
	}

	// Return existing config with placeholder for secret (unless empty)
	secretPlaceholder := offsite.PlaceholderSecretKey
	if settings.SecretAccessKey == "" {
		secretPlaceholder = ""
	}

	return &OffsiteGetResult{
		Config: offsite.OffsiteSettingsJSON{
			Type:            settings.Type,
			Bucket:          settings.Bucket,
			Endpoint:        settings.Endpoint,
			Region:          settings.Region,
			AccessKeyID:     settings.AccessKeyID,
			SecretAccessKey: secretPlaceholder,
			BucketPath:      settings.BucketPath,
			EC2IAMRole:      settings.EC2IAMRole,
		},
	}, nil
}

type OffsiteApplyParams struct {
	ConfigJSON []byte
}

func OffsiteApply(deps *Dependencies, params *OffsiteApplyParams) error {
	var config offsite.OffsiteSettingsJSON
	if err := json.Unmarshal(params.ConfigJSON, &config); err != nil {
		return fmt.Errorf("parse JSON config: %w", err)
	}

	if err := offsite.ValidateOffsiteConfig(&config); err != nil {
		return fmt.Errorf("configuration validation failed: %w", err)
	}

	secretKey := config.SecretAccessKey
	var encryptedKey string

	switch secretKey {
	case offsite.PlaceholderSecretKey:
		previousKey, err := deps.Store.Offsite.GetPreviousSecretKey()
		if err != nil {
			return fmt.Errorf("no previous secret key available: %w", err)
		}

		// If previous key is empty, keep it empty
		if previousKey == "" {
			encryptedKey = ""
		} else {
			// Decrypt the previous key
			decryptedKey, err := crypto.DecryptPassword(previousKey, deps.MasterKey)
			if err != nil {
				return fmt.Errorf("decrypt previous secret key: %w", err)
			}
			// Re-encrypt it (in case the master key changed, though unlikely)
			secretKey = decryptedKey
			encryptedKey, err = crypto.EncryptPassword(secretKey, deps.MasterKey)
			if err != nil {
				return fmt.Errorf("encrypt secret key: %w", err)
			}
		}
	case "":
		// Empty secret key - store as empty without encryption
		encryptedKey = ""
	default:
		// Non-empty secret key - encrypt it
		var err error
		encryptedKey, err = crypto.EncryptPassword(secretKey, deps.MasterKey)
		if err != nil {
			return fmt.Errorf("encrypt secret key: %w", err)
		}
	}

	// Normalize bucket path: "/" means root, same as empty string
	bucketPath := config.BucketPath
	if bucketPath != nil && *bucketPath == "/" {
		bucketPath = new("")
	}

	settings := &offsite.OffsiteSettings{
		Type:            config.Type,
		Bucket:          config.Bucket,
		Endpoint:        config.Endpoint,
		Region:          config.Region,
		AccessKeyID:     config.AccessKeyID,
		SecretAccessKey: encryptedKey,
		BucketPath:      bucketPath,
		EC2IAMRole:      config.EC2IAMRole,
	}

	if err := deps.Store.Offsite.Create(settings); err != nil {
		return fmt.Errorf("create offsite settings: %w", err)
	}

	return nil
}

func OffsiteRemove(deps *Dependencies) error {
	if err := deps.Store.Offsite.Remove(); err != nil {
		return fmt.Errorf("remove offsite settings: %w", err)
	}
	return nil
}

type OffsiteTestResult struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	Error   string `json:"error,omitempty"`
}

// GetActiveOffsiteSettingsDecrypted returns the active offsite settings with the
// SecretAccessKey decrypted, ready to hand to an S3 client. It returns a copy so
// the stored ciphertext is never mutated, and never lets the encrypted secret
// reach an S3 auth path (the bug that left cron remote cleanup unable to delete).
// For EC2 IAM-role mode (or an empty secret) the secret is left empty.
// Returns (nil, nil) when no offsite configuration is active.
func GetActiveOffsiteSettingsDecrypted(deps *Dependencies) (*offsite.OffsiteSettings, error) {
	settings, err := deps.Store.Offsite.GetActive()
	if err != nil {
		return nil, fmt.Errorf("get active offsite settings: %w", err)
	}
	if settings == nil {
		return nil, nil
	}

	decrypted := *settings
	if !decrypted.EC2IAMRole && decrypted.SecretAccessKey != "" {
		secretKey, err := crypto.DecryptPassword(decrypted.SecretAccessKey, deps.MasterKey)
		if err != nil {
			return nil, fmt.Errorf("decrypt offsite secret key: %w", err)
		}
		decrypted.SecretAccessKey = secretKey
	}
	return &decrypted, nil
}

func OffsiteTest(deps *Dependencies) (*OffsiteTestResult, error) {
	settings, err := GetActiveOffsiteSettingsDecrypted(deps)
	if err != nil {
		return &OffsiteTestResult{
			Success: false,
			Error:   fmt.Sprintf("Failed to load offsite settings: %v", err),
		}, nil
	}

	if settings == nil {
		return &OffsiteTestResult{
			Success: false,
			Error:   "No active offsite configuration found",
		}, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	s3Client, err := s3.NewClient(ctx, settings)
	if err != nil {
		return &OffsiteTestResult{
			Success: false,
			Error:   fmt.Sprintf("Failed to create S3 client: %v", err),
		}, nil
	}

	// Generate test file name
	now := time.Now()
	randomBig, _ := rand.Int(rand.Reader, big.NewInt(9999)) // #nosec G104 - fallback is fine
	randomInt := randomBig.Int64()
	testKey := fmt.Sprintf("test%s-%04d.txt", now.Format("20060102-150405"), randomInt)
	testContent := "This is a test file from ODDK to check offsite operations"

	err = s3Client.UploadFile(ctx, testKey, strings.NewReader(testContent))
	if err != nil {
		logErr := deps.Store.Offsite.AddLog(&offsite.OffsiteLog{
			Event:             "test",
			OffsiteSettingsID: settings.ID,
			Object:            testKey,
			Success:           false,
			ErrorDetails:      new(fmt.Sprintf("Upload failed: %v", err)),
		})
		if logErr != nil {
			deps.Logger.Printf("Failed to log offsite test error: %v", logErr)
		}

		return &OffsiteTestResult{
			Success: false,
			Error:   fmt.Sprintf("Failed to upload test file: %v", err),
		}, nil
	}

	downloadedContent, err := s3Client.DownloadFile(ctx, testKey)
	if err != nil {
		// Try to clean up
		_ = s3Client.DeleteFile(ctx, testKey)

		logErr := deps.Store.Offsite.AddLog(&offsite.OffsiteLog{
			Event:             "test",
			OffsiteSettingsID: settings.ID,
			Object:            testKey,
			Success:           false,
			ErrorDetails:      new(fmt.Sprintf("Download failed: %v", err)),
		})
		if logErr != nil {
			deps.Logger.Printf("Failed to log offsite test error: %v", logErr)
		}

		return &OffsiteTestResult{
			Success: false,
			Error:   fmt.Sprintf("Failed to download test file: %v", err),
		}, nil
	}

	// Verify content matches
	if string(downloadedContent) != testContent {
		// Try to clean up
		_ = s3Client.DeleteFile(ctx, testKey)

		logErr := deps.Store.Offsite.AddLog(&offsite.OffsiteLog{
			Event:             "test",
			OffsiteSettingsID: settings.ID,
			Object:            testKey,
			Success:           false,
			ErrorDetails:      new("Downloaded content does not match uploaded content"),
		})
		if logErr != nil {
			deps.Logger.Printf("Failed to log offsite test error: %v", logErr)
		}

		return &OffsiteTestResult{
			Success: false,
			Error:   "Downloaded content does not match uploaded content",
		}, nil
	}

	err = s3Client.DeleteFile(ctx, testKey)
	if err != nil {
		logErr := deps.Store.Offsite.AddLog(&offsite.OffsiteLog{
			Event:             "test",
			OffsiteSettingsID: settings.ID,
			Object:            testKey,
			Success:           false,
			ErrorDetails:      new(fmt.Sprintf("Delete failed: %v", err)),
		})
		if logErr != nil {
			deps.Logger.Printf("Failed to log offsite test error: %v", logErr)
		}

		return &OffsiteTestResult{
			Success: false,
			Error:   fmt.Sprintf("Failed to delete test file: %v", err),
		}, nil
	}

	logErr := deps.Store.Offsite.AddLog(&offsite.OffsiteLog{
		Event:             "test",
		OffsiteSettingsID: settings.ID,
		Object:            testKey,
		Success:           true,
	})
	if logErr != nil {
		deps.Logger.Printf("Failed to log offsite test success: %v", logErr)
	}

	return &OffsiteTestResult{
		Success: true,
		Message: fmt.Sprintf("Offsite configuration test passed. Test file '%s' was successfully uploaded, verified, and deleted.", testKey),
	}, nil
}
