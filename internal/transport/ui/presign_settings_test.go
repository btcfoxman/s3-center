package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
)

type memoryPresignSettings struct {
	mu         sync.Mutex
	ttl        time.Duration
	overridden bool
	err        error
}

func (s *memoryPresignSettings) AccessPresignTTL(context.Context) (time.Duration, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return 0, false, s.err
	}
	if s.ttl == 0 {
		return config.DefaultAccessPresignTTL, false, nil
	}
	return s.ttl, s.overridden, nil
}

func (s *memoryPresignSettings) SetAccessPresignTTL(_ context.Context, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.ttl = ttl
	s.overridden = true
	return nil
}

func doPresignSettingsRequest(t *testing.T, h *Handler, mux *http.ServeMux, method, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := authedRequest(t, h, mux, method, "/ui/api/presign-settings", strings.NewReader(body))
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

func TestPresignSettingsAPIReadsDefaultAndPersistsTwelveHours(t *testing.T) {
	t.Parallel()
	h, mux := newTestHandler(t)
	settings := &memoryPresignSettings{}
	h.presignSettings = settings

	w := doPresignSettingsRequest(t, h, mux, http.MethodGet, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET status=%d body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	var initial map[string]any
	if err := json.NewDecoder(w.Body).Decode(&initial); err != nil {
		t.Fatal(err)
	}
	if initial["access_url_expiration_hours"] != float64(3) || initial["default_hours"] != float64(3) || initial["maximum_hours"] != float64(168) || initial["source"] != "configuration" {
		t.Fatalf("initial response = %#v", initial)
	}

	w = doPresignSettingsRequest(t, h, mux, http.MethodPost, `{"access_url_expiration_hours":12}`)
	if w.Code != http.StatusOK {
		t.Fatalf("POST status=%d body=%s", w.Code, w.Body.String())
	}
	var updated map[string]any
	if err := json.NewDecoder(w.Body).Decode(&updated); err != nil {
		t.Fatal(err)
	}
	if updated["access_url_expiration_hours"] != float64(12) || updated["source"] != "management" {
		t.Fatalf("updated response = %#v", updated)
	}
	if ttl, overridden, err := settings.AccessPresignTTL(t.Context()); err != nil || !overridden || ttl != 12*time.Hour {
		t.Fatalf("stored settings = (%v, %v, %v)", ttl, overridden, err)
	}
}

func TestPresignSettingsAPIValidatesPayloadAndAvailability(t *testing.T) {
	t.Parallel()
	h, mux := newTestHandler(t)
	h.presignSettings = &memoryPresignSettings{}

	for _, body := range []string{
		`{"access_url_expiration_hours":0}`,
		`{"access_url_expiration_hours":169}`,
		`{"access_url_expiration_hours":12,"unknown":true}`,
		`{"access_url_expiration_hours":12}{"access_url_expiration_hours":3}`,
	} {
		w := doPresignSettingsRequest(t, h, mux, http.MethodPost, body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("body=%s status=%d response=%s", body, w.Code, w.Body.String())
		}
	}

	h.presignSettings = nil
	w := doPresignSettingsRequest(t, h, mux, http.MethodGet, "")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("unavailable status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestDashboardRendersTemporaryLinkSettings(t *testing.T) {
	t.Parallel()
	h, mux := newTestHandler(t)
	h.presignSettings = &memoryPresignSettings{ttl: 12 * time.Hour}
	cfg := h.cfg.Load()
	cfg.DirectUpload.Enabled = true
	h.UpdateConfig(cfg)

	req := authedRequest(t, h, mux, http.MethodGet, "/ui/", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("dashboard status=%d body=%s", w.Code, w.Body.String())
	}
	for _, marker := range [][]byte{
		[]byte(`id="presign-settings-dialog"`),
		[]byte(`id="presign-hours"`),
		[]byte("产品默认"),
		[]byte("static/presign-settings.js"),
	} {
		if !bytes.Contains(w.Body.Bytes(), marker) {
			t.Errorf("dashboard missing %q", marker)
		}
	}
}

var _ PresignSettingsOps = (*memoryPresignSettings)(nil)
