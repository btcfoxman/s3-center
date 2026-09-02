// Package backendmanage owns database-backed Cloudflare R2 backend onboarding.
package backendmanage

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/breaker"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/proxy/infra"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

const (
	defaultFreeTierBytes = int64(10_000_000_000)
	defaultBufferBytes   = int64(1_000_000_000)
	definitionVersion    = 1
)

var (
	accountIDPattern   = regexp.MustCompile(`^[a-fA-F0-9]{32}$`)
	backendNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,62}$`)
	bucketNamePattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`)
)

// Definition is the encrypted durable representation. Version allows future
// schema migrations without exposing secrets as relational columns.
type Definition struct {
	Version int                  `json:"version"`
	Backend config.BackendConfig `json:"backend"`
	Managed bool                 `json:"managed"`
}

// CreateRequest is accepted from the authenticated admin UI.
type CreateRequest struct {
	Name            string `json:"name"`
	AccountLabel    string `json:"account_label"`
	AccountID       string `json:"account_id"`
	Jurisdiction    string `json:"jurisdiction"`
	Bucket          string `json:"bucket"`
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"` //nolint:gosec // request credential, never returned
	ProxyURL        string `json:"proxy_url"`
	WarningPercent  int    `json:"warning_percent"`
}

// UpdateProxyRequest changes the server-side egress used by every dynamically
// managed bucket belonging to one Cloudflare account. An empty ProxyURL means
// direct access.
type UpdateProxyRequest struct {
	AccountID string `json:"account_id"`
	ProxyURL  string `json:"proxy_url"`
}

// View is safe to return to browsers. Credentials and proxy userinfo are never
// included.
type View struct {
	Name            string `json:"name"`
	Provider        string `json:"provider"`
	AccountLabel    string `json:"account_label"`
	AccountID       string `json:"account_id"`
	Bucket          string `json:"bucket"`
	QuotaGroup      string `json:"quota_group"`
	FreeTierBytes   int64  `json:"free_tier_bytes"`
	AdmissibleBytes int64  `json:"admissible_bytes"`
	WarningPercent  int    `json:"warning_percent"`
	Proxy           string `json:"proxy"`
	Managed         bool   `json:"managed"`
}

type Store interface {
	core.ManagedBackendStore
	SyncQuotaLimits(ctx context.Context, backends []config.BackendConfig) error
}

// BucketBackend is an object backend that can verify its configured bucket.
// Keeping the factory injectable makes account-level proxy replacement
// testable without contacting Cloudflare.
type BucketBackend interface {
	backend.ObjectBackend
	CheckBucket(ctx context.Context) error
}

// BackendFactory creates a bucket client from one validated definition.
type BackendFactory func(ctx context.Context, cfg *config.BackendConfig) (BucketBackend, error)

type Manager struct {
	enabled               bool
	cipher                *definitionCipher
	store                 Store
	runtime               *infra.BackendRuntime
	breakerConfig         config.BackendCircuitBreakerConfig
	breakerRegistry       *breaker.Registry
	maxObjectSize         int64
	defaultWarningPercent int
	backendFactory        BackendFactory

	createMu    sync.Mutex
	mu          sync.RWMutex
	definitions map[string]*Definition
}

type proxyReplacement struct {
	definition        *Definition
	encrypted         []byte
	previousEncrypted []byte
	live              backend.ObjectBackend
	previousLive      backend.ObjectBackend
	cb                *backend.CircuitBreakerBackend
	previousCB        *backend.CircuitBreakerBackend
}

type ManagerDeps struct {
	Config          *config.Config
	Store           Store
	Runtime         *infra.BackendRuntime
	BreakerRegistry *breaker.Registry
	Definitions     []Definition
	BackendFactory  BackendFactory
}

func New(deps ManagerDeps) (*Manager, error) {
	backendFactory := deps.BackendFactory
	if backendFactory == nil {
		backendFactory = func(ctx context.Context, cfg *config.BackendConfig) (BucketBackend, error) {
			return backend.NewS3Backend(ctx, cfg)
		}
	}
	m := &Manager{
		enabled:               deps.Config.ManagedBackends.Enabled,
		store:                 deps.Store,
		runtime:               deps.Runtime,
		breakerConfig:         deps.Config.BackendCircuitBreaker,
		breakerRegistry:       deps.BreakerRegistry,
		maxObjectSize:         deps.Config.Server.MaxObjectSize,
		defaultWarningPercent: deps.Config.ManagedBackends.DefaultWarningPercent,
		backendFactory:        backendFactory,
		definitions:           make(map[string]*Definition, len(deps.Definitions)),
	}
	for idx := range deps.Definitions {
		definition := deps.Definitions[idx]
		m.definitions[definition.Backend.Name] = &definition
	}
	if m.enabled {
		cipher, err := newDefinitionCipher(deps.Config.ManagedBackends.MasterKey)
		if err != nil {
			return nil, err
		}
		m.cipher = cipher
	}
	return m, nil
}

