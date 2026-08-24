// -------------------------------------------------------------------------------
// Backend Configuration
//
// Author: Alex Freidah
//
// Defines BackendConfig - the per-backend YAML block describing the S3
// endpoint, credentials, optional quota and per-object size cap, and the
// read/write tunables (timeouts, max idle conns, signing tweaks) - plus
// its validators. Every backend listed in config.yaml is parsed into one
// of these structs and handed to the BackendManager at startup.
// -------------------------------------------------------------------------------

package config

import (
	"cmp"
	"errors"
	"fmt"
	"net/url"
)

// CredentialSourceStatic and friends enumerate the supported credential_source values.
const (
	// CredentialSourceStatic uses access_key_id / secret_access_key from the config (current default behaviour).
	CredentialSourceStatic = "static"
	// CredentialSourceDefaultChain resolves credentials via the AWS SDK default chain (env, IMDS, SSO, ~/.aws, STS).
	CredentialSourceDefaultChain = "default_chain"
)

// BackendConfig holds configuration for an S3-compatible storage backend.
type BackendConfig struct {
	Name            string `yaml:"name"`              // Identifier for metrics/tracing
	Provider        string `yaml:"provider"`          // Provider family, e.g. cloudflare_r2
	AccountID       string `yaml:"account_id"`        // Provider account identifier (non-secret)
	AccountLabel    string `yaml:"account_label"`     // Human-readable account label
	Endpoint        string `yaml:"endpoint"`          // S3-compatible endpoint URL
	Region          string `yaml:"region"`            // AWS region or equivalent
	Bucket          string `yaml:"bucket"`            // Target bucket name
	AccessKeyID     string `yaml:"access_key_id"`     // AWS access key ID (required when credential_source is "static")
	SecretAccessKey string `yaml:"secret_access_key"` // AWS secret access key (required when credential_source is "static")
	// CredentialSource selects how credentials are resolved: "static" (default, uses keys above) or "default_chain" (AWS SDK chain: env, IMDS, SSO, STS).
	CredentialSource   string `yaml:"credential_source"`
	ForcePathStyle     bool   `yaml:"force_path_style"`     // Use path-style URLs
	UnsignedPayload    *bool  `yaml:"unsigned_payload"`     // Skip SigV4 payload hash to stream uploads without buffering (default: true)
	DisableChecksum    bool   `yaml:"disable_checksum"`     // Disable SDK default checksums for GCS and other providers that reject them (default: false)
	StripSDKHeaders    bool   `yaml:"strip_sdk_headers"`    // Remove SDK v2 headers (amz-sdk-*, accept-encoding, x-id) before signing for GCS compatibility (default: false)
	QuotaBytes         int64  `yaml:"quota_bytes"`          // Maximum bytes allowed on this backend (0 = unlimited)
	MaxObjectSize      int64  `yaml:"max_object_size"`      // Maximum size of a single object in bytes (0 = unlimited)
	APIRequestLimit    int64  `yaml:"api_request_limit"`    // Monthly API request limit (0 = unlimited)
	EgressByteLimit    int64  `yaml:"egress_byte_limit"`    // Monthly egress byte limit (0 = unlimited)
	IngressByteLimit   int64  `yaml:"ingress_byte_limit"`   // Monthly ingress byte limit (0 = unlimited)
	QuotaGroup         string `yaml:"quota_group"`          // Shared provider/account quota group (defaults to backend name)
	RawLimitBytes      int64  `yaml:"raw_limit_bytes"`      // Provider hard/free-tier ceiling for the group
	StaticBufferBytes  int64  `yaml:"static_buffer_bytes"`  // Fixed safety reserve kept unavailable
	DynamicBufferBytes int64  `yaml:"dynamic_buffer_bytes"` // Drift/traffic reserve kept unavailable
	BillingPolicy      string `yaml:"billing_policy"`       // free_capped or metered
	ProxyURL           string `yaml:"proxy_url"`            // Optional account-scoped HTTP(S)/SOCKS5 egress proxy
	WarningPercent     int    `yaml:"warning_percent"`      // Capacity alert threshold (50-100, default 80)
}

// NormalizeAndValidateBackend applies the same defaults and validation used for
// YAML entries to one runtime-created backend definition.
func NormalizeAndValidateBackend(b *BackendConfig) error {
	return errors.Join(validateBackend(0, b, make(map[string]bool))...)
}

// validateBackends checks every BackendConfig in the configured list,
// fills missing names with backend-N defaults, and surfaces every
// per-entry problem in one pass so operators see all the typos at
// once rather than fixing one and rerunning.
func validateBackends(backends []BackendConfig) []error {
	var errs []error

	if len(backends) == 0 {
		errs = append(errs, ErrNoBackends)
	}

	seenNames := make(map[string]bool)
	for i := range backends {
		errs = append(errs, validateBackend(i, &backends[i], seenNames)...)
	}
	return errs
}

