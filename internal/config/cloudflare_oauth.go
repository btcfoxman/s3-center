package config

import (
	"cmp"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const defaultCloudflareOAuthSessionTTL = 20 * time.Minute

var connectorSchemePattern = regexp.MustCompile(`^[a-z][a-z0-9+.-]{1,31}$`)

// CloudflareOAuthConfig enables browser-authorized Cloudflare R2 onboarding.
// The OAuth access token is kept in memory only for the short provisioning
// session; durable object access uses a generated bucket-scoped R2 token.
type CloudflareOAuthConfig struct {
	Enabled                 bool          `yaml:"enabled"`
	ClientID                string        `yaml:"client_id"`
	ClientSecret            string        `yaml:"client_secret"` //nolint:gosec // operator-provided OAuth secret
	RedirectURL             string        `yaml:"redirect_url"`
	Scopes                  []string      `yaml:"scopes"`
	SessionTTL              time.Duration `yaml:"session_ttl"`
	ConnectorScheme         string        `yaml:"connector_scheme"`
	TokenEndpointAuthMethod string        `yaml:"token_endpoint_auth_method"`
}

//nolint:gocognit // Keep all related operator-facing validation errors in a single aggregation pass.
func (c *CloudflareOAuthConfig) setDefaultsAndValidate() []error {
	c.SessionTTL = cmp.Or(c.SessionTTL, defaultCloudflareOAuthSessionTTL)
	c.ConnectorScheme = cmp.Or(strings.TrimSpace(c.ConnectorScheme), "s3center")
	c.TokenEndpointAuthMethod = cmp.Or(strings.TrimSpace(c.TokenEndpointAuthMethod), "client_secret_basic")
	for idx := range c.Scopes {
		c.Scopes[idx] = strings.TrimSpace(c.Scopes[idx])
	}

	var errs []error
	if !connectorSchemePattern.MatchString(c.ConnectorScheme) {
		errs = append(errs, fmt.Errorf("cloudflare_oauth.connector_scheme must be a valid lowercase URI scheme"))
	}
	if c.SessionTTL < 5*time.Minute || c.SessionTTL > time.Hour {
		errs = append(errs, fmt.Errorf("cloudflare_oauth.session_ttl must be between 5m and 1h"))
	}
	if c.TokenEndpointAuthMethod != "client_secret_basic" && c.TokenEndpointAuthMethod != "client_secret_post" {
		errs = append(errs, fmt.Errorf("cloudflare_oauth.token_endpoint_auth_method must be client_secret_basic or client_secret_post"))
	}
	if !c.Enabled {
		return errs
	}
	if strings.TrimSpace(c.ClientID) == "" {
		errs = append(errs, fmt.Errorf("cloudflare_oauth.client_id is required when OAuth onboarding is enabled"))
	}
	if strings.TrimSpace(c.ClientSecret) == "" {
		errs = append(errs, fmt.Errorf("cloudflare_oauth.client_secret is required when OAuth onboarding is enabled"))
	}
	if len(c.Scopes) == 0 {
		errs = append(errs, fmt.Errorf("cloudflare_oauth.scopes must include the scopes registered on the OAuth client"))
	} else {
		for _, scope := range c.Scopes {
			if scope == "" || strings.ContainsAny(scope, " \t\r\n") {
				errs = append(errs, fmt.Errorf("cloudflare_oauth.scopes entries must be non-empty scope identifiers"))
				break
			}
		}
	}
	redirect, err := url.Parse(c.RedirectURL)
	if err != nil || redirect.Scheme == "" || redirect.Host == "" {
		errs = append(errs, fmt.Errorf("cloudflare_oauth.redirect_url must be an absolute URL"))
	} else if redirect.Scheme != "https" && (redirect.Scheme != "http" || !isLoopbackHost(redirect.Hostname())) {
		errs = append(errs, fmt.Errorf("cloudflare_oauth.redirect_url must use HTTPS except on loopback development hosts"))
	}
	return errs
}

func isLoopbackHost(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}
