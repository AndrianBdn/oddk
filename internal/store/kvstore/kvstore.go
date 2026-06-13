package kvstore

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/hypersequent/oddk/internal/operr"
)

// KVStore provides a key-value configuration store with caching
type KVStore struct {
	db          *sqlx.DB
	strCache    map[string]string // Cache for string values
	intCache    map[string]int    // Cache for integer values
	mutex       sync.RWMutex
	initialized bool // Flag to ensure initialization was called
}

// NewKVStore creates a new KV store instance
func NewKVStore(db *sqlx.DB) *KVStore {
	return &KVStore{
		db:       db,
		strCache: make(map[string]string),
		intCache: make(map[string]int),
	}
}

// loadCache loads all key-value pairs from database into type-specific caches
func (kv *KVStore) loadCache() error {
	records, err := kv.GetAll()
	if err != nil {
		return fmt.Errorf("load cache from database: %w", err)
	}

	kv.mutex.Lock()
	defer kv.mutex.Unlock()

	// Clear existing caches
	kv.strCache = make(map[string]string)
	kv.intCache = make(map[string]int)

	for _, record := range records {
		if strings.HasSuffix(record.Key, ".int") {
			// Integer key - parse and store in int cache
			intValue, err := strconv.Atoi(record.Value)
			if err != nil {
				return fmt.Errorf("invalid integer value for key %s: %w", record.Key, err)
			}
			kv.intCache[record.Key] = intValue
		} else if strings.HasSuffix(record.Key, ".str") {
			// String key - store in string cache
			kv.strCache[record.Key] = record.Value
		}
		// Ignore keys without proper type suffix
	}

	return nil
}

// checkInitialized panics if the KVStore has not been properly initialized
func (kv *KVStore) checkInitialized() {
	kv.mutex.RLock()
	initialized := kv.initialized
	kv.mutex.RUnlock()

	if !initialized {
		panic("KVStore not initialized - call Initialize() first")
	}
}

// GetAll retrieves all key-value pairs from the database using sqlx Select
func (kv *KVStore) GetAll() ([]*KVRecord, error) {
	var records []*KVRecord
	query := `SELECT key, value, updated_at FROM kvstore ORDER BY key`

	err := kv.db.Select(&records, query)
	if err != nil {
		return nil, fmt.Errorf("query all kvstore records: %w", err)
	}

	return records, nil
}

// dbGet retrieves a value from database only
func (kv *KVStore) dbGet(keyStr string) (string, error) {
	query := `SELECT value FROM kvstore WHERE key = ?`
	var value string
	err := kv.db.Get(&value, query, keyStr)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", operr.NotFoundf("key not found: %s", keyStr)
		}
		return "", fmt.Errorf("get kvstore value: %w", err)
	}
	return value, nil
}

// dbSet stores a value in database using sqlx NamedExec
func (kv *KVStore) dbSet(keyStr, value string) error {
	record := KVRecord{
		Key:       keyStr,
		Value:     value,
		UpdatedAt: time.Now().UTC().Format("2006-01-02T15:04:05Z07:00"),
	}

	query := `
		INSERT INTO kvstore (key, value, updated_at)
		VALUES (:key, :value, :updated_at)
		ON CONFLICT(key) DO UPDATE SET
			value = excluded.value,
			updated_at = excluded.updated_at
	`

	_, err := kv.db.NamedExec(query, record)
	if err != nil {
		return fmt.Errorf("set kvstore value: %w", err)
	}
	return nil
}

// dbDelete removes a value from database using sqlx MustExec for simplicity
func (kv *KVStore) dbDelete(keyStr string) error {
	query := `DELETE FROM kvstore WHERE key = ?`
	result, err := kv.db.Exec(query, keyStr)
	if err != nil {
		return fmt.Errorf("delete kvstore value: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete kvstore value: rows affected: %w", err)
	}
	if rowsAffected == 0 {
		return operr.NotFoundf("key not found: %s", keyStr)
	}

	return nil
}

// Get retrieves a string value by key, checking string cache only
func (kv *KVStore) Get(key Key) (string, error) {
	kv.checkInitialized()
	keyStr := key.String()

	kv.mutex.Lock()
	defer kv.mutex.Unlock()

	// Try string cache first
	if value, exists := kv.strCache[keyStr]; exists {
		return value, nil
	}

	// Cache miss - query database
	value, err := kv.dbGet(keyStr)
	if err != nil {
		return "", err
	}

	// Update only string cache
	kv.strCache[keyStr] = value

	return value, nil
}

// Set stores a string key-value pair, updating database and string cache only
func (kv *KVStore) Set(key Key, value string) error {
	kv.checkInitialized()
	keyStr := key.String()

	kv.mutex.Lock()
	defer kv.mutex.Unlock()

	err := kv.dbSet(keyStr, value)
	if err != nil {
		return err
	}

	// Update only string cache
	kv.strCache[keyStr] = value

	return nil
}

// Delete removes a string key-value pair from database and string cache only
func (kv *KVStore) Delete(key Key) error {
	keyStr := key.String()

	kv.mutex.Lock()
	defer kv.mutex.Unlock()

	err := kv.dbDelete(keyStr)
	if err != nil {
		return err
	}

	// Remove from string cache only
	delete(kv.strCache, keyStr)

	return nil
}

