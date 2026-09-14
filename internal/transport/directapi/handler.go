// Package directapi owns the authenticated /v1/uploads REST surface.
package directapi

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/directupload"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

//go:embed console.html
var consoleHTML string

type token struct {
	hash     [32]byte
	scopes   map[string]struct{}
	buckets  map[string]struct{}
	prefixes []string
}
type Handler struct {
	manager     *directupload.Manager
	deleter     ObjectDeleter
	tokens      []token
	consoleHost string
}

// ObjectDeleter performs a metadata-coordinated upstream delete. DELETE has no
// object response body, so it remains a control-plane operation in index-only
// mode while preserving quota and index consistency.
type ObjectDeleter interface {
	DeleteObject(context.Context, string) error
}

//nolint:gocritic // The handler owns an immutable configuration snapshot.
func New(manager *directupload.Manager, cfg config.DirectUploadConfig, deleters ...ObjectDeleter) *Handler {
	h := &Handler{manager: manager, consoleHost: cfg.ConsoleHost}
	if len(deleters) > 0 {
		h.deleter = deleters[0]
	}
	for _, v := range cfg.Tokens {
		t := token{
			hash: sha256.Sum256([]byte(v.Token)), scopes: map[string]struct{}{},
			buckets: map[string]struct{}{}, prefixes: append([]string(nil), v.Prefixes...),
		}
		for _, s := range v.Scopes {
			t.scopes[s] = struct{}{}
		}
		for _, bucket := range v.Buckets {
			t.buckets[bucket] = struct{}{}
		}
		h.tokens = append(h.tokens, t)
	}
	return h
}
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("/v1/uploads", h.collection)
	mux.HandleFunc("/v1/uploads/", h.item)
	mux.HandleFunc("/v1/objects", h.objects)
	mux.HandleFunc("/v1/objects/presign", h.presignObject)
	mux.HandleFunc("/v1/objects/delete", h.deleteObject)
	mux.HandleFunc("/v1/multipart", h.multipartCollection)
	mux.HandleFunc("/v1/multipart/", h.multipartItem)
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

func (h *Handler) authenticate(w http.ResponseWriter, r *http.Request, scope string) *token {
	raw := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	sum := sha256.Sum256([]byte(raw))
	var matched *token
	for i := range h.tokens {
		t := &h.tokens[i]
		_, ok := t.scopes[scope]
		if subtle.ConstantTimeCompare(sum[:], t.hash[:]) == 1 && ok {
			matched = t
		}
	}
	if raw == "" || matched == nil {
		w.Header().Set("WWW-Authenticate", `Bearer realm="s3-center"`)
		writeError(w, http.StatusUnauthorized, "unauthorized", "有效的 Bearer Token 和权限是必需的")
		return nil
	}
	return matched
}

