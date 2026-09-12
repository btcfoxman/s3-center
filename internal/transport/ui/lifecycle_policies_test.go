package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

type memoryLifecyclePolicyStore struct {
	mu       sync.Mutex
	nextID   int64
	policies []core.LifecyclePolicy
}

func (s *memoryLifecyclePolicyStore) ListLifecyclePolicies(context.Context) ([]core.LifecyclePolicy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.policies), nil
}

func (s *memoryLifecyclePolicyStore) CreateLifecyclePolicy(_ context.Context, prefix string, hours int) (*core.LifecyclePolicy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, policy := range s.policies {
		if policy.Prefix == prefix {
			return nil, core.ErrLifecyclePolicyConflict
		}
	}
	s.nextID++
	now := time.Now().UTC()
	policy := core.LifecyclePolicy{ID: s.nextID, Prefix: prefix, ExpirationHours: hours, Enabled: true, CreatedAt: now, UpdatedAt: now}
	s.policies = append(s.policies, policy)
	return &policy, nil
}

func (s *memoryLifecyclePolicyStore) SetLifecyclePolicyEnabled(_ context.Context, id int64, enabled bool) (*core.LifecyclePolicy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.policies {
		if s.policies[i].ID == id {
			s.policies[i].Enabled = enabled
			s.policies[i].UpdatedAt = time.Now().UTC()
			policy := s.policies[i]
			return &policy, nil
		}
	}
	return nil, core.ErrLifecyclePolicyNotFound
}

func (s *memoryLifecyclePolicyStore) DeleteLifecyclePolicy(_ context.Context, id int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.policies {
		if s.policies[i].ID == id {
			s.policies = append(s.policies[:i], s.policies[i+1:]...)
			return true, nil
		}
	}
	return false, nil
}

func policyRequest(t *testing.T, h *Handler, mux *http.ServeMux, body any) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := authedRequest(t, h, mux, http.MethodPost, "/ui/api/lifecycle-policies", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

func TestLifecyclePolicyAPI_CreatePauseResumeDelete(t *testing.T) {
	t.Parallel()
	h, mux := newTestHandler(t)
	store := &memoryLifecyclePolicyStore{}
	h.lifecyclePolicies = store

	w := policyRequest(t, h, mux, map[string]any{
		"action": "create", "bucket": "test-bucket", "directory": "tmp/render", "expiration_hours": 18, "acknowledged": true,
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", w.Code, w.Body.String())
	}
	policies, _ := store.ListLifecyclePolicies(t.Context())
	if len(policies) != 1 || policies[0].Prefix != "test-bucket/tmp/render/" || policies[0].ExpirationHours != 18 || !policies[0].Enabled {
		t.Fatalf("policies = %+v", policies)
	}

	w = policyRequest(t, h, mux, map[string]any{"action": "set_enabled", "id": policies[0].ID, "enabled": false})
	if w.Code != http.StatusOK {
		t.Fatalf("pause status=%d body=%s", w.Code, w.Body.String())
	}
	w = policyRequest(t, h, mux, map[string]any{"action": "set_enabled", "id": policies[0].ID, "enabled": true})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unacknowledged resume status=%d, want 400", w.Code)
	}
	w = policyRequest(t, h, mux, map[string]any{"action": "set_enabled", "id": policies[0].ID, "enabled": true, "acknowledged": true})
	if w.Code != http.StatusOK {
		t.Fatalf("resume status=%d body=%s", w.Code, w.Body.String())
	}
	w = policyRequest(t, h, mux, map[string]any{"action": "delete", "id": policies[0].ID})
	if w.Code != http.StatusOK {
		t.Fatalf("delete status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestLifecyclePolicyAPI_ValidatesDestructiveScope(t *testing.T) {
	t.Parallel()
	h, mux := newTestHandler(t)
	h.lifecyclePolicies = &memoryLifecyclePolicyStore{}

	cases := []map[string]any{
		{"action": "create", "bucket": "missing", "expiration_hours": 24, "acknowledged": true},
		{"action": "create", "bucket": "test-bucket", "directory": "../escape", "expiration_hours": 24, "acknowledged": true},
		{"action": "create", "bucket": "test-bucket", "expiration_hours": 0, "acknowledged": true},
		{"action": "create", "bucket": "test-bucket", "expiration_hours": 24, "acknowledged": false},
	}
	for _, body := range cases {
		w := policyRequest(t, h, mux, body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("body=%v status=%d response=%s", body, w.Code, w.Body.String())
		}
	}
}

func TestLifecyclePolicyAPI_ConflictAndNotFound(t *testing.T) {
	t.Parallel()
	h, mux := newTestHandler(t)
	store := &memoryLifecyclePolicyStore{}
	h.lifecyclePolicies = store
	body := map[string]any{"action": "create", "bucket": "test-bucket", "expiration_hours": 24, "acknowledged": true}
	if w := policyRequest(t, h, mux, body); w.Code != http.StatusCreated {
		t.Fatalf("first create status=%d", w.Code)
	}
	if w := policyRequest(t, h, mux, body); w.Code != http.StatusConflict {
		t.Fatalf("duplicate create status=%d, want 409", w.Code)
	}
	if w := policyRequest(t, h, mux, map[string]any{"action": "set_enabled", "id": 999, "enabled": false}); w.Code != http.StatusNotFound {
		t.Fatalf("missing update status=%d, want 404", w.Code)
	}
	if w := policyRequest(t, h, mux, map[string]any{"action": "delete", "id": 999}); w.Code != http.StatusNotFound {
		t.Fatalf("missing delete status=%d, want 404", w.Code)
	}
}

func TestDashboardRendersLifecyclePolicyControls(t *testing.T) {
	t.Parallel()
	h, mux := newTestHandler(t)
	h.lifecyclePolicies = &memoryLifecyclePolicyStore{nextID: 7, policies: []core.LifecyclePolicy{{
		ID: 7, Prefix: "test-bucket/cache/", ExpirationHours: 48, Enabled: true,
	}}}

	req := authedRequest(t, h, mux, http.MethodGet, "/ui/", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("dashboard status=%d", w.Code)
	}
	for _, marker := range []string{"RETENTION AUTOMATION", "test-bucket/cache/", "48", "lifecycle-policy-form", "static/lifecycle.js"} {
		if !bytes.Contains(w.Body.Bytes(), []byte(marker)) {
			t.Errorf("dashboard missing %q", marker)
		}
	}
}

var _ core.LifecyclePolicyStore = (*memoryLifecyclePolicyStore)(nil)
