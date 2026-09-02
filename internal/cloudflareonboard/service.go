// Package cloudflareonboard implements short-lived Cloudflare OAuth sessions
// that discover R2 buckets and mint least-privilege S3 credentials.
package cloudflareonboard

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/backendmanage"
	"github.com/afreidah/s3-orchestrator/internal/config"
)

const (
	defaultAuthorizationURL = "https://dash.cloudflare.com/oauth2/auth"
	defaultTokenURL         = "https://dash.cloudflare.com/oauth2/token" //nolint:gosec // Public OAuth endpoint, not a credential.
	defaultAPIBaseURL       = "https://api.cloudflare.com/client/v4"
)

var profileIDPattern = regexp.MustCompile(`^cf-[a-f0-9]{16}$`)

// BackendManager is the runtime-backend surface needed after OAuth succeeds.
type BackendManager interface {
	CreateProvisioned(context.Context, *backendmanage.CreateRequest, backendmanage.Connection) (backendmanage.View, error)
	ConnectionForAccount(string) (backendmanage.AccountConnection, error)
	HasBackend(string) bool
}

// Options keeps external endpoints and time injectable for deterministic tests.
type Options struct {
	Config           config.CloudflareOAuthConfig
	Backends         BackendManager
	AuthorizationURL string
	TokenURL         string
	APIBaseURL       string
	Now              func() time.Time
	HTTPClient       func(proxyURL string) *http.Client
}

// Service owns in-memory, expiring onboarding sessions. OAuth credentials are
// never persisted: after provisioning, only the generated bucket R2 credential
// is retained by backendmanage in its encrypted definition.
type Service struct {
	cfg              config.CloudflareOAuthConfig
	backends         BackendManager
	authorizationURL string
	tokenURL         string
	apiBaseURL       string
	now              func() time.Time
	httpClient       func(string) *http.Client

	mu           sync.Mutex
	sessions     map[string]*session
	states       map[string]string
	tickets      map[string]string
	reportTokens map[string]string
}

type session struct {
	ID                string
	State             string
	Ticket            string
	ReportToken       string
	IdentityID        string
	ProfileID         string
	IdentityLabel     string
	ProxyURL          string
	ExpectedAccountID string
	CodeVerifier      string
	AuthorizationURL  string
	Status            string
	ExitIP            string
	LastError         string
	Accounts          []Account
	AccessToken       string
	CreatedAt         time.Time
	ExpiresAt         time.Time
	Result            *backendmanage.View
}

// StartRequest starts a new identity, or reauthorizes an existing account when
// AccountID is set. Existing account proxy credentials never round-trip through
// the browser-facing API.
type StartRequest struct {
	AccountID     string `json:"account_id"`
	IdentityLabel string `json:"identity_label"`
	ProxyURL      string `json:"proxy_url"`
}

// StartResult contains the browser URL plus an opaque, single-use connector
// ticket. The handler turns the ticket into a custom-protocol URL.
type StartResult struct {
	Session          SessionView `json:"session"`
	AuthorizationURL string      `json:"authorization_url"`
	ConnectorTicket  string      `json:"-"`
}

// ConnectorPayload is returned once to the local connector. Proxy credentials
// are intentionally present only on this no-store response.
type ConnectorPayload struct {
	ProfileID        string `json:"profile_id"`
	IdentityID       string `json:"identity_id"`
	IdentityLabel    string `json:"identity_label"`
	ProxyURL         string `json:"proxy_url"`
	AuthorizationURL string `json:"authorization_url"`
	ReportToken      string `json:"report_token"`
	ReportURL        string `json:"report_url,omitempty"`
}

// ConnectorReport is the narrow status surface accepted from the local agent.
type ConnectorReport struct {
	Token  string `json:"token"`
	Status string `json:"status"`
	ExitIP string `json:"exit_ip"`
	Error  string `json:"error"`
}

