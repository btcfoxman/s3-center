package config

import (
	"strings"
	"testing"
	"time"
)

func TestDirectUploadAccessPresignTTLDefaultsToThreeHours(t *testing.T) {
	t.Parallel()
	cfg := DirectUploadConfig{
		Enabled:    true,
		SessionTTL: 10 * time.Minute,
		Tokens:     []DirectUploadToken{{Name: "client", Token: "secret"}},
	}
	if errs := cfg.setDefaultsAndValidate(); len(errs) != 0 {
		t.Fatalf("validation errors = %v", errs)
	}
	if cfg.AccessPresignTTL != 3*time.Hour {
		t.Fatalf("access presign TTL = %v, want 3h", cfg.AccessPresignTTL)
	}
	if cfg.PresignTTL != 10*time.Minute {
		t.Fatalf("upload presign TTL changed to %v", cfg.PresignTTL)
	}
}

func TestDirectUploadAccessPresignTTLRejectsValuesOutsideWholeHourRange(t *testing.T) {
	t.Parallel()
	for _, ttl := range []time.Duration{-time.Hour, 30 * time.Minute, MaxAccessPresignTTL + time.Hour} {
		cfg := DirectUploadConfig{
			Enabled:          true,
			PresignTTL:       time.Minute,
			AccessPresignTTL: ttl,
			SessionTTL:       time.Minute,
			Tokens:           []DirectUploadToken{{Name: "client", Token: "secret"}},
		}
		errs := cfg.setDefaultsAndValidate()
		if len(errs) == 0 || !strings.Contains(errs[0].Error(), "access_presign_ttl") {
			t.Errorf("TTL %v validation errors = %v, want access_presign_ttl error", ttl, errs)
		}
	}
}