// GetWithDefault retrieves a value by key, returning default if not found
func (kv *KVStore) GetWithDefault(key Key, defaultValue string) string {
	value, err := kv.Get(key)
	if err != nil {
		return defaultValue
	}
	return value
}

// Initialize loads the initial cache from database and sets up system defaults
// This should be called after database migrations are complete
func (kv *KVStore) Initialize() error {
	err := kv.loadCache()
	if err != nil {
		return err
	}

	// Set initialized flag right after cache load so we can use normal functions
	kv.mutex.Lock()
	kv.initialized = true
	kv.mutex.Unlock()

	// Ensure all required system parameters exist with defaults
	err = kv.EnsureSystemDefaults()
	if err != nil {
		return fmt.Errorf("ensure system defaults: %w", err)
	}

	return nil
}

// GetInt retrieves an integer value by key, using integer cache only
func (kv *KVStore) GetInt(key KeyInt) (int, error) {
	kv.checkInitialized()
	keyStr := key.String()

	kv.mutex.Lock()
	defer kv.mutex.Unlock()

	// Try int cache first
	if value, exists := kv.intCache[keyStr]; exists {
		return value, nil
	}

	// Cache miss - query database
	value, err := kv.dbGet(keyStr)
	if err != nil {
		return 0, err
	}

	// Convert to integer
	intValue, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("database value for key %s is not a valid integer: %w", keyStr, err)
	}

	// Update only int cache
	kv.intCache[keyStr] = intValue

	return intValue, nil
}

// SetInt stores an integer key-value pair, updating database and int cache only
func (kv *KVStore) SetInt(key KeyInt, value int) error {
	kv.checkInitialized()
	keyStr := key.String()
	valueStr := strconv.Itoa(value)

	kv.mutex.Lock()
	defer kv.mutex.Unlock()

	err := kv.dbSet(keyStr, valueStr)
	if err != nil {
		return err
	}

	// Update only int cache
	kv.intCache[keyStr] = value

	return nil
}

// GetIntWithDefault retrieves an integer value by key, returning default if not found
func (kv *KVStore) GetIntWithDefault(key KeyInt, defaultValue int) int {
	value, err := kv.GetInt(key)
	if err != nil {
		return defaultValue
	}
	return value
}

// DeleteInt removes an integer key-value pair from database and int cache only
func (kv *KVStore) DeleteInt(key KeyInt) error {
	keyStr := key.String()

	kv.mutex.Lock()
	defer kv.mutex.Unlock()

	err := kv.dbDelete(keyStr)
	if err != nil {
		return err
	}

	// Remove from int cache only
	delete(kv.intCache, keyStr)

	return nil
}

// GetRaw retrieves a value by raw key string from appropriate cache or database
func (kv *KVStore) GetRaw(keyStr string) (string, error) {
	kv.mutex.Lock()
	defer kv.mutex.Unlock()

	// Try appropriate cache first based on suffix
	if strings.HasSuffix(keyStr, ".int") {
		if value, exists := kv.intCache[keyStr]; exists {
			return strconv.Itoa(value), nil
		}
	} else if strings.HasSuffix(keyStr, ".str") {
		if value, exists := kv.strCache[keyStr]; exists {
			return value, nil
		}
	}

	// Cache miss - query database
	value, err := kv.dbGet(keyStr)
	if err != nil {
		return "", err
	}

	if strings.HasSuffix(keyStr, ".int") {
		if intValue, err := strconv.Atoi(value); err == nil {
			kv.intCache[keyStr] = intValue
		}
	} else if strings.HasSuffix(keyStr, ".str") {
		kv.strCache[keyStr] = value
	}

	return value, nil
}

// SetRaw stores a value by raw key string in appropriate cache and database
func (kv *KVStore) SetRaw(keyStr, value string) error {
	kv.mutex.Lock()
	defer kv.mutex.Unlock()

	err := kv.dbSet(keyStr, value)
	if err != nil {
		return err
	}

	if strings.HasSuffix(keyStr, ".int") {
		if intValue, err := strconv.Atoi(value); err == nil {
			kv.intCache[keyStr] = intValue
		}
	} else if strings.HasSuffix(keyStr, ".str") {
		kv.strCache[keyStr] = value
	}

	return nil
}

// DeleteRaw removes a key-value pair by raw key string
func (kv *KVStore) DeleteRaw(keyStr string) error {
	kv.mutex.Lock()
	defer kv.mutex.Unlock()

	err := kv.dbDelete(keyStr)
	if err != nil {
		return err
	}

	if strings.HasSuffix(keyStr, ".int") {
		delete(kv.intCache, keyStr)
	} else if strings.HasSuffix(keyStr, ".str") {
		delete(kv.strCache, keyStr)
	}

	return nil
}

// ExistsRaw checks if a raw key string exists in the store
func (kv *KVStore) ExistsRaw(keyStr string) bool {
	_, err := kv.GetRaw(keyStr)
	return err == nil
}