// Bucket is a selectable R2 bucket discovered with the OAuth access token.
type Bucket struct {
	Name         string `json:"name"`
	Jurisdiction string `json:"jurisdiction"`
	Location     string `json:"location,omitempty"`
	StorageClass string `json:"storage_class,omitempty"`
}

// Account is a Cloudflare account visible to the authorizing identity.
type Account struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Buckets []Bucket `json:"buckets"`
	Error   string   `json:"error,omitempty"`
}

// SessionView is safe for the authenticated administration UI.
type SessionView struct {
	ID            string              `json:"id"`
	Status        string              `json:"status"`
	IdentityID    string              `json:"identity_id"`
	ProfileID     string              `json:"profile_id"`
	IdentityLabel string              `json:"identity_label"`
	Proxy         string              `json:"proxy"`
	ExitIP        string              `json:"exit_ip,omitempty"`
	Error         string              `json:"error,omitempty"`
	Accounts      []Account           `json:"accounts,omitempty"`
	Result        *backendmanage.View `json:"result,omitempty"`
	ExpiresAt     time.Time           `json:"expires_at"`
}

// CompleteRequest chooses one discovered bucket and provisions it.
type CompleteRequest struct {
	SessionID      string `json:"session_id"`
	AccountID      string `json:"account_id"`
	Bucket         string `json:"bucket"`
	BackendName    string `json:"backend_name"`
	WarningPercent int    `json:"warning_percent"`
}

