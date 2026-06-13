package daemon

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/hypersequent/oddk/internal/operr"
)

// KVPair represents a key-value pair for API responses
type KVPair struct {
	Key       string `json:"key"`
	Value     string `json:"value"`
	UpdatedAt string `json:"updatedAt"`
}

// KVSetRequest represents the request body for setting a key-value pair
type KVSetRequest struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// handleListCustomKV handles GET /api/customkv - list all custom key-value pairs
func (s *Server) handleListCustomKV(w http.ResponseWriter, r *http.Request) {
	records, err := s.store.KV.GetAll()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Convert to API response format
	pairs := make([]KVPair, len(records))
	for i, record := range records {
		pairs[i] = KVPair{
			Key:       record.Key,
			Value:     record.Value,
			UpdatedAt: record.UpdatedAt,
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(pairs)
}

// handleGetCustomKV handles GET /api/customkv/{key} - get a specific key-value pair
func (s *Server) handleGetCustomKV(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")

	if key == "" {
		http.Error(w, "key parameter is required", http.StatusBadRequest)
		return
	}

	value, err := s.store.KV.GetRaw(key)
	if err != nil {
		if errors.Is(err, operr.ErrNotFound) {
			http.Error(w, "key not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	records, err := s.store.KV.GetAll()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var updatedAt string
	for _, record := range records {
		if record.Key == key {
			updatedAt = record.UpdatedAt
			break
		}
	}

	pair := KVPair{
		Key:       key,
		Value:     value,
		UpdatedAt: updatedAt,
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(pair)
}

// handleSetCustomKV handles PUT /api/customkv/{key} - set a key-value pair
func (s *Server) handleSetCustomKV(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")

	if key == "" {
		http.Error(w, "key parameter is required", http.StatusBadRequest)
		return
	}

	var req KVSetRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	// Use key from URL, ignore key in body if present
	req.Key = key

	// Check if key exists - we only allow modifying existing system parameters
	if !s.store.KV.ExistsRaw(req.Key) {
		http.Error(w, "key not found - only existing system parameters can be modified", http.StatusNotFound)
		return
	}

	if !hasValidSuffix(req.Key) {
		http.Error(w, "key must end with .str (for strings) or .int (for integers)", http.StatusBadRequest)
		return
	}

	// If it's an integer key, validate the value can be parsed as integer
	if hasIntSuffix(req.Key) {
		if !isValidInteger(req.Value) {
			http.Error(w, "value must be a valid integer for .int keys", http.StatusBadRequest)
			return
		}
	}

	err := s.store.KV.SetRaw(req.Key, req.Value)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	records, err := s.store.KV.GetAll()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var pair KVPair
	for _, record := range records {
		if record.Key == req.Key {
			pair = KVPair{
				Key:       record.Key,
				Value:     record.Value,
				UpdatedAt: record.UpdatedAt,
			}
			break
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(pair)
}

// Helper functions for validation
func hasValidSuffix(key string) bool {
	return hasStrSuffix(key) || hasIntSuffix(key)
}

func hasStrSuffix(key string) bool {
	return len(key) > 4 && key[len(key)-4:] == ".str"
}

func hasIntSuffix(key string) bool {
	return len(key) > 4 && key[len(key)-4:] == ".int"
}

func isValidInteger(value string) bool {
	if value == "" {
		return false
	}
	// Check if it's a valid integer (simple check)
	for i, r := range value {
		if i == 0 && r == '-' {
			continue // Allow negative numbers
		}
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
