package daemon

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/hypersequent/oddk/internal/operr"
)

// ErrorResponse represents an API error response
type ErrorResponse struct {
	Error string `json:"error"`
}

// writeJSON writes a JSON response with the given status code
func (s *Server) writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(data); err != nil {
		log.Printf("Error encoding JSON response: %v", err)
	}
}

// writeError writes an error response with the given status code and message
func (s *Server) writeError(w http.ResponseWriter, status int, message string) {
	s.writeJSON(w, status, ErrorResponse{Error: message})
}

// writeOpError writes an error response with the status code derived from the
// error's operr marker (see internal/operr); unmarked errors are 500s.
func (s *Server) writeOpError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, operr.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, operr.ErrConflict):
		status = http.StatusConflict
	case errors.Is(err, operr.ErrInvalid):
		status = http.StatusBadRequest
	case errors.Is(err, operr.ErrForbidden):
		status = http.StatusForbidden
	}
	s.writeError(w, status, err.Error())
}

// withAuth is middleware that validates the authorization token
func (s *Server) withAuth(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			s.writeError(w, http.StatusUnauthorized, "missing authorization header")
			return
		}

		parts := strings.Split(authHeader, " ")
		if len(parts) != 2 || parts[0] != "Bearer" {
			s.writeError(w, http.StatusUnauthorized, "invalid authorization header format")
			return
		}

		token := parts[1]
		valid, err := s.store.Auth.ValidateToken(token)
		if err != nil {
			log.Printf("Error validating token: %v", err)
			s.writeError(w, http.StatusInternalServerError, "error validating token")
			return
		}

		if !valid {
			s.writeError(w, http.StatusUnauthorized, "invalid token")
			return
		}

		handler(w, r)
	}
}

// extractPathParam safely extracts a path parameter and returns bad request if missing
func (s *Server) extractPathParam(w http.ResponseWriter, r *http.Request, param string) (string, bool) {
	value := r.PathValue(param)
	if value == "" {
		s.writeError(w, http.StatusBadRequest, "missing "+param+" parameter")
		return "", false
	}
	return value, true
}