// New constructs an OAuth onboarding service.
func New(options *Options) (*Service, error) {
	if options == nil {
		return nil, errors.New("cloudflare onboarding options are required")
	}
	if options.Config.Enabled && options.Backends == nil {
		return nil, errors.New("cloudflare onboarding requires backend management")
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	httpClient := options.HTTPClient
	if httpClient == nil {
		httpClient = proxyHTTPClient
	}
	return &Service{
		cfg: options.Config, backends: options.Backends,
		authorizationURL: firstNonEmpty(options.AuthorizationURL, defaultAuthorizationURL),
		tokenURL:         firstNonEmpty(options.TokenURL, defaultTokenURL),
		apiBaseURL:       strings.TrimRight(firstNonEmpty(options.APIBaseURL, defaultAPIBaseURL), "/"),
		now:              now, httpClient: httpClient,
		sessions: make(map[string]*session), states: make(map[string]string),
		tickets: make(map[string]string), reportTokens: make(map[string]string),
	}, nil
}

func firstNonEmpty(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

// Enabled reports whether the server has a configured Cloudflare OAuth client.
func (s *Service) Enabled() bool { return s != nil && s.cfg.Enabled }

// ConnectorScheme returns the registered local URI scheme.
func (s *Service) ConnectorScheme() string { return s.cfg.ConnectorScheme }

// RedirectURL returns the trusted public callback URL used to derive connector
// endpoints without trusting an inbound Host header.
func (s *Service) RedirectURL() string { return s.cfg.RedirectURL }

// Start creates an expiring OAuth authorization session.
//
//nolint:gocognit // Session setup intentionally keeps validation and secret generation in one fail-closed transaction.
func (s *Service) Start(_ context.Context, request StartRequest) (StartResult, error) {
	if !s.Enabled() {
		return StartResult{}, errors.New("cloudflare OAuth onboarding is disabled")
	}
	now := s.now()
	s.mu.Lock()
	s.cleanupLocked(now)
	s.mu.Unlock()

	request.AccountID = strings.ToLower(strings.TrimSpace(request.AccountID))
	request.IdentityLabel = strings.TrimSpace(request.IdentityLabel)
	request.ProxyURL = normalizeProxyURL(request.ProxyURL)

	identityID, err := randomID("cfi-", 12)
	if err != nil {
		return StartResult{}, err
	}
	profileID, err := randomID("cf-", 8)
	if err != nil {
		return StartResult{}, err
	}
	expectedAccountID := ""
	if request.AccountID != "" {
		existing, lookupErr := s.backends.ConnectionForAccount(request.AccountID)
		if lookupErr != nil {
			return StartResult{}, lookupErr
		}
		expectedAccountID = existing.AccountID
		request.IdentityLabel = firstNonEmpty(request.IdentityLabel, existing.AccountLabel)
		request.ProxyURL = existing.ProxyURL
		if existing.Connection != nil {
			identityID = existing.Connection.IdentityID
			profileID = existing.Connection.ProfileID
		}
	}
	if request.IdentityLabel == "" {
		request.IdentityLabel = "Cloudflare R2 identity"
	}
	if len(request.IdentityLabel) > 80 {
		return StartResult{}, errors.New("identity label must not exceed 80 characters")
	}
	if err := validateProxyURL(request.ProxyURL); err != nil {
		return StartResult{}, err
	}
	if !profileIDPattern.MatchString(profileID) {
		return StartResult{}, errors.New("invalid browser profile identifier")
	}

	id, err := randomID("cfs-", 16)
	if err != nil {
		return StartResult{}, err
	}
	state, err := randomToken(32)
	if err != nil {
		return StartResult{}, err
	}
	ticket, err := randomToken(32)
	if err != nil {
		return StartResult{}, err
	}
	reportToken, err := randomToken(32)
	if err != nil {
		return StartResult{}, err
	}
	verifier, err := randomToken(48)
	if err != nil {
		return StartResult{}, err
	}
	authorizationURL, err := s.buildAuthorizationURL(state, verifier)
	if err != nil {
		return StartResult{}, err
	}
	item := &session{
		ID: id, State: state, Ticket: ticket, ReportToken: reportToken,
		IdentityID: identityID, ProfileID: profileID, IdentityLabel: request.IdentityLabel,
		ProxyURL: request.ProxyURL, ExpectedAccountID: expectedAccountID,
		CodeVerifier: verifier, AuthorizationURL: authorizationURL,
		Status: "waiting_connector", CreatedAt: now, ExpiresAt: now.Add(s.cfg.SessionTTL),
	}
	s.mu.Lock()
	s.sessions[id] = item
	s.states[state] = id
	s.tickets[ticket] = id
	s.reportTokens[reportToken] = id
	view := viewOfSession(item)
	s.mu.Unlock()
	return StartResult{Session: view, AuthorizationURL: authorizationURL, ConnectorTicket: ticket}, nil
}

func (s *Service) buildAuthorizationURL(state, verifier string) (string, error) {
	authorize, err := url.Parse(s.authorizationURL)
	if err != nil {
		return "", fmt.Errorf("parse Cloudflare authorization URL: %w", err)
	}
	challenge := sha256.Sum256([]byte(verifier))
	query := authorize.Query()
	query.Set("client_id", s.cfg.ClientID)
	query.Set("redirect_uri", s.cfg.RedirectURL)
	query.Set("response_type", "code")
	query.Set("scope", strings.Join(s.cfg.Scopes, " "))
	query.Set("state", state)
	query.Set("code_challenge_method", "S256")
	query.Set("code_challenge", base64.RawURLEncoding.EncodeToString(challenge[:]))
	authorize.RawQuery = query.Encode()
	return authorize.String(), nil
}

// ConsumeConnectorTicket returns a connector launch payload exactly once.
func (s *Service) ConsumeConnectorTicket(ticket string) (ConnectorPayload, error) {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupLocked(now)
	id, ok := s.tickets[ticket]
	if !ok {
		return ConnectorPayload{}, errors.New("connector ticket is invalid, expired, or already used")
	}
	delete(s.tickets, ticket)
	item := s.sessions[id]
	if item == nil {
		return ConnectorPayload{}, errors.New("connector session no longer exists")
	}
	item.Status = "opening_browser"
	return ConnectorPayload{
		ProfileID: item.ProfileID, IdentityID: item.IdentityID, IdentityLabel: item.IdentityLabel,
		ProxyURL: item.ProxyURL, AuthorizationURL: item.AuthorizationURL, ReportToken: item.ReportToken,
	}, nil
}

// ReportConnector records fail-closed proxy validation and browser startup.
func (s *Service) ReportConnector(report ConnectorReport) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupLocked(s.now())
	id, ok := s.reportTokens[report.Token]
	if !ok {
		return errors.New("connector report token is invalid or expired")
	}
	item := s.sessions[id]
	if item == nil {
		return errors.New("connector session no longer exists")
	}
	if item.Status != "waiting_connector" && item.Status != "opening_browser" && item.Status != "waiting_authorization" {
		return errors.New("connector session no longer accepts browser reports")
	}
	if report.Error != "" {
		item.Status = "error"
		item.LastError = truncate(strings.TrimSpace(report.Error), 300)
		return nil
	}
	switch report.Status {
	case "proxy_verified", "browser_open":
		item.Status = "waiting_authorization"
	default:
		return errors.New("unsupported connector status")
	}
	if report.ExitIP != "" {
		if net.ParseIP(strings.TrimSpace(report.ExitIP)) == nil {
			return errors.New("connector exit_ip is invalid")
		}
		item.ExitIP = strings.TrimSpace(report.ExitIP)
	}
	return nil
}

// Status returns the safe view polled by the administration UI.
func (s *Service) Status(id string) (SessionView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupLocked(s.now())
	item := s.sessions[id]
	if item == nil {
		return SessionView{}, errors.New("onboarding session is invalid or expired")
	}
	return viewOfSession(item), nil
}

// Authorize exchanges the callback code and discovers accessible accounts and
// buckets through the exact proxy bound to the browser profile.
//
//nolint:gocognit // The callback is a linear state machine whose guarded transitions must remain visible together.
func (s *Service) Authorize(ctx context.Context, state, code, oauthError string) error {
	state = strings.TrimSpace(state)
	s.mu.Lock()
	s.cleanupLocked(s.now())
	id := s.states[state]
	item := s.sessions[id]
	if item == nil {
		s.mu.Unlock()
		return errors.New("oauth state is invalid or expired")
	}
	// Authorization codes and state values are single use. A callback retry gets
	// a fresh onboarding session instead of replaying the same grant.
	delete(s.states, state)
	if oauthError != "" {
		item.Status = "error"
		item.LastError = "Cloudflare authorization was declined: " + truncate(oauthError, 180)
		s.mu.Unlock()
		return errors.New(item.LastError)
	}
	if strings.TrimSpace(code) == "" {
		item.Status = "error"
		item.LastError = "Cloudflare callback did not include an authorization code"
		s.mu.Unlock()
		return errors.New(item.LastError)
	}
	proxyURL := item.ProxyURL
	verifier := item.CodeVerifier
	expectedAccountID := item.ExpectedAccountID
	item.Status = "discovering"
	s.mu.Unlock()

	client := s.httpClient(proxyURL)
	accessToken, err := s.exchangeCode(ctx, client, code, verifier)
	if err == nil {
		var accounts []Account
		accounts, err = s.discoverAccounts(ctx, client, accessToken)
		if err == nil && expectedAccountID != "" {
			accounts = slices.DeleteFunc(accounts, func(account Account) bool { return account.ID != expectedAccountID })
			if len(accounts) == 0 {
				err = errors.New("the isolated Cloudflare profile is not authorized for the expected account")
			}
		}
		if err == nil && len(accounts) == 0 {
			err = errors.New("cloudflare returned no accessible accounts")
		}
		if err == nil {
			s.mu.Lock()
			if current := s.sessions[id]; current != nil {
				current.AccessToken = accessToken
				current.Accounts = accounts
				current.Status = "authorized"
				current.LastError = ""
				current.ExpiresAt = s.now().Add(s.cfg.SessionTTL)
				delete(s.reportTokens, current.ReportToken)
			}
			s.mu.Unlock()
			return nil
		}
	}
	err = redactProxyError(err, proxyURL)
	s.mu.Lock()
	if current := s.sessions[id]; current != nil {
		current.Status = "error"
		current.LastError = truncate(err.Error(), 300)
		current.AccessToken = ""
		delete(s.reportTokens, current.ReportToken)
	}
	s.mu.Unlock()
	return err
}

// Complete mints one bucket-scoped account token and hot-adds the R2 backend.
func (s *Service) Complete(ctx context.Context, request CompleteRequest) (backendmanage.View, error) {
	if !s.Enabled() {
		return backendmanage.View{}, errors.New("cloudflare OAuth onboarding is disabled")
	}
	s.mu.Lock()
	s.cleanupLocked(s.now())
	item := s.sessions[strings.TrimSpace(request.SessionID)]
	if item == nil {
		s.mu.Unlock()
		return backendmanage.View{}, errors.New("onboarding session is invalid or expired")
	}
	if item.Status != "authorized" || item.AccessToken == "" {
		s.mu.Unlock()
		return backendmanage.View{}, errors.New("cloudflare authorization is not ready")
	}
	account, bucket, ok := selectedBucket(item.Accounts, request.AccountID, request.Bucket)
	if !ok {
		s.mu.Unlock()
		return backendmanage.View{}, errors.New("selected Cloudflare account or R2 bucket was not discovered in this session")
	}
	accessToken := item.AccessToken
	proxyURL := item.ProxyURL
	identityID, profileID := item.IdentityID, item.ProfileID
	item.Status = "provisioning"
	item.LastError = ""
	s.mu.Unlock()

	backendName := strings.TrimSpace(request.BackendName)
	if backendName == "" {
		backendName = s.uniqueBackendName(account.ID, bucket.Name)
	}
	client := s.httpClient(proxyURL)
	accessKeyID, secretAccessKey, err := s.createBucketToken(ctx, client, accessToken, account, bucket)
	if err == nil {
		view, createErr := s.backends.CreateProvisioned(ctx, &backendmanage.CreateRequest{
			Name: backendName, AccountLabel: account.Name, AccountID: account.ID,
			Jurisdiction: bucket.Jurisdiction, Bucket: bucket.Name,
			AccessKeyID: accessKeyID, SecretAccessKey: secretAccessKey,
			ProxyURL: proxyURL, WarningPercent: request.WarningPercent,
		}, backendmanage.Connection{
			IdentityID: identityID, ProfileID: profileID,
			AuthMode: "cloudflare_oauth", AuthorizedAt: s.now().UTC(),
		})
		if createErr == nil {
			s.mu.Lock()
			if current := s.sessions[item.ID]; current != nil {
				current.Status = "complete"
				current.AccessToken = ""
				current.CodeVerifier = ""
				copyOfView := view
				current.Result = &copyOfView
				delete(s.tickets, current.Ticket)
				delete(s.reportTokens, current.ReportToken)
			}
			s.mu.Unlock()
			return view, nil
		}
		revokeErr := s.deleteAccountToken(ctx, client, accessToken, account.ID, accessKeyID)
		err = createErr
		if revokeErr != nil {
			err = errors.Join(err, fmt.Errorf("revoke unused R2 token: %w", revokeErr))
		}
	}
	err = redactProxyError(err, proxyURL)
	s.mu.Lock()
	if current := s.sessions[item.ID]; current != nil {
		current.Status = "authorized"
		current.LastError = truncate(err.Error(), 300)
	}
	s.mu.Unlock()
	return backendmanage.View{}, err
}

func selectedBucket(accounts []Account, accountID, bucketName string) (Account, Bucket, bool) {
	accountID = strings.ToLower(strings.TrimSpace(accountID))
	bucketName = strings.TrimSpace(bucketName)
	for _, account := range accounts {
		if account.ID != accountID {
			continue
		}
		for _, bucket := range account.Buckets {
			if bucket.Name == bucketName {
				return account, bucket, true
			}
		}
	}
	return Account{}, Bucket{}, false
}

func (s *Service) uniqueBackendName(accountID, bucket string) string {
	base := "r2-" + accountID[:min(6, len(accountID))] + "-" + strings.ToLower(bucket)
	base = sanitizeBackendName(base)
	if len(base) > 63 {
		base = strings.TrimRight(base[:63], "-._")
	}
	if !s.backends.HasBackend(base) {
		return base
	}
	suffix, err := randomID("-", 3)
	if err != nil {
		suffix = fmt.Sprintf("-%d", s.now().Unix()%1_000_000)
	}
	if len(base)+len(suffix) > 63 {
		base = strings.TrimRight(base[:63-len(suffix)], "-._")
	}
	return base + suffix
}

func sanitizeBackendName(value string) string {
	var builder strings.Builder
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || char == '-' || char == '.' || char == '_' {
			builder.WriteRune(char)
		} else {
			builder.WriteByte('-')
		}
	}
	return strings.Trim(builder.String(), "-._")
}

