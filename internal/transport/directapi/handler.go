// Package directapi owns the authenticated /v1/uploads REST surface.
package directapi

import (
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/directupload"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

//go:embed console.html
var consoleHTML string

type token struct {
	hash   [32]byte
	scopes map[string]struct{}
}
type Handler struct {
	manager     *directupload.Manager
	tokens      []token
	consoleHost string
}

//nolint:gocritic // The handler owns an immutable configuration snapshot.
func New(manager *directupload.Manager, cfg config.DirectUploadConfig) *Handler {
	h := &Handler{manager: manager, consoleHost: cfg.ConsoleHost}
	for _, v := range cfg.Tokens {
		t := token{hash: sha256.Sum256([]byte(v.Token)), scopes: map[string]struct{}{}}
		for _, s := range v.Scopes {
			t.scopes[s] = struct{}{}
		}
		h.tokens = append(h.tokens, t)
	}
	return h
}
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("/v1/uploads", h.collection)
	mux.HandleFunc("/v1/uploads/", h.item)
	mux.HandleFunc("/direct/", h.console)
}

func (h *Handler) console(w http.ResponseWriter, r *http.Request) {
	if h.consoleHost != "" && !strings.EqualFold(strings.Split(r.Host, ":")[0], h.consoleHost) {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(consoleHTML))
}

func (h *Handler) authorize(w http.ResponseWriter, r *http.Request, scope string) bool {
	raw := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	sum := sha256.Sum256([]byte(raw))
	allowed := 0
	for _, t := range h.tokens {
		_, ok := t.scopes[scope]
		scopeOK := 0
		if ok {
			scopeOK = 1
		}
		allowed |= subtle.ConstantTimeCompare(sum[:], t.hash[:]) * scopeOK
	}
	if raw == "" || allowed != 1 {
		w.Header().Set("WWW-Authenticate", `Bearer realm="s3-center"`)
		writeError(w, http.StatusUnauthorized, "unauthorized", "有效的 Bearer Token 和权限是必需的")
		return false
	}
	return true
}

type createRequest struct {
	Bucket      string `json:"bucket"`
	Key         string `json:"key"`
	Size        int64  `json:"size"`
	ContentType string `json:"content_type"`
}

func (h *Handler) collection(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "仅支持 POST")
		return
	}
	if !h.authorize(w, r, "uploads:write") {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var in createRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	res, err := h.manager.Create(r.Context(), directupload.CreateRequest{Bucket: in.Bucket, Key: in.Key, Size: in.Size, ContentType: in.ContentType, IdempotencyKey: r.Header.Get("Idempotency-Key")})
	if err != nil {
		writeManagerError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": res.Session.ID, "status": res.Session.Status, "backend": res.Session.BackendName, "declared_bytes": res.Session.DeclaredBytes, "expires_at": res.Session.ExpiresAt, "upload": map[string]any{"method": res.Upload.Method, "url": res.Upload.URL, "headers": res.Upload.Headers, "expires_at": res.Upload.ExpiresAt}})
}

//nolint:gocognit // The switch is the explicit method/action authorization matrix for this resource.
func (h *Handler) item(w http.ResponseWriter, r *http.Request) {
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/uploads/"), "/")
	parts := strings.Split(path, "/")
	if len(parts) == 0 || parts[0] == "" {
		writeError(w, 400, "invalid_request", "缺少上传会话 ID")
		return
	}
	id := parts[0]
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}
	switch {
	case r.Method == http.MethodGet && action == "":
		if !h.authorize(w, r, "uploads:read") {
			return
		}
		u, err := h.manager.Get(r.Context(), id)
		if err != nil {
			writeManagerError(w, err)
			return
		}
		writeJSON(w, 200, u)
	case r.Method == http.MethodPost && action == "complete":
		if !h.authorize(w, r, "uploads:write") {
			return
		}
		u, err := h.manager.Complete(r.Context(), id)
		if err != nil {
			writeManagerError(w, err)
			return
		}
		writeJSON(w, 200, u)
	case r.Method == http.MethodDelete && action == "":
		if !h.authorize(w, r, "uploads:write") {
			return
		}
		if err := h.manager.Abort(r.Context(), id); err != nil {
			writeManagerError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "请求方法或操作不受支持")
	}
}

func writeManagerError(w http.ResponseWriter, err error) {
	status, code := http.StatusBadRequest, "invalid_request"
	switch {
	case errors.Is(err, core.ErrNoSpaceAvailable):
		status, code = http.StatusInsufficientStorage, "insufficient_capacity"
	case errors.Is(err, core.ErrUploadSessionNotFound):
		status, code = http.StatusNotFound, "session_not_found"
	case errors.Is(err, core.ErrUploadSessionExpired):
		status, code = http.StatusGone, "session_expired"
	case errors.Is(err, core.ErrUploadSessionConflict), errors.Is(err, core.ErrUploadSizeMismatch):
		status, code = http.StatusConflict, "session_conflict"
	}
	writeError(w, status, code, err.Error())
}
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		_ = fmt.Errorf("encode response: %w", err)
	}
}