// validateBackend checks one entry in the backends list. seenNames is the
// caller's shared set used to flag name collisions across the full list.
// An empty Name is filled with a default so later messages and metrics can
// reference it unambiguously. An empty CredentialSource defaults to
// "static" so existing configs keep working without the new field.
func validateBackend(idx int, b *BackendConfig, seenNames map[string]bool) []error {
	prefix := fmt.Sprintf("backends[%d]", idx)
	if b.Name == "" {
		b.Name = fmt.Sprintf("backend-%d", idx)
	}
	b.CredentialSource = cmp.Or(b.CredentialSource, CredentialSourceStatic)
	b.QuotaGroup = cmp.Or(b.QuotaGroup, b.Name)
	b.BillingPolicy = cmp.Or(b.BillingPolicy, "free_capped")
	b.WarningPercent = cmp.Or(b.WarningPercent, 80)

	var errs []error
	if seenNames[b.Name] {
		errs = append(errs, prefixedDetail(prefix, ErrDuplicateBackend, fmt.Sprintf("%q", b.Name)))
	}
	seenNames[b.Name] = true

	errs = append(errs, requiredBackendStringErrs(prefix, b)...)
	errs = append(errs, credentialSourceErrs(prefix, b)...)
	errs = append(errs, nonNegativeBackendFieldErrs(prefix, b)...)
	if b.WarningPercent < 50 || b.WarningPercent > 100 {
		errs = append(errs, prefixed(prefix, fmt.Errorf("warning_percent must be between 50 and 100")))
	}
	if b.ProxyURL != "" {
		proxy, err := url.Parse(b.ProxyURL)
		if err != nil || proxy.Host == "" {
			errs = append(errs, prefixed(prefix, fmt.Errorf("proxy_url must be an absolute proxy URL")))
		} else if proxy.Scheme != "http" && proxy.Scheme != "https" && proxy.Scheme != "socks5" && proxy.Scheme != "socks5h" {
			errs = append(errs, prefixed(prefix, fmt.Errorf("proxy_url scheme must be http, https, socks5, or socks5h")))
		}
	}
	return errs
}

// requiredBackendStringErrs returns errors for the universally-required
// string fields (endpoint, bucket). Credential-related fields are
// handled by credentialSourceErrs since the requirement depends on the
// source.
func requiredBackendStringErrs(prefix string, b *BackendConfig) []error {
	var errs []error
	if b.Endpoint == "" {
		errs = append(errs, prefixed(prefix, ErrEndpointRequired))
	}
	if b.Bucket == "" {
		errs = append(errs, prefixed(prefix, ErrBackendBucketReqd))
	}
	return errs
}

// credentialSourceErrs enforces the per-source key requirements: static
// needs both keys; default_chain rejects them so a stale entry cannot
// silently shadow the SDK-resolved credentials.
func credentialSourceErrs(prefix string, b *BackendConfig) []error {
	var errs []error
	switch b.CredentialSource {
	case CredentialSourceStatic:
		if b.AccessKeyID == "" {
			errs = append(errs, prefixed(prefix, ErrAccessKeyIDReqd))
		}
		if b.SecretAccessKey == "" {
			errs = append(errs, prefixed(prefix, ErrSecretAccessKeyReqd))
		}
	case CredentialSourceDefaultChain:
		if b.AccessKeyID != "" || b.SecretAccessKey != "" {
			errs = append(errs, prefixed(prefix, ErrCredentialsWithDefaultChain))
		}
	default:
		errs = append(errs, prefixedDetail(prefix, ErrInvalidCredentialSource, fmt.Sprintf("got %q", b.CredentialSource)))
	}
	return errs
}

// nonNegativeBackendFieldErrs returns one error per quota/limit field that
// has gone negative.
func nonNegativeBackendFieldErrs(prefix string, b *BackendConfig) []error {
	var errs []error
	if b.QuotaBytes < 0 {
		errs = append(errs, prefixed(prefix, ErrNegativeQuota))
	}
	if b.MaxObjectSize < 0 {
		errs = append(errs, prefixed(prefix, ErrNegativeMaxObject))
	}
	if b.APIRequestLimit < 0 {
		errs = append(errs, prefixed(prefix, ErrNegativeAPILimit))
	}
	if b.EgressByteLimit < 0 {
		errs = append(errs, prefixed(prefix, ErrNegativeEgress))
	}
	if b.IngressByteLimit < 0 {
		errs = append(errs, prefixed(prefix, ErrNegativeIngress))
	}
	if b.RawLimitBytes < 0 || b.StaticBufferBytes < 0 || b.DynamicBufferBytes < 0 {
		errs = append(errs, prefixed(prefix, fmt.Errorf("quota group limits must not be negative")))
	}
	if b.RawLimitBytes > 0 && b.StaticBufferBytes+b.DynamicBufferBytes >= b.RawLimitBytes {
		errs = append(errs, prefixed(prefix, fmt.Errorf("quota group buffers must be smaller than raw_limit_bytes")))
	}
	if b.BillingPolicy != "free_capped" && b.BillingPolicy != "metered" {
		errs = append(errs, prefixed(prefix, fmt.Errorf("billing_policy must be free_capped or metered")))
	}
	return errs
}
