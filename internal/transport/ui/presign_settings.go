package ui

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
)

type presignSettingsRequest struct {
	AccessURLExpirationHours int `json:"access_url_expiration_hours"`
}

func (h *Handler) handleAPIPresignSettings(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	w.Header().Set("Cache-Control", "no-store")
	if h.presignSettings == nil {
		httputil.WriteJSONError(w, http.StatusServiceUnavailable, "temporary-link settings are unavailable")
		return
	}
	if r.Method == http.MethodGet {
		h.writePresignSettings(w, r, http.StatusOK)
		return
	}
	if !httputil.RequireMethod(w, r, http.MethodPost) {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var request presignSettingsRequest
	if err := decoder.Decode(&request); err != nil {
		httputil.WriteJSONError(w, http.StatusBadRequest, "invalid temporary-link settings request")
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		httputil.WriteJSONError(w, http.StatusBadRequest, "invalid temporary-link settings request")
		return
	}
	if request.AccessURLExpirationHours < 1 || request.AccessURLExpirationHours > int(config.MaxAccessPresignTTL/time.Hour) {
		httputil.WriteJSONError(w, http.StatusBadRequest, "access_url_expiration_hours must be between 1 and 168")
		return
	}

	previous, _, err := h.presignSettings.AccessPresignTTL(r.Context())
	if err != nil {
		h.log.ErrorContext(r.Context(), "failed to read temporary-link settings", "error", err)
		httputil.WriteJSONError(w, http.StatusServiceUnavailable, "failed to read temporary-link settings")
		return
	}
	next := time.Duration(request.AccessURLExpirationHours) * time.Hour
	if err := h.presignSettings.SetAccessPresignTTL(r.Context(), next); err != nil {
		h.log.ErrorContext(r.Context(), "failed to update temporary-link settings", "error", err)
		httputil.WriteJSONError(w, http.StatusServiceUnavailable, "failed to update temporary-link settings")
		return
	}
	audit.Log(r.Context(), "presign.access_ttl.changed",
		slog.Int("previous_hours", int(previous/time.Hour)),
		slog.Int("access_url_expiration_hours", request.AccessURLExpirationHours))
	h.writePresignSettings(w, r, http.StatusOK)
}

func (h *Handler) writePresignSettings(w http.ResponseWriter, r *http.Request, status int) {
	ttl, overridden, err := h.presignSettings.AccessPresignTTL(r.Context())
	if err != nil {
		h.log.ErrorContext(r.Context(), "failed to load temporary-link settings", "error", err)
		httputil.WriteJSONError(w, http.StatusServiceUnavailable, "failed to load temporary-link settings")
		return
	}
	source := "configuration"
	if overridden {
		source = "management"
	}
	httputil.WriteJSON(w, status, map[string]any{
		"access_url_expiration_hours": int(ttl / time.Hour),
		"default_hours":               int(config.DefaultAccessPresignTTL / time.Hour),
		"maximum_hours":               int(config.MaxAccessPresignTTL / time.Hour),
		"source":                      source,
	})
}
