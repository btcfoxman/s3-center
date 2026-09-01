package config

import (
	"strings"
	"testing"
)

func TestValidateIndexOnlyModeAcceptsControlPlaneOnlyFeatures(t *testing.T) {
	t.Parallel()
	cfg := Config{DirectUpload: DirectUploadConfig{Enabled: true, IndexOnly: true}}
	if errs := cfg.validateIndexOnlyMode(); len(errs) != 0 {
		t.Fatalf("valid index-only config returned errors: %v", errs)
	}
}

func TestValidateIndexOnlyModeRejectsBodyTransformFeatures(t *testing.T) {
	t.Parallel()
	cfg := Config{
		DirectUpload: DirectUploadConfig{IndexOnly: true},
		Encryption:   EncryptionConfig{Enabled: true},
		Compression:  CompressionConfig{Enabled: true},
		Cache:        CacheConfig{Enabled: true},
		Replication:  ReplicationConfig{Factor: 2},
		Rebalance:    RebalanceConfig{Enabled: true},
		Integrity:    IntegrityConfig{Enabled: true},
	}
	errs := cfg.validateIndexOnlyMode()
	if len(errs) != 7 {
		t.Fatalf("index-only validation returned %d errors, want 7: %v", len(errs), errs)
	}
	joined := make([]string, len(errs))
	for i := range errs {
		joined[i] = errs[i].Error()
	}
	message := strings.Join(joined, "\n")
	for _, field := range []string{"direct_upload.enabled", "encryption.enabled", "compression.enabled", "cache.enabled", "replication.factor", "rebalance.enabled", "integrity.enabled"} {
		if !strings.Contains(message, field) {
			t.Errorf("validation errors do not mention %s: %s", field, message)
		}
	}
}

func TestDirectUploadChangesRequireRestart(t *testing.T) {
	t.Parallel()
	oldConfig := Config{DirectUpload: DirectUploadConfig{Enabled: true, Tokens: []DirectUploadToken{{Name: "client", Token: "old"}}}}
	newConfig := oldConfig
	newConfig.DirectUpload.Tokens = []DirectUploadToken{{Name: "client", Token: "rotated"}}
	changed := NonReloadableFieldsChanged(&oldConfig, &newConfig)
	for _, field := range changed {
		if field == "direct_upload" {
			return
		}
	}
	t.Fatalf("direct upload token rotation was not marked non-reloadable: %v", changed)
}