func (m *Manager) Enabled() bool { return m != nil && m.enabled }

func (m *Manager) List() []View {
	m.mu.RLock()
	defer m.mu.RUnlock()
	views := make([]View, 0, len(m.definitions))
	for _, definition := range m.definitions {
		views = append(views, viewOf(definition))
	}
	slices.SortFunc(views, func(a, b View) int { return strings.Compare(a.Name, b.Name) })
	return views
}

func (m *Manager) HasBackend(name string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.definitions[name]
	return ok
}

func viewOf(definition *Definition) View {
	b := definition.Backend
	proxy := "直连"
	if b.ProxyURL != "" {
		if parsed, err := url.Parse(b.ProxyURL); err == nil {
			proxy = parsed.Scheme + "://" + parsed.Host
		}
	}
	return View{
		Name: b.Name, Provider: b.Provider, AccountLabel: b.AccountLabel, AccountID: b.AccountID,
		Bucket: b.Bucket, QuotaGroup: b.QuotaGroup, FreeTierBytes: b.RawLimitBytes,
		AdmissibleBytes: b.QuotaBytes, WarningPercent: b.WarningPercent,
		Proxy: proxy, Managed: definition.Managed,
	}
}

func (m *Manager) Create(ctx context.Context, request *CreateRequest) (View, error) {
	if !m.Enabled() {
		return View{}, errors.New("managed backends are disabled")
	}
	m.createMu.Lock()
	defer m.createMu.Unlock()
	definition, err := buildDefinition(request, m.maxObjectSize, m.defaultWarningPercent)
	if err != nil {
		return View{}, err
	}
	name := definition.Backend.Name

	m.mu.RLock()
	if _, exists := m.definitions[name]; exists {
		m.mu.RUnlock()
		return View{}, fmt.Errorf("backend %q already exists", name)
	}
	m.mu.RUnlock()

	live, cbBackend, err := m.probeBackend(ctx, definition)
	if err != nil {
		return View{}, err
	}

	encrypted, err := m.cipher.seal(name, definition)
	if err != nil {
		return View{}, err
	}
	if err := m.store.UpsertManagedBackend(ctx, name, encrypted); err != nil {
		return View{}, err
	}
	rollback := true
	defer func() {
		if rollback {
			_ = m.store.DeleteManagedBackend(context.Background(), name)
		}
	}()
	if err := m.store.SyncQuotaLimits(ctx, []config.BackendConfig{definition.Backend}); err != nil {
		return View{}, err
	}

	limits := core.UsageLimits{
		APIRequestLimit:  definition.Backend.APIRequestLimit,
		EgressByteLimit:  definition.Backend.EgressByteLimit,
		IngressByteLimit: definition.Backend.IngressByteLimit,
	}
	if err := m.runtime.AddBackend(name, live, definition.Backend.MaxObjectSize, limits); err != nil {
		return View{}, err
	}
	if cbBackend != nil && m.breakerRegistry != nil {
		m.breakerRegistry.Register(cbBackend)
	}
	m.mu.Lock()
	m.definitions[name] = definition
	m.mu.Unlock()
	rollback = false
	return viewOf(definition), nil
}

// UpdateAccountProxy validates a candidate egress against every managed
// bucket in the account, then persists and hot-swaps all clients as one
// logical operation. A failed validation leaves the previous proxy untouched.
func (m *Manager) UpdateAccountProxy(ctx context.Context, request *UpdateProxyRequest) ([]View, error) {
	if !m.Enabled() {
		return nil, errors.New("managed backends are disabled")
	}
	request.AccountID = strings.ToLower(strings.TrimSpace(request.AccountID))
	request.ProxyURL = normalizeProxyURL(request.ProxyURL)
	if !accountIDPattern.MatchString(request.AccountID) {
		return nil, errors.New("cloudflare account ID must be 32 hexadecimal characters")
	}

	m.createMu.Lock()
	defer m.createMu.Unlock()

	targets, err := m.accountProxyTargets(request.AccountID)
	if err != nil {
		return nil, err
	}
	replacements, err := m.prepareProxyReplacements(ctx, targets, request.ProxyURL)
	if err != nil {
		return nil, err
	}
	if err := m.persistProxyReplacements(ctx, replacements); err != nil {
		return nil, err
	}
	if err := m.replaceProxyClients(replacements); err != nil {
		return nil, errors.Join(err, m.restoreStoredProxies(replacements))
	}
	return m.commitProxyReplacements(replacements), nil
}