func viewOfSession(item *session) SessionView {
	accounts := make([]Account, len(item.Accounts))
	for idx := range item.Accounts {
		accounts[idx] = item.Accounts[idx]
		accounts[idx].Buckets = slices.Clone(item.Accounts[idx].Buckets)
	}
	var result *backendmanage.View
	if item.Result != nil {
		copyOfResult := *item.Result
		result = &copyOfResult
	}
	return SessionView{
		ID: item.ID, Status: item.Status, IdentityID: item.IdentityID,
		ProfileID: item.ProfileID, IdentityLabel: item.IdentityLabel,
		Proxy: displayProxy(item.ProxyURL), ExitIP: item.ExitIP, Error: item.LastError,
		Accounts: accounts, Result: result, ExpiresAt: item.ExpiresAt,
	}
}

func (s *Service) cleanupLocked(now time.Time) {
	for id, item := range s.sessions {
		if now.Before(item.ExpiresAt) {
			continue
		}
		delete(s.sessions, id)
		delete(s.states, item.State)
		delete(s.tickets, item.Ticket)
		delete(s.reportTokens, item.ReportToken)
	}
}

func normalizeProxyURL(raw string) string {
	value := strings.TrimSpace(raw)
	if value != "" && !strings.Contains(value, "://") {
		value = "http://" + value
	}
	return value
}