func (t *token) allows(bucket, key string) bool {
	if len(t.buckets) > 0 {
		if _, ok := t.buckets[bucket]; !ok {
			return false
		}
	}
	if len(t.prefixes) == 0 {
		return true
	}
	for _, prefix := range t.prefixes {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

func authorizeResource(w http.ResponseWriter, t *token, bucket, key string) bool {
	if t != nil && t.allows(bucket, key) {
		return true
	}
	writeError(w, http.StatusForbidden, "resource_forbidden", "token is not authorized for this bucket or prefix")
	return false
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
	var in createRequest
	if !decodeRequest(w, r, &in) {
		return
	}
	t := h.authenticate(w, r, "uploads:write")
	if t == nil || !authorizeResource(w, t, in.Bucket, strings.TrimPrefix(in.Key, "/")) {
		return
	}
	res, err := h.manager.Create(r.Context(), directupload.CreateRequest{Bucket: in.Bucket, Key: in.Key, Size: in.Size, ContentType: in.ContentType, IdempotencyKey: r.Header.Get("Idempotency-Key")})
	if err != nil {
		writeManagerError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": res.Session.ID, "status": res.Session.Status, "declared_bytes": res.Session.DeclaredBytes, "expires_at": res.Session.ExpiresAt, "upload": map[string]any{"method": res.Upload.Method, "url": res.Upload.URL, "headers": res.Upload.Headers, "expires_at": res.Upload.ExpiresAt}})
}

//nolint:gocognit // The switch is the explicit method/action authorization matrix for this resource.
func (h *Handler) item(w http.ResponseWriter, r *http.Request) {
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/uploads/"), "/")
	parts := strings.Split(path, "/")
	if len(parts) == 0 || parts[0] == "" {
		writeError(w, 400, "invalid_request", "缺少上传会话 ID")
		return
	}
	if len(parts) > 2 {
		writeError(w, http.StatusNotFound, "not_found", "upload session route does not exist")
		return
	}
	id := parts[0]
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}
	switch {
	case r.Method == http.MethodGet && action == "":
		t := h.authenticate(w, r, "uploads:read")
		if t == nil {
			return
		}
		u, err := h.manager.Get(r.Context(), id)
		if err != nil {
			writeManagerError(w, err)
			return
		}
		if !authorizeUploadSession(w, t, u) {
			return
		}
		writeJSON(w, http.StatusOK, uploadSessionResponse(u))
	case r.Method == http.MethodPost && action == "complete":
		t := h.authenticate(w, r, "uploads:write")
		if t == nil {
			return
		}
		u, err := h.manager.Get(r.Context(), id)
		if err != nil {
			writeManagerError(w, err)
			return
		}
		if !authorizeUploadSession(w, t, u) {
			return
		}
		u, err = h.manager.Complete(r.Context(), id)
		if err != nil {
			writeManagerError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, uploadSessionResponse(u))
	case r.Method == http.MethodDelete && action == "":
		t := h.authenticate(w, r, "uploads:write")
		if t == nil {
			return
		}
		u, err := h.manager.Get(r.Context(), id)
		if err != nil {
			writeManagerError(w, err)
			return
		}
		if !authorizeUploadSession(w, t, u) {
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

func authorizeUploadSession(w http.ResponseWriter, t *token, session *core.UploadSession) bool {
	bucket, key, ok := strings.Cut(session.LogicalKey, "/")
	if !ok {
		writeError(w, http.StatusInternalServerError, "invalid_session", "upload session has an invalid logical key")
		return false
	}
	return authorizeResource(w, t, bucket, key)
}

type objectRequest struct {
	Bucket         string `json:"bucket"`
	Key            string `json:"key"`
	Method         string `json:"method"`
	ExpiresSeconds int64  `json:"expires_seconds"`
}

func (h *Handler) objects(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "only GET is supported")
		return
	}
	t := h.authenticate(w, r, "objects:list")
	if t == nil {
		return
	}
	bucket := strings.TrimSpace(r.URL.Query().Get("bucket"))
	prefix := strings.TrimPrefix(strings.TrimSpace(r.URL.Query().Get("prefix")), "/")
	if !authorizeResource(w, t, bucket, prefix) {
		return
	}
	maxKeys := 1000
	if raw := r.URL.Query().Get("max_keys"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 1000 {
			writeError(w, http.StatusBadRequest, "invalid_request", "max_keys must be between 1 and 1000")
			return
		}
		maxKeys = parsed
	}
	result, err := h.manager.ListIndex(r.Context(), bucket, prefix, r.URL.Query().Get("start_after"), maxKeys)
	if err != nil {
		writeManagerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"bucket": bucket, "prefix": prefix, "objects": result.Objects,
		"is_truncated": result.IsTruncated, "next_start_after": result.NextStartKey,
	})
}

func (h *Handler) presignObject(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "only POST is supported")
		return
	}
	var in objectRequest
	if !decodeRequest(w, r, &in) {
		return
	}
	t := h.authenticate(w, r, "objects:read")
	key := strings.TrimPrefix(strings.TrimSpace(in.Key), "/")
	if t == nil || !authorizeResource(w, t, in.Bucket, key) {
		return
	}
	if in.ExpiresSeconds < 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "expires_seconds must not be negative")
		return
	}
	if in.ExpiresSeconds > int64(config.MaxAccessPresignTTL/time.Second) {
		writeError(w, http.StatusBadRequest, "invalid_request", "expires_seconds must not exceed 604800")
		return
	}
	result, err := h.manager.PresignAccess(r.Context(), &directupload.AccessRequest{
		Bucket: in.Bucket, Key: key, Method: in.Method,
		Expires: time.Duration(in.ExpiresSeconds) * time.Second,
	})
	if err != nil {
		writeManagerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"bucket": in.Bucket, "key": key, "size_bytes": result.SizeBytes,
		"request": map[string]any{
			"method": result.Request.Method, "url": result.Request.URL,
			"headers": result.Request.Headers, "expires_at": result.Request.ExpiresAt,
		},
	})
}

