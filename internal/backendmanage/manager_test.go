package backendmanage

import (
	"bytes"
	"strings"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/config"
)

const testMasterKey = "0123456789abcdef0123456789abcdef"

func TestDefinitionCipherRoundTripAndAAD(t *testing.T) {
	t.Parallel()
	cipher, err := newDefinitionCipher(testMasterKey)
	if err != nil {
		t.Fatal(err)
	}
	definition, err := buildDefinition(CreateRequest{
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
	definition, err := buildDefinition(CreateRequest{
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

func TestViewRedactsProxyCredentials(t *testing.T) {
	t.Parallel()
	view := viewOf(Definition{Backend: config.BackendConfig{
		Name: "b", ProxyURL: "http://user:password@203.0.113.7:3128",
	}})
	if view.Proxy != "http://203.0.113.7:3128" {
		t.Fatalf("redacted proxy = %q", view.Proxy)
	}
}
