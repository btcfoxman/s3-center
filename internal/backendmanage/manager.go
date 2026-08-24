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

type Manager struct {
	enabled               bool
	cipher                *definitionCipher
	store                 Store
	runtime               *infra.BackendRuntime
	breakerConfig         config.BackendCircuitBreakerConfig
	breakerRegistry       *breaker.Registry
	maxObjectSize         int64
	defaultWarningPercent int

	createMu    sync.Mutex
	mu          sync.RWMutex
	definitions map[string]Definition
}

type ManagerDeps struct {
	Config          *config.Config
	Store           Store
	Runtime         *infra.BackendRuntime
	BreakerRegistry *breaker.Registry
	Definitions     []Definition
}

func New(deps ManagerDeps) (*Manager, error) {
	m := &Manager{
		enabled:               deps.Config.ManagedBackends.Enabled,
		store:                 deps.Store,
		runtime:               deps.Runtime,
		breakerConfig:         deps.Config.BackendCircuitBreaker,
		breakerRegistry:       deps.BreakerRegistry,
		maxObjectSize:         deps.Config.Server.MaxObjectSize,
		defaultWarningPercent: deps.Config.ManagedBackends.DefaultWarningPercent,
		definitions:           make(map[string]Definition, len(deps.Definitions)),
	}
	for _, definition := range deps.Definitions {
		m.definitions[definition.Backend.Name] = definition
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

func viewOf(definition Definition) View {
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

func (m *Manager) Create(ctx context.Context, request CreateRequest) (View, error) {
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

	s3Backend, err := backend.NewS3Backend(ctx, &definition.Backend)
	if err != nil {
		return View{}, err
	}
	checkCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := s3Backend.CheckBucket(checkCtx); err != nil {
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

	var live backend.ObjectBackend = s3Backend
	var cbBackend *backend.CircuitBreakerBackend
	if m.breakerConfig.Enabled {
		cbBackend = backend.NewCircuitBreakerBackend(s3Backend, backend.CircuitBreakerConfig{
			Name: name, Threshold: m.breakerConfig.FailureThreshold, Timeout: m.breakerConfig.OpenTimeout,
		})
		live = cbBackend
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

func buildDefinition(request CreateRequest, maxObjectSize int64, defaultWarningPercent int) (Definition, error) {
	request.Name = strings.TrimSpace(request.Name)
	request.AccountLabel = strings.TrimSpace(request.AccountLabel)
	request.AccountID = strings.ToLower(strings.TrimSpace(request.AccountID))
	request.Bucket = strings.TrimSpace(request.Bucket)
	request.AccessKeyID = strings.TrimSpace(request.AccessKeyID)
	request.SecretAccessKey = strings.TrimSpace(request.SecretAccessKey)
	request.ProxyURL = strings.TrimSpace(request.ProxyURL)
	if !backendNamePattern.MatchString(request.Name) {
		return Definition{}, errors.New("backend name must be 1-63 letters, numbers, dots, underscores, or hyphens")
	}
	if len(request.AccountLabel) > 80 {
		return Definition{}, errors.New("account label must not exceed 80 characters")
	}
	if !accountIDPattern.MatchString(request.AccountID) {
		return Definition{}, errors.New("Cloudflare account ID must be 32 hexadecimal characters")
	}
	if !bucketNamePattern.MatchString(request.Bucket) {
		return Definition{}, errors.New("R2 bucket name must be 3-63 lowercase letters, numbers, or hyphens")
	}
	if request.AccessKeyID == "" || request.SecretAccessKey == "" {
		return Definition{}, errors.New("R2 access key ID and secret access key are required")
	}
	if request.ProxyURL != "" && !strings.Contains(request.ProxyURL, "://") {
		request.ProxyURL = "http://" + request.ProxyURL
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
		return Definition{}, errors.New("unsupported R2 jurisdiction")
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
		return Definition{}, err
	}
	return Definition{Version: definitionVersion, Backend: backendConfig, Managed: true}, nil
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
