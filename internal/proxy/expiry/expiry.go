// -------------------------------------------------------------------------------
// Expiry - Lifecycle Rule Evaluation
//
// Author: Alex Freidah
//
// Evaluates lifecycle rules and deletes objects whose created_at is older than
// the configured expiration period. Deletion goes through the normal object
// delete path so quota decrement, cache invalidation, and the cleanup queue all
// behave exactly as they do for a client-issued delete.
//
// Owns the reloadable lifecycle config rather than reading it from a facade:
// the rules and the code that applies them belong together, and the reload
// hook writes here directly.
// -------------------------------------------------------------------------------

// Package expiry applies lifecycle expiration rules to stored objects.
package expiry

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/syncutil"
)

//go:generate mockgen -destination=mock_test.go -package=expiry github.com/afreidah/s3-orchestrator/internal/proxy/expiry ObjectDeleter

// defaultBatchSize bounds one store query when the operator configured none.
const defaultBatchSize = 100

// ObjectDeleter removes one object through the full delete path.
// *object.Manager satisfies it.
type ObjectDeleter interface {
	DeleteObject(ctx context.Context, key string) error
}

// Manager applies lifecycle rules on demand. Safe for concurrent use; the
// config is swapped atomically by the reload path.
type Manager struct {
	store       core.ExpiredObjectsLister
	policyStore core.LifecyclePolicyStore
	objects     ObjectDeleter
	cfg         syncutil.AtomicConfig[config.LifecycleConfig]
	log         *slog.Logger
}

// New builds a Manager. log may be nil, in which case the default logger is
// used at call time.
func New(store core.ExpiredObjectsLister, objects ObjectDeleter, log *slog.Logger) *Manager {
	return &Manager{store: store, objects: objects, log: log}
}

// SetPolicyStore enables database-backed policies managed by the web console.
// Wiring calls this during startup before workers begin, so no synchronization
// is needed around the immutable dependency.
func (m *Manager) SetPolicyStore(store core.LifecyclePolicyStore) { m.policyStore = store }

// SetConfig atomically replaces the lifecycle configuration.
func (m *Manager) SetConfig(cfg *config.LifecycleConfig) { m.cfg.Store(cfg) }

// Config returns the current lifecycle configuration, or nil when unset.
func (m *Manager) Config() *config.LifecycleConfig { return m.cfg.Load() }

// HasPolicyStore reports whether console-managed policies are available. The
// lifecycle ticker uses this to remain active even when YAML has no rules.
func (m *Manager) HasPolicyStore() bool { return m.policyStore != nil }

// ConfiguredRules merges hot-reloadable YAML rules with enabled database-backed
// policies. A dynamic rule replaces a static rule with the same prefix; the UI
// rejects that overlap, but deterministic precedence keeps a race harmless.
func (m *Manager) ConfiguredRules(ctx context.Context) ([]config.LifecycleRule, error) {
	var rules []config.LifecycleRule
	if cfg := m.Config(); cfg != nil {
		rules = slices.Clone(cfg.Rules)
	}
	if m.policyStore == nil {
		return rules, nil
	}

	policies, err := m.policyStore.ListLifecyclePolicies(ctx)
	if err != nil {
		return nil, fmt.Errorf("load lifecycle policies: %w", err)
	}
	byPrefix := make(map[string]int, len(rules))
	for i := range rules {
		byPrefix[rules[i].Prefix] = i
	}
	for _, policy := range policies {
		if !policy.Enabled {
			continue
		}
		rule := config.LifecycleRule{Prefix: policy.Prefix, ExpirationHours: policy.ExpirationHours}
		if i, ok := byPrefix[policy.Prefix]; ok {
			rules[i] = rule
			continue
		}
		byPrefix[policy.Prefix] = len(rules)
		rules = append(rules, rule)
	}
	return rules, nil
}

// ProcessConfigured loads the policy snapshot for this tick and applies it.
func (m *Manager) ProcessConfigured(ctx context.Context) (deleted, failed int, err error) {
	rules, err := m.ConfiguredRules(ctx)
	if err != nil {
		return 0, 0, err
	}
	deleted, failed = m.ProcessRules(ctx, rules)
	return deleted, failed, nil
}

// logger returns the configured logger, falling back to the default so a
// Manager built without one in a test still logs through the standard handler.
func (m *Manager) logger() *slog.Logger {
	if m.log == nil {
		return slog.Default()
	}
	return m.log
}

// ProcessRules evaluates every rule and deletes the objects each one expires,
// returning the total deleted and failed counts across all rules.
func (m *Manager) ProcessRules(ctx context.Context, rules []config.LifecycleRule) (deleted, failed int) {
	batchSize := batchSizeFor(m.Config())

	ctx, span := telemetry.StartSpan(ctx, "ProcessLifecycleRules",
		telemetry.AttrOperation.String("lifecycle"),
	)
	defer span.End()

	for _, rule := range rules {
		d, f := m.applyRule(ctx, rule, batchSize)
		deleted += d
		failed += f
	}
	return deleted, failed
}

// batchSizeFor returns the per-tick batch size, falling back to the default
// when the operator configured none.
func batchSizeFor(cfg *config.LifecycleConfig) int {
	if cfg != nil && cfg.BatchSize > 0 {
		return cfg.BatchSize
	}
	return defaultBatchSize
}

// applyRule runs a single rule until the store stops returning expired
// objects, or a full batch produces zero successful deletions. That second
// condition is the infinite-loop guard: without it, a backend outage means
// every batch fails and the same rows are re-listed forever.
func (m *Manager) applyRule(ctx context.Context, rule config.LifecycleRule, batchSize int) (deleted, failed int) {
	cutoff := time.Now().Add(-rule.ExpirationDuration())
	for {
		objects, err := m.store.ListExpiredObjects(ctx, rule.Prefix, cutoff, batchSize)
		if err != nil {
			m.logger().ErrorContext(ctx, "failed to list expired objects",
				slog.String("prefix", rule.Prefix), "error", err)
			failed++
			return deleted, failed
		}
		if len(objects) == 0 {
			return deleted, failed
		}

		batchDeleted, batchFailed := m.deleteBatch(ctx, rule, objects)
		deleted += batchDeleted
		failed += batchFailed

		if batchDeleted == 0 {
			m.logger().WarnContext(ctx, "batch yielded zero deletions, stopping rule",
				"prefix", rule.Prefix, "batch_failed", len(objects))
			return deleted, failed
		}
		if len(objects) < batchSize {
			return deleted, failed
		}
	}
}

// deleteBatch deletes one batch of expired objects, emitting an audit event
// and a metric per outcome.
func (m *Manager) deleteBatch(ctx context.Context, rule config.LifecycleRule, objects []core.ObjectLocation) (deleted, failed int) {
	for i := range objects {
		key := objects[i].ObjectKey
		if err := m.objects.DeleteObject(ctx, key); err != nil {
			m.logger().WarnContext(ctx, "failed to delete expired object",
				slog.String("key", key), "error", err)
			telemetry.LifecycleFailedTotal.Inc()
			failed++
			continue
		}
		audit.Log(ctx, "lifecycle.delete",
			slog.String("key", key),
			slog.String("prefix", rule.Prefix),
			slog.Int("retention_hours", int(rule.ExpirationDuration()/time.Hour)),
		)
		telemetry.LifecycleDeletedTotal.Inc()
		deleted++
	}
	return deleted, failed
}
