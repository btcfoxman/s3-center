package ui

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"unicode"

	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
)

const maxLifecycleExpirationHours = 10 * 365 * 24

type lifecyclePolicyRequest struct {
	Action          string `json:"action"`
	ID              int64  `json:"id"`
	Bucket          string `json:"bucket"`
	Directory       string `json:"directory"`
	ExpirationHours int    `json:"expiration_hours"`
	Enabled         bool   `json:"enabled"`
	Acknowledged    bool   `json:"acknowledged"`
}

func (h *Handler) handleAPILifecyclePolicies(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	if h.lifecyclePolicies == nil {
		httputil.WriteJSONError(w, http.StatusServiceUnavailable, "lifecycle policy storage is unavailable")
		return
	}
	if r.Method == http.MethodGet {
		h.listLifecyclePolicies(w, r)
		return
	}
	if !httputil.RequireMethod(w, r, http.MethodPost) {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var request lifecyclePolicyRequest
	if err := decoder.Decode(&request); err != nil {
		httputil.WriteJSONError(w, http.StatusBadRequest, "invalid lifecycle policy request")
		return
	}

	switch request.Action {
	case "create":
		h.createLifecyclePolicy(w, r, &request)
	case "set_enabled":
		h.setLifecyclePolicyEnabled(w, r, &request)
	case "delete":
		h.deleteLifecyclePolicy(w, r, request.ID)
	default:
		httputil.WriteJSONError(w, http.StatusBadRequest, "unsupported lifecycle policy action")
	}
}

func (h *Handler) listLifecyclePolicies(w http.ResponseWriter, r *http.Request) {
	policies, err := h.lifecyclePolicies.ListLifecyclePolicies(r.Context())
	if err != nil {
		h.log.ErrorContext(r.Context(), "failed to list lifecycle policies", "error", err)
		httputil.WriteJSONError(w, http.StatusServiceUnavailable, "failed to load lifecycle policies")
		return
	}
	httputil.WriteJSON(w, http.StatusOK, map[string]any{"policies": policies})
}

func (h *Handler) createLifecyclePolicy(w http.ResponseWriter, r *http.Request, request *lifecyclePolicyRequest) {
	if !request.Acknowledged {
		httputil.WriteJSONError(w, http.StatusBadRequest, "permanent deletion acknowledgement is required")
		return
	}
	if request.ExpirationHours < 1 || request.ExpirationHours > maxLifecycleExpirationHours {
		httputil.WriteJSONError(w, http.StatusBadRequest, "expiration_hours must be between 1 and 87600")
		return
	}
	prefix, err := h.lifecyclePrefix(request.Bucket, request.Directory)
	if err != nil {
		httputil.WriteJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if h.hasStaticLifecyclePrefix(prefix) {
		httputil.WriteJSONError(w, http.StatusConflict, "this prefix is already managed by config.yaml")
		return
	}

	policy, err := h.lifecyclePolicies.CreateLifecyclePolicy(r.Context(), prefix, request.ExpirationHours)
	if err != nil {
		status := http.StatusUnprocessableEntity
		if errors.Is(err, core.ErrLifecyclePolicyConflict) {
			status = http.StatusConflict
		}
		h.log.WarnContext(r.Context(), "lifecycle policy creation failed", "prefix", prefix, "error", err)
		httputil.WriteJSONError(w, status, err.Error())
		return
	}
	audit.Log(r.Context(), "lifecycle.policy.created",
		slog.Int64("policy_id", policy.ID), slog.String("prefix", policy.Prefix), slog.Int("retention_hours", policy.ExpirationHours))
	httputil.WriteJSON(w, http.StatusCreated, map[string]any{"ok": true, "policy": policy})
}

func (h *Handler) setLifecyclePolicyEnabled(w http.ResponseWriter, r *http.Request, request *lifecyclePolicyRequest) {
	if request.ID <= 0 {
		httputil.WriteJSONError(w, http.StatusBadRequest, "policy id is required")
		return
	}
	if request.Enabled && !request.Acknowledged {
		httputil.WriteJSONError(w, http.StatusBadRequest, "permanent deletion acknowledgement is required when enabling a policy")
		return
	}
	policy, err := h.lifecyclePolicies.SetLifecyclePolicyEnabled(r.Context(), request.ID, request.Enabled)
	if err != nil {
		status := http.StatusUnprocessableEntity
		if errors.Is(err, core.ErrLifecyclePolicyNotFound) {
			status = http.StatusNotFound
		}
		httputil.WriteJSONError(w, status, err.Error())
		return
	}
	audit.Log(r.Context(), "lifecycle.policy.state_changed",
		slog.Int64("policy_id", policy.ID), slog.String("prefix", policy.Prefix), slog.Bool("enabled", policy.Enabled))
	httputil.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "policy": policy})
}

func (h *Handler) deleteLifecyclePolicy(w http.ResponseWriter, r *http.Request, id int64) {
	if id <= 0 {
		httputil.WriteJSONError(w, http.StatusBadRequest, "policy id is required")
		return
	}
	deleted, err := h.lifecyclePolicies.DeleteLifecyclePolicy(r.Context(), id)
	if err != nil {
		httputil.WriteJSONError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if !deleted {
		httputil.WriteJSONError(w, http.StatusNotFound, core.ErrLifecyclePolicyNotFound.Error())
		return
	}
	audit.Log(r.Context(), "lifecycle.policy.deleted", slog.Int64("policy_id", id))
	httputil.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) lifecyclePrefix(bucket, directory string) (string, error) {
	bucket = strings.TrimSpace(bucket)
	if !h.configuredBucket(bucket) {
		return "", errors.New("unknown virtual bucket")
	}
	directory = strings.Trim(strings.TrimSpace(directory), "/")
	if directory == "" {
		return bucket + "/", nil
	}
	if len(directory) > 900 || strings.Contains(directory, `\`) {
		return "", errors.New("directory prefix is invalid")
	}
	for _, segment := range strings.Split(directory, "/") {
		if segment == "" || segment == "." || segment == ".." || strings.IndexFunc(segment, unicode.IsControl) >= 0 {
			return "", errors.New("directory prefix contains an invalid path segment")
		}
	}
	return bucket + "/" + directory + "/", nil
}

func (h *Handler) configuredBucket(name string) bool {
	cfg := h.cfg.Load()
	for _, bucket := range cfg.Buckets {
		if bucket.Name == name {
			return true
		}
	}
	return false
}

func (h *Handler) hasStaticLifecyclePrefix(prefix string) bool {
	cfg := h.cfg.Load()
	for _, rule := range cfg.Lifecycle.Rules {
		if rule.Prefix == prefix {
			return true
		}
	}
	return false
}
