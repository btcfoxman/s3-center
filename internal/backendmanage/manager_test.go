package backendmanage

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/proxy/infra"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

const testMasterKey = "0123456789abcdef0123456789abcdef"

func TestDefinitionCipherRoundTripAndAAD(t *testing.T) {
	t.Parallel()
	cipher, err := newDefinitionCipher(testMasterKey)
	if err != nil {
		t.Fatal(err)
	}
	definition, err := buildDefinition(&CreateRequest{
		Name: "r2-team-02", AccountLabel: "Team 02",
		AccountID: strings.Repeat("a", 32), Bucket: "team-assets",
		AccessKeyID: "access-key", SecretAccessKey: "top-secret-value",
		ProxyURL: "203.0.113.9:8080", WarningPercent: 82,
	}, 256<<20, 80)
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := cipher.seal(definition.Backend.Name, definition)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encrypted, []byte("top-secret-value")) || bytes.Contains(encrypted, []byte("203.0.113.9")) {
		t.Fatal("encrypted definition contains sensitive plaintext")
	}
	opened, err := cipher.open(definition.Backend.Name, encrypted)
	if err != nil {
		t.Fatal(err)
	}
	if opened.Backend.SecretAccessKey != definition.Backend.SecretAccessKey || opened.Backend.ProxyURL != "http://203.0.113.9:8080" {
		t.Fatalf("round trip mismatch: %+v", opened.Backend)
	}
	if _, err := cipher.open("different-name", encrypted); err == nil {
		t.Fatal("payload should be bound to its row name through AEAD additional data")
	}
}

func TestBuildDefinitionCloudflareDefaults(t *testing.T) {
	t.Parallel()
	definition, err := buildDefinition(&CreateRequest{
		Name: "r2-eu", AccountID: strings.Repeat("b", 32), Jurisdiction: "eu",
		Bucket: "eu-assets", AccessKeyID: "key", SecretAccessKey: "secret",
	}, 1234, 75)
	if err != nil {
		t.Fatal(err)
	}
	b := definition.Backend
	if b.Endpoint != "https://"+strings.Repeat("b", 32)+".eu.r2.cloudflarestorage.com" {
		t.Errorf("endpoint = %q", b.Endpoint)
	}
	if b.RawLimitBytes != 10_000_000_000 || b.QuotaBytes != 8_000_000_000 {
		t.Errorf("free/safe limits = %d/%d", b.RawLimitBytes, b.QuotaBytes)
	}
	if b.WarningPercent != 75 || b.MaxObjectSize != 1234 || !b.ForcePathStyle {
		t.Fatalf("defaults not applied: %+v", b)
	}
}

//nolint:gosec // Deliberate fake credential verifies browser redaction.
func TestViewRedactsProxyCredentials(t *testing.T) {
	t.Parallel()
	view := viewOf(&Definition{Backend: config.BackendConfig{
		Name:     "b",
		ProxyURL: "http://user:password@203.0.113.7:3128",
	}})
	if view.Proxy != "http://203.0.113.7:3128" {
		t.Fatalf("redacted proxy = %q", view.Proxy)
	}
}

type proxyTestBackend struct {
	backend.ObjectBackend
	proxy    string
	checkErr error
}

func (b *proxyTestBackend) CheckBucket(context.Context) error { return b.checkErr }

type proxyTestStore struct {
	payloads map[string][]byte
	writes   int
	attempts int
	failAt   int
}

func (s *proxyTestStore) ListManagedBackends(context.Context) ([]core.ManagedBackendRecord, error) {
	return nil, nil
}

func (s *proxyTestStore) UpsertManagedBackend(_ context.Context, name string, encrypted []byte) error {
	s.attempts++
	if s.failAt > 0 && s.attempts == s.failAt {
		return errors.New("injected managed-backend write failure")
	}
	s.writes++
	s.payloads[name] = bytes.Clone(encrypted)
	return nil
}