func (m *Manager) accountProxyTargets(accountID string) ([]Definition, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	targets := make([]Definition, 0)
	for _, definition := range m.definitions {
		if definition.Backend.AccountID != accountID {
			continue
		}
		if !definition.Managed {
			return nil, errors.New("account contains a config-managed backend; update proxy_url in config.yaml")
		}
		targets = append(targets, *definition)
	}
	if len(targets) == 0 {
		return nil, errors.New("no dynamically managed backends found for account")
	}
	slices.SortFunc(targets, func(a, b Definition) int {
		return strings.Compare(a.Backend.Name, b.Backend.Name)
	})
	return targets, nil
}

func (m *Manager) prepareProxyReplacements(ctx context.Context, targets []Definition, proxyURL string) ([]proxyReplacement, error) {
	replacements := make([]proxyReplacement, 0, len(targets))
	for idx := range targets {
		previous := targets[idx]
		updated := previous
		updated.Backend.ProxyURL = proxyURL
		if err := config.NormalizeAndValidateBackend(&updated.Backend); err != nil {
			return nil, err
		}
		live, cbBackend, err := m.probeBackend(ctx, &updated)
		if err != nil {
			return nil, fmt.Errorf("validate backend %q through candidate proxy: %w", updated.Backend.Name, err)
		}
		previousLive, err := m.runtime.GetBackend(updated.Backend.Name)
		if err != nil {
			return nil, err
		}
		encrypted, err := m.cipher.seal(updated.Backend.Name, &updated)
		if err != nil {
			return nil, err
		}
		previousEncrypted, err := m.cipher.seal(previous.Backend.Name, &previous)
		if err != nil {
			return nil, err
		}
		previousCB, _ := previousLive.(*backend.CircuitBreakerBackend)
		replacements = append(replacements, proxyReplacement{
			definition: &updated, encrypted: encrypted, previousEncrypted: previousEncrypted,
			live: live, previousLive: previousLive, cb: cbBackend, previousCB: previousCB,
		})
	}
	return replacements, nil
}

func (m *Manager) persistProxyReplacements(ctx context.Context, replacements []proxyReplacement) error {
	persisted := 0
	for idx := range replacements {
		item := &replacements[idx]
		if err := m.store.UpsertManagedBackend(ctx, item.definition.Backend.Name, item.encrypted); err != nil {
			return errors.Join(err, m.restoreStoredProxies(replacements[:persisted]))
		}
		persisted++
	}
	return nil
}

func (m *Manager) replaceProxyClients(replacements []proxyReplacement) error {
	replaced := 0
	for idx := range replacements {
		item := &replacements[idx]
		if _, err := m.runtime.ReplaceBackend(item.definition.Backend.Name, item.live); err != nil {
			for rollbackIdx := replaced - 1; rollbackIdx >= 0; rollbackIdx-- {
				rollback := &replacements[rollbackIdx]
				_, _ = m.runtime.ReplaceBackend(rollback.definition.Backend.Name, rollback.previousLive)
			}
			return err
		}
		replaced++
	}
	return nil
}

func (m *Manager) commitProxyReplacements(replacements []proxyReplacement) []View {
	m.mu.Lock()
	for idx := range replacements {
		item := &replacements[idx]
		m.definitions[item.definition.Backend.Name] = item.definition
	}
	m.mu.Unlock()

	views := make([]View, 0, len(replacements))
	for idx := range replacements {
		item := &replacements[idx]
		if m.breakerRegistry != nil {
			m.breakerRegistry.Unregister(item.previousCB)
			m.breakerRegistry.Register(item.cb)
		}
		views = append(views, viewOf(item.definition))
	}
	return views
}

func (m *Manager) restoreStoredProxies(replacements []proxyReplacement) error {
	restoreCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var restoreErr error
	for idx := range replacements {
		item := &replacements[idx]
		if err := m.store.UpsertManagedBackend(restoreCtx, item.definition.Backend.Name, item.previousEncrypted); err != nil {
			restoreErr = errors.Join(restoreErr, fmt.Errorf("restore backend %q: %w", item.definition.Backend.Name, err))
		}
	}
	return restoreErr
}

func (m *Manager) probeBackend(ctx context.Context, definition *Definition) (backend.ObjectBackend, *backend.CircuitBreakerBackend, error) {
	candidate, err := m.backendFactory(ctx, &definition.Backend)
	if err != nil {
		return nil, nil, err
	}
	checkCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := candidate.CheckBucket(checkCtx); err != nil {
		return nil, nil, err
	}
	if !m.breakerConfig.Enabled {
		return candidate, nil, nil
	}
	cbBackend := backend.NewCircuitBreakerBackend(candidate, backend.CircuitBreakerConfig{
		Name: definition.Backend.Name, Threshold: m.breakerConfig.FailureThreshold, Timeout: m.breakerConfig.OpenTimeout,
	})
	return cbBackend, cbBackend, nil
}