func (h *Handler) deleteObject(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "only POST is supported")
		return
	}
	var in objectRequest
	if !decodeRequest(w, r, &in) {
		return
	}
	t := h.authenticate(w, r, "objects:delete")
	key := strings.TrimPrefix(strings.TrimSpace(in.Key), "/")
	if t == nil || !authorizeResource(w, t, in.Bucket, key) {
		return
	}
	logicalKey, err := h.manager.LogicalObjectKey(in.Bucket, key)
	if err != nil {
		writeManagerError(w, err)
		return
	}
	if h.deleter == nil {
		writeError(w, http.StatusNotImplemented, "delete_unavailable", "metadata-coordinated delete is not configured")
		return
	}
	if err := h.deleter.DeleteObject(r.Context(), logicalKey); err != nil {
		writeManagerError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) multipartCollection(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "only POST is supported")
		return
	}
	var in createRequest
	if !decodeRequest(w, r, &in) {
		return
	}
	t := h.authenticate(w, r, "uploads:write")
	key := strings.TrimPrefix(strings.TrimSpace(in.Key), "/")
	if t == nil || !authorizeResource(w, t, in.Bucket, key) {
		return
	}
	result, err := h.manager.CreateMultipart(r.Context(), directupload.CreateRequest{
		Bucket: in.Bucket, Key: key, Size: in.Size, ContentType: in.ContentType,
		IdempotencyKey: r.Header.Get("Idempotency-Key"),
	})
	if err != nil {
		writeManagerError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, uploadSessionResponse(result.Session))
}

type multipartPartRequest struct {
	PartNumber int `json:"part_number"`
}

type multipartCompleteRequest struct {
	Parts []backend.CompletedPart `json:"parts"`
}

//nolint:gocognit // The explicit action matrix keeps multipart authorization auditable.
func (h *Handler) multipartItem(w http.ResponseWriter, r *http.Request) {
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/multipart/"), "/")
	parts := strings.Split(path, "/")
	if len(parts) < 2 || parts[0] == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "multipart session id and action are required")
		return
	}
	id, action := parts[0], strings.Join(parts[1:], "/")
	scope := "uploads:write"
	t := h.authenticate(w, r, scope)
	if t == nil {
		return
	}
	session, err := h.manager.Get(r.Context(), id)
	if err != nil {
		writeManagerError(w, err)
		return
	}
	bucket, key, ok := strings.Cut(session.LogicalKey, "/")
	if !ok || !authorizeResource(w, t, bucket, key) {
		return
	}
	switch {
	case r.Method == http.MethodPost && action == "parts/presign":
		var in multipartPartRequest
		if !decodeRequest(w, r, &in) {
			return
		}
		request, err := h.manager.PresignMultipartPart(r.Context(), id, in.PartNumber)
		if err != nil {
			writeManagerError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"part_number": in.PartNumber, "request": request})
	case r.Method == http.MethodPost && action == "complete":
		var in multipartCompleteRequest
		if !decodeRequest(w, r, &in) {
			return
		}
		completed, err := h.manager.CompleteMultipart(r.Context(), id, in.Parts)
		if err != nil {
			writeManagerError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, uploadSessionResponse(completed))
	case r.Method == http.MethodDelete && action == "abort":
		if err := h.manager.Abort(r.Context(), id); err != nil {
			writeManagerError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "multipart action is not supported")
	}
}

func uploadSessionResponse(session *core.UploadSession) map[string]any {
	return map[string]any{
		"id": session.ID, "status": session.Status,
		"logical_key": session.LogicalKey, "declared_bytes": session.DeclaredBytes,
		"expires_at": session.ExpiresAt,
	}
}

func decodeRequest(w http.ResponseWriter, r *http.Request, out any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid_request", "request body must contain exactly one JSON object")
		return false
	}
	return true
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
	case errors.Is(err, core.ErrObjectNotFound):
		status, code = http.StatusNotFound, "object_not_found"
	case errors.Is(err, directupload.ErrDirectAccessUnsupported):
		status, code = http.StatusConflict, "direct_access_unsupported"
	case errors.Is(err, directupload.ErrAccessSettingsUnavailable):
		status, code = http.StatusServiceUnavailable, "access_settings_unavailable"
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
