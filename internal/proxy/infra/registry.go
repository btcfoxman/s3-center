// -------------------------------------------------------------------------------
// Backend Registry - Lookup and Health Filtering
//
// Author: Alex Freidah
//
// Owns the static backend map + the configured iteration order, plus the
// dynamic drain checker that decides which backends are currently in
// service. Hides the circuit-breaker probe logic so write-eligibility
// callers do not have to know about the breaker implementation. Used
// internally by *BackendRuntime; consumers reach the same methods through BackendRuntime's
// public surface (Backends, BackendOrder, GetBackend, etc.).
// -------------------------------------------------------------------------------

package infra

import (
	"fmt"
	"slices"
	"sync"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/breaker"
)

// backendRegistry owns the per-process backend map, the configured
// iteration order, and the drain checker. drainMgr is set via
// SetDrainChecker once the drain manager exists, since it is built after
// the runtime.
type backendRegistry struct {
	mu          sync.RWMutex
	backends    map[string]backend.ObjectBackend
	order       []string
	writePaused map[string]bool
	drainMgr    DrainChecker
}

// newBackendRegistry constructs a registry from the backend map and
// iteration order. The drain checker is wired post-construction via
// SetDrainChecker.
func newBackendRegistry(backends map[string]backend.ObjectBackend, order []string, paused map[string]bool) *backendRegistry {
	writePaused := make(map[string]bool, len(paused))
	for name, isPaused := range paused {
		if isPaused {
			writePaused[name] = true
		}
	}
	return &backendRegistry{backends: backends, order: order, writePaused: writePaused}
}

// SetDrainChecker installs the drain manager after BackendRuntime has been
// constructed.
func (r *backendRegistry) SetDrainChecker(d DrainChecker) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.drainMgr = d
}

// All returns the backend map (read-only contract).
func (r *backendRegistry) All() map[string]backend.ObjectBackend {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make(map[string]backend.ObjectBackend, len(r.backends))
	for name, be := range r.backends {
		result[name] = be
	}
	return result
}

// Order returns the configured backend iteration order.
func (r *backendRegistry) Order() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return slices.Clone(r.order)
}

// Get returns the named backend, or an error if it doesn't exist.
func (r *backendRegistry) Get(name string) (backend.ObjectBackend, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	b, ok := r.backends[name]
	if !ok {
		return nil, fmt.Errorf("backend %s not found", name)
	}
	return b, nil
}

// Add registers a backend and appends it to the deterministic routing order.
func (r *backendRegistry) Add(name string, be backend.ObjectBackend, paused bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.backends[name]; exists {
		return fmt.Errorf("backend %s already exists", name)
	}
	r.backends[name] = be
	r.order = append(r.order, name)
	if paused {
		r.writePaused[name] = true
	}
	return nil
}

// Replace swaps one live backend client without changing its routing order.
// Existing in-flight operations may finish on the old client; subsequent
// lookups observe the replacement. This is used when an account-scoped proxy
// is updated at runtime.
func (r *backendRegistry) Replace(name string, be backend.ObjectBackend) (backend.ObjectBackend, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	old, exists := r.backends[name]
	if !exists {
		return nil, fmt.Errorf("backend %s not found", name)
	}
	r.backends[name] = be
	return old, nil
}

// SetWritePaused atomically controls whether a backend may receive newly
// allocated objects. The backend remains registered so indexed reads,
// deletes, and already-issued direct-upload grants keep working.
func (r *backendRegistry) SetWritePaused(name string, paused bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.backends[name]; !exists {
		return fmt.Errorf("backend %s not found", name)
	}
	if paused {
		r.writePaused[name] = true
	} else {
		delete(r.writePaused, name)
	}
	return nil
}

// IsWritePaused reports whether new placement is disabled for a backend.
func (r *backendRegistry) IsWritePaused(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.writePaused[name]
}

// ExcludeWritePaused removes backends that an operator paused for new
// placement while preserving the caller's slice.
func (r *backendRegistry) ExcludeWritePaused(eligible []string) []string {
	return slices.DeleteFunc(slices.Clone(eligible), r.IsWritePaused)
}

// IsDraining returns true if the named backend is currently being
// drained. Returns false when no drain manager is set (e.g. early
// startup before SetDrainChecker).
func (r *backendRegistry) IsDraining(name string) bool {
	r.mu.RLock()
	drainMgr := r.drainMgr
	r.mu.RUnlock()
	if drainMgr == nil {
		return false
	}
	return drainMgr.IsDraining(name)
}

// ExcludeDraining filters out backends that are currently draining.
func (r *backendRegistry) ExcludeDraining(eligible []string) []string {
	return slices.DeleteFunc(slices.Clone(eligible), r.IsDraining)
}

// ExcludeUnhealthy filters out backends whose circuit breaker is open
// and not probe-eligible. Backends that are not breaker-wrapped pass
// through unconditionally.
func (r *backendRegistry) ExcludeUnhealthy(eligible []string) []string {
	return slices.DeleteFunc(slices.Clone(eligible), func(name string) bool {
		b, err := r.Get(name)
		if err != nil {
			return true
		}
		cb, ok := b.(*backend.CircuitBreakerBackend)
		return ok && cb.State() == breaker.StateOpen && !cb.ProbeEligible()
	})
}
