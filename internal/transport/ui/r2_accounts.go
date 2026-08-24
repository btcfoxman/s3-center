package ui

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/afreidah/s3-orchestrator/internal/backendmanage"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
)

func (h *Handler) handleAPIR2Accounts(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	if h.backendManagement == nil || !h.backendManagement.Enabled() {
		httputil.WriteJSONError(w, http.StatusServiceUnavailable, "dynamic R2 account management is disabled")
		return
	}
	switch r.Method {
	case http.MethodGet:
		httputil.WriteJSON(w, http.StatusOK, map[string]any{"accounts": h.backendManagement.List()})
	case http.MethodPost:
		r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		var request backendmanage.CreateRequest
		if err := decoder.Decode(&request); err != nil {
			httputil.WriteJSONError(w, http.StatusBadRequest, "invalid account definition")
			return
		}
		view, err := h.backendManagement.Create(r.Context(), request)
		if err != nil {
			status := http.StatusUnprocessableEntity
			if strings.Contains(err.Error(), "already exists") {
				status = http.StatusConflict
			}
			h.log.WarnContext(r.Context(), "R2 account onboarding failed", "backend", request.Name, "error", err)
			httputil.WriteJSONError(w, status, err.Error())
			return
		}
		h.log.InfoContext(r.Context(), "R2 account added", "backend", view.Name, "bucket", view.Bucket, "proxy", view.Proxy)
		httputil.WriteJSON(w, http.StatusCreated, map[string]any{"ok": true, "account": view})
	default:
		w.Header().Set("Allow", "GET, POST")
		httputil.WriteJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}
