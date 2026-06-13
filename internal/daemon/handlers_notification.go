package daemon

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/hypersequent/oddk/internal/operations"
	"github.com/hypersequent/oddk/internal/store/notifications"
)

func (s *Server) handleNotificationAdd(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name   string          `json:"name"`
		Type   string          `json:"type"`
		Config json.RawMessage `json:"config"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	if req.Name == "" {
		s.writeError(w, http.StatusBadRequest, "Name is required")
		return
	}

	if err := notifications.ValidateNotificationType(req.Type); err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	params := operations.NotificationAddParams{
		Name:   req.Name,
		Type:   notifications.NotificationType(req.Type),
		Config: req.Config,
	}

	result, err := operations.NotificationAdd(r.Context(), s.opDeps, params)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.writeJSON(w, http.StatusCreated, result)
}

func (s *Server) handleNotificationList(w http.ResponseWriter, r *http.Request) {
	params := operations.NotificationListParams{}

	result, err := operations.NotificationList(r.Context(), s.opDeps, params)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleNotificationRemove(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		s.writeError(w, http.StatusBadRequest, "Name is required")
		return
	}

	params := operations.NotificationRemoveParams{
		Name: name,
	}

	result, err := operations.NotificationRemove(r.Context(), s.opDeps, params)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleNotificationTest(w http.ResponseWriter, r *http.Request) {
	params := operations.NotificationTestParams{}

	result, err := operations.NotificationTest(r.Context(), s.opDeps, params)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleNotificationLogs(w http.ResponseWriter, r *http.Request) {
	limitStr := r.URL.Query().Get("limit")
	limit := 100
	if limitStr != "" {
		if l, err := strconv.Atoi(limitStr); err == nil && l > 0 {
			limit = l
		}
	}

	params := operations.NotificationLogsParams{
		Limit: limit,
	}

	result, err := operations.NotificationLogs(r.Context(), s.opDeps, params)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleNotificationGet(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		s.writeError(w, http.StatusBadRequest, "Name is required")
		return
	}

	params := operations.NotificationGetParams{
		Name: name,
	}

	result, err := operations.NotificationGet(r.Context(), s.opDeps, params)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleNotificationEdit(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		s.writeError(w, http.StatusBadRequest, "Name is required")
		return
	}

	var req struct {
		Type   string          `json:"type"`
		Config json.RawMessage `json:"config"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	params := operations.NotificationEditParams{
		Name:   name,
		Type:   req.Type,
		Config: req.Config,
	}

	result, err := operations.NotificationEdit(r.Context(), s.opDeps, params)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.writeJSON(w, http.StatusOK, result)
}
