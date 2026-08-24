package config

import (
	"strings"
	"testing"
)

func TestManagedBackendsConfigRequiresStableEncryptionKey(t *testing.T) {
	t.Parallel()
	disabled := ManagedBackendsConfig{}
	if errs := disabled.setDefaultsAndValidate(); len(errs) != 0 || disabled.DefaultWarningPercent != 80 {
		t.Fatalf("disabled defaults = %+v, errors = %v", disabled, errs)
	}

	short := ManagedBackendsConfig{Enabled: true, MasterKey: "too-short"}
	if errs := short.setDefaultsAndValidate(); len(errs) != 1 || !strings.Contains(errs[0].Error(), "at least 32 bytes") {
		t.Fatalf("short-key errors = %v", errs)
	}

	valid := ManagedBackendsConfig{Enabled: true, MasterKey: strings.Repeat("k", 32), DefaultWarningPercent: 90}
	if errs := valid.setDefaultsAndValidate(); len(errs) != 0 {
		t.Fatalf("valid config errors = %v", errs)
	}
}

func TestNormalizeAndValidateBackendAcceptsAccountProxySchemes(t *testing.T) {
	t.Parallel()
	for _, proxyURL := range []string{
		"http://user:password@proxy.example:8080",
		"https://proxy.example:8443",
		"socks5://127.0.0.1:1080",
		"socks5h://proxy.example:1080",
	} {
		backend := BackendConfig{
			Name: "r2", Endpoint: "https://example.r2.cloudflarestorage.com",
			Bucket: "bucket", AccessKeyID: "key", SecretAccessKey: "secret", ProxyURL: proxyURL,
		}
		if err := NormalizeAndValidateBackend(&backend); err != nil {
			t.Errorf("proxy %q: %v", proxyURL, err)
		}
	}
}

func TestNormalizeAndValidateBackendRejectsUnsafeProxyScheme(t *testing.T) {
	t.Parallel()
	backend := BackendConfig{
		Name: "r2", Endpoint: "https://example.r2.cloudflarestorage.com",
		Bucket: "bucket", AccessKeyID: "key", SecretAccessKey: "secret",
		ProxyURL: "file:///etc/passwd",
	}
	if err := NormalizeAndValidateBackend(&backend); err == nil || !strings.Contains(err.Error(), "proxy_url") {
		t.Fatalf("unsafe proxy error = %v", err)
	}
}