func (s *proxyTestStore) DeleteManagedBackend(context.Context, string) error { return nil }

func (s *proxyTestStore) SyncQuotaLimits(context.Context, []config.BackendConfig) error { return nil }

//nolint:gocognit,gosec // One end-to-end assertion path intentionally uses fake proxy credentials.
func TestUpdateAccountProxyValidatesPersistsAndHotSwapsEveryBucket(t *testing.T) {
	t.Parallel()
	const accountID = "0123456789abcdef0123456789abcdef"
	definitions := []Definition{
		managedTestDefinition(t, "r2-a", accountID, "bucket-a", "http://old.example:8080"),
		managedTestDefinition(t, "r2-b", accountID, "bucket-b", "http://old.example:8080"),
	}
	oldA := &proxyTestBackend{proxy: "old-a"}
	oldB := &proxyTestBackend{proxy: "old-b"}
	runtime := proxyTestRuntime([]string{"r2-a", "r2-b"}, map[string]backend.ObjectBackend{
		"r2-a": oldA,
		"r2-b": oldB,
	})
	store := &proxyTestStore{payloads: make(map[string][]byte)}
	seen := make(map[string]string)
	manager, err := New(ManagerDeps{
		Config: managedTestConfig(), Store: store, Runtime: runtime, Definitions: definitions,
		BackendFactory: func(_ context.Context, cfg *config.BackendConfig) (BucketBackend, error) {
			seen[cfg.Name] = cfg.ProxyURL
			return &proxyTestBackend{proxy: cfg.ProxyURL}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	views, err := manager.UpdateAccountProxy(context.Background(), &UpdateProxyRequest{
		AccountID: accountID,
		ProxyURL:  "user:secret@proxy.example:3128",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 2 || store.writes != 2 {
		t.Fatalf("views/writes = %d/%d, want 2/2", len(views), store.writes)
	}
	for _, name := range []string{"r2-a", "r2-b"} {
		wantProxy := "http://user:secret@proxy.example:3128"
		if seen[name] != wantProxy {
			t.Errorf("factory proxy for %s = %q", name, seen[name])
		}
		live, getErr := runtime.GetBackend(name)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if live.(*proxyTestBackend).proxy != wantProxy {
			t.Errorf("live proxy for %s = %q", name, live.(*proxyTestBackend).proxy)
		}
		stored, openErr := manager.cipher.open(name, store.payloads[name])
		if openErr != nil {
			t.Fatal(openErr)
		}
		if stored.Backend.ProxyURL != wantProxy {
			t.Errorf("stored proxy for %s = %q", name, stored.Backend.ProxyURL)
		}
	}
	if views[0].Proxy != "http://proxy.example:3128" {
		t.Errorf("browser proxy = %q, want credentials redacted", views[0].Proxy)
	}
	if runtime.BackendOrder()[0] != "r2-a" || runtime.BackendOrder()[1] != "r2-b" {
		t.Fatalf("routing order changed: %v", runtime.BackendOrder())
	}
}

func TestUpdateAccountProxyProbeFailureLeavesAccountUntouched(t *testing.T) {
	t.Parallel()
	const accountID = "fedcba9876543210fedcba9876543210"
	definition := managedTestDefinition(t, "r2-a", accountID, "bucket-a", "")
	old := &proxyTestBackend{proxy: "direct"}
	runtime := proxyTestRuntime([]string{"r2-a"}, map[string]backend.ObjectBackend{"r2-a": old})
	store := &proxyTestStore{payloads: make(map[string][]byte)}
	manager, err := New(ManagerDeps{
		Config: managedTestConfig(), Store: store, Runtime: runtime, Definitions: []Definition{definition},
		BackendFactory: func(context.Context, *config.BackendConfig) (BucketBackend, error) {
			return &proxyTestBackend{checkErr: errors.New("proxy unavailable")}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := manager.UpdateAccountProxy(context.Background(), &UpdateProxyRequest{
		AccountID: accountID, ProxyURL: "http://bad.example:8080",
	}); err == nil {
		t.Fatal("expected proxy validation failure")
	}
	if store.writes != 0 {
		t.Fatalf("persisted %d definitions after failed probe", store.writes)
	}
	live, err := runtime.GetBackend("r2-a")
	if err != nil {
		t.Fatal(err)
	}
	if live != old || manager.List()[0].Proxy != "直连" {
		t.Fatal("failed proxy probe changed the live account")
	}
}

func TestUpdateAccountProxyPersistenceFailureRollsBackEarlierBuckets(t *testing.T) {
	t.Parallel()
	const accountID = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	definitions := []Definition{
		managedTestDefinition(t, "r2-a", accountID, "bucket-a", "http://old.example:8080"),
		managedTestDefinition(t, "r2-b", accountID, "bucket-b", "http://old.example:8080"),
	}
	oldA := &proxyTestBackend{proxy: "old-a"}
	oldB := &proxyTestBackend{proxy: "old-b"}
	runtime := proxyTestRuntime([]string{"r2-a", "r2-b"}, map[string]backend.ObjectBackend{
		"r2-a": oldA, "r2-b": oldB,
	})
	store := &proxyTestStore{payloads: make(map[string][]byte), failAt: 2}
	manager, err := New(ManagerDeps{
		Config: managedTestConfig(), Store: store, Runtime: runtime, Definitions: definitions,
		BackendFactory: func(_ context.Context, cfg *config.BackendConfig) (BucketBackend, error) {
			return &proxyTestBackend{proxy: cfg.ProxyURL}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for idx := range definitions {
		definition := &definitions[idx]
		payload, sealErr := manager.cipher.seal(definition.Backend.Name, definition)
		if sealErr != nil {
			t.Fatal(sealErr)
		}
		store.payloads[definition.Backend.Name] = payload
	}

	if _, err := manager.UpdateAccountProxy(context.Background(), &UpdateProxyRequest{
		AccountID: accountID, ProxyURL: "http://new.example:8080",
	}); err == nil {
		t.Fatal("expected persistence failure")
	}
	for idx := range definitions {
		definition := &definitions[idx]
		stored, openErr := manager.cipher.open(definition.Backend.Name, store.payloads[definition.Backend.Name])
		if openErr != nil {
			t.Fatal(openErr)
		}
		if stored.Backend.ProxyURL != "http://old.example:8080" {
			t.Errorf("stored proxy for %s was not rolled back: %q", definition.Backend.Name, stored.Backend.ProxyURL)
		}
	}
	liveA, _ := runtime.GetBackend("r2-a")
	liveB, _ := runtime.GetBackend("r2-b")
	if liveA != oldA || liveB != oldB {
		t.Fatal("runtime changed after persistence failure")
	}
}

func managedTestDefinition(t *testing.T, name, accountID, bucket, proxyURL string) Definition {
	t.Helper()
	definition, err := buildDefinition(&CreateRequest{
		Name: name, AccountID: accountID, Bucket: bucket,
		AccessKeyID: "access", SecretAccessKey: "secret", ProxyURL: proxyURL,
	}, 1024, 80)
	if err != nil {
		t.Fatal(err)
	}
	return *definition
}

func managedTestConfig() *config.Config {
	return &config.Config{
		Server: config.ServerConfig{MaxObjectSize: 1024},
		ManagedBackends: config.ManagedBackendsConfig{
			Enabled: true, MasterKey: testMasterKey, DefaultWarningPercent: 80,
		},
	}
}

func proxyTestRuntime(order []string, backends map[string]backend.ObjectBackend) *infra.BackendRuntime {
	tracker := counter.NewUsageTracker(counter.NewLocalCounterBackend(order), nil)
	return infra.New(&infra.Config{
		Backends: backends, Order: order, Usage: tracker, MaxObjectSizes: make(map[string]int64),
	})
}