func normalizeProxyURL(raw string) string {
	proxyURL := strings.TrimSpace(raw)
	if proxyURL != "" && !strings.Contains(proxyURL, "://") {
		proxyURL = "http://" + proxyURL
	}
	return proxyURL
}

func buildDefinition(request *CreateRequest, maxObjectSize int64, defaultWarningPercent int) (*Definition, error) {
	request.Name = strings.TrimSpace(request.Name)
	request.AccountLabel = strings.TrimSpace(request.AccountLabel)
	request.AccountID = strings.ToLower(strings.TrimSpace(request.AccountID))
	request.Bucket = strings.TrimSpace(request.Bucket)
	request.AccessKeyID = strings.TrimSpace(request.AccessKeyID)
	request.SecretAccessKey = strings.TrimSpace(request.SecretAccessKey)
	request.ProxyURL = normalizeProxyURL(request.ProxyURL)
	if !backendNamePattern.MatchString(request.Name) {
		return nil, errors.New("backend name must be 1-63 letters, numbers, dots, underscores, or hyphens")
	}
	if len(request.AccountLabel) > 80 {
		return nil, errors.New("account label must not exceed 80 characters")
	}
	if !accountIDPattern.MatchString(request.AccountID) {
		return nil, errors.New("cloudflare account ID must be 32 hexadecimal characters")
	}
	if !bucketNamePattern.MatchString(request.Bucket) {
		return nil, errors.New("R2 bucket name must be 3-63 lowercase letters, numbers, or hyphens")
	}
	if request.AccessKeyID == "" || request.SecretAccessKey == "" {
		return nil, errors.New("R2 access key ID and secret access key are required")
	}
	warning := request.WarningPercent
	if warning == 0 {
		warning = defaultWarningPercent
	}
	endpointSuffix, ok := map[string]string{
		"":        ".r2.cloudflarestorage.com",
		"default": ".r2.cloudflarestorage.com",
		"eu":      ".eu.r2.cloudflarestorage.com",
		"fedramp": ".fedramp.r2.cloudflarestorage.com",
		"us":      ".us.r2.cloudflarestorage.com",
	}[strings.ToLower(strings.TrimSpace(request.Jurisdiction))]
	if !ok {
		return nil, errors.New("unsupported R2 jurisdiction")
	}
	backendConfig := config.BackendConfig{
		Name: request.Name, Provider: "cloudflare_r2", AccountID: request.AccountID,
		AccountLabel: request.AccountLabel, Endpoint: "https://" + request.AccountID + endpointSuffix,
		Region: "auto", Bucket: request.Bucket, AccessKeyID: request.AccessKeyID,
		SecretAccessKey: request.SecretAccessKey, CredentialSource: config.CredentialSourceStatic,
		ForcePathStyle: true, QuotaBytes: defaultFreeTierBytes - 2*defaultBufferBytes,
		MaxObjectSize: maxObjectSize, QuotaGroup: "cf-" + request.AccountID,
		RawLimitBytes: defaultFreeTierBytes, StaticBufferBytes: defaultBufferBytes,
		DynamicBufferBytes: defaultBufferBytes, BillingPolicy: "free_capped",
		ProxyURL: request.ProxyURL, WarningPercent: warning,
	}
	if err := config.NormalizeAndValidateBackend(&backendConfig); err != nil {
		return nil, err
	}
	return &Definition{Version: definitionVersion, Backend: backendConfig, Managed: true}, nil
}

// Load decrypts every persisted definition during startup.
func Load(ctx context.Context, store core.ManagedBackendStore, masterKey string) ([]Definition, error) {
	cipher, err := newDefinitionCipher(masterKey)
	if err != nil {
		return nil, err
	}
	records, err := store.ListManagedBackends(ctx)
	if err != nil {
		return nil, err
	}
	definitions := make([]Definition, 0, len(records))
	for _, record := range records {
		definition, err := cipher.open(record.Name, record.EncryptedConfig)
		if err != nil {
			return nil, err
		}
		if definition.Backend.Name != record.Name {
			return nil, fmt.Errorf("managed backend name mismatch: row %q payload %q", record.Name, definition.Backend.Name)
		}
		if err := config.NormalizeAndValidateBackend(&definition.Backend); err != nil {
			return nil, fmt.Errorf("validate managed backend %q: %w", record.Name, err)
		}
		definition.Managed = true
		definitions = append(definitions, definition)
	}
	return definitions, nil
}
