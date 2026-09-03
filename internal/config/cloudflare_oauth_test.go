package config

import (
	"strings"
	"testing"
	"time"
)

func TestCloudflareOAuthConfigDefaultsWhenDisabled(t *testing.T) {
	t.Parallel()
	cfg := CloudflareOAuthConfig{}
	if errs := cfg.setDefaultsAndValidate(); len(errs) != 0 {
		t.Fatalf("disabled config errors = %v", errs)
	}
	if cfg.SessionTTL != 20*time.Minute || cfg.ConnectorScheme != "s3center" || cfg.TokenEndpointAuthMethod != "client_secret_basic" {
		t.Fatalf("defaults = %+v", cfg)
	}
}

func TestCloudflareOAuthConfigValidatesEnabledFields(t *testing.T) {
	t.Parallel()
	cfg := CloudflareOAuthConfig{Enabled: true}
	errs := cfg.setDefaultsAndValidate()
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "redirect_url") {
		t.Fatalf("enabled empty config errors = %v", errs)
	}

	valid := CloudflareOAuthConfig{
		Enabled:     true,
		RedirectURL: "https://s3.example.com/ui/",
	}
	if errs := valid.setDefaultsAndValidate(); len(errs) != 0 {
		t.Fatalf("valid config errors = %v", errs)
	}

	partialClient := valid
	partialClient.ClientID = "client"
	if errs := partialClient.setDefaultsAndValidate(); len(errs) == 0 || !strings.Contains(errs[0].Error(), "configured together") {
		t.Fatalf("partial OAuth client errors = %v", errs)
	}
}

func TestCloudflareOAuthConfigRejectsInsecureRemoteRedirect(t *testing.T) {
	t.Parallel()
	cfg := CloudflareOAuthConfig{
		Enabled: true, RedirectURL: "http://example.com/ui/",
	}
	if errs := cfg.setDefaultsAndValidate(); len(errs) == 0 || !strings.Contains(errs[0].Error(), "HTTPS") {
		t.Fatalf("redirect errors = %v", errs)
	}
}