func validateProxyURL(raw string) error {
	if raw == "" {
		return nil
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return errors.New("proxy URL must be an absolute URL")
	}
	switch parsed.Scheme {
	case "http", "https", "socks5", "socks5h":
		return nil
	default:
		return errors.New("proxy URL scheme must be http, https, socks5, or socks5h")
	}
}

func displayProxy(raw string) string {
	if raw == "" {
		return "Direct"
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "Proxy configured"
	}
	return parsed.Scheme + "://" + parsed.Host
}

func proxyHTTPClient(proxyURL string) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = 20
	transport.MaxIdleConnsPerHost = 10
	transport.IdleConnTimeout = 60 * time.Second
	transport.ResponseHeaderTimeout = 30 * time.Second
	if proxyURL != "" {
		parsed, _ := url.Parse(proxyURL)
		transport.Proxy = http.ProxyURL(parsed)
	}
	return &http.Client{Transport: transport, Timeout: 45 * time.Second}
}

func redactProxyError(err error, proxyURL string) error {
	if err == nil || proxyURL == "" {
		return err
	}
	message := strings.ReplaceAll(err.Error(), proxyURL, displayProxy(proxyURL))
	if parsed, parseErr := url.Parse(proxyURL); parseErr == nil && parsed.User != nil {
		message = strings.ReplaceAll(message, parsed.User.String()+"@", "***@")
	}
	return errors.New(message)
}

func randomID(prefix string, byteCount int) (string, error) {
	raw := make([]byte, byteCount)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("create onboarding identifier: %w", err)
	}
	return prefix + hex.EncodeToString(raw), nil
}

func randomToken(byteCount int) (string, error) {
	raw := make([]byte, byteCount)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("create onboarding token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func truncate(value string, maxLength int) string {
	if len(value) <= maxLength {
		return value
	}
	return value[:maxLength] + "..."
}
