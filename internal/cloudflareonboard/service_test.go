package cloudflareonboard

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/backendmanage"
	"github.com/afreidah/s3-orchestrator/internal/config"
)

const (
	testAccountID       = "0123456789abcdef0123456789abcdef"
	testTokenID         = "abcdef0123456789abcdef0123456789"
	testBootstrapID     = "11111111111111111111111111111111"
	testBootstrapToken  = "cfat_test_bootstrap_token_0123456789" //nolint:gosec // Synthetic test credential.
	testOAuthToken      = "cfoat_not_an_api_token_0123456789"    //nolint:gosec // Synthetic negative-test credential.
	testGeneratedSecret = "one-time-token-value"
)

type fakeBackends struct {
	mu          sync.Mutex
	request     *backendmanage.CreateRequest
	connection  backendmanage.Connection
	createErr   error
	createErrs  []error
	createCalls int
	existing    backendmanage.AccountConnection
}

func (f *fakeBackends) CreateProvisioned(_ context.Context, request *backendmanage.CreateRequest, connection backendmanage.Connection) (backendmanage.View, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createCalls++
	copyOfRequest := *request
	f.request, f.connection = &copyOfRequest, connection
	if len(f.createErrs) > 0 {
		err := f.createErrs[0]
		f.createErrs = f.createErrs[1:]
		if err != nil {
			return backendmanage.View{}, err
		}
	}
	if f.createErr != nil {
		return backendmanage.View{}, f.createErr
	}
	return backendmanage.View{Name: request.Name, AccountID: request.AccountID, Bucket: request.Bucket, ProfileID: connection.ProfileID, AuthMode: connection.AuthMode}, nil
}

func (f *fakeBackends) ConnectionForAccount(accountID string) (backendmanage.AccountConnection, error) {
	if f.existing.AccountID == accountID {
		return f.existing, nil
	}
	return backendmanage.AccountConnection{}, errors.New("account not found")
}

func (f *fakeBackends) HasBackend(string) bool { return false }

type fakeHTTPStatusError struct{ status int }

func (e fakeHTTPStatusError) Error() string       { return http.StatusText(e.status) }
func (e fakeHTTPStatusError) HTTPStatusCode() int { return e.status }

type cloudflareFixture struct {
	server     *httptest.Server
	mu         sync.Mutex
	createBody map[string]any
	deletedIDs map[string]bool
}

//nolint:gocognit,gosec // The local fake intentionally exercises the complete bootstrap-token flow.
func newCloudflareFixture(t *testing.T) *cloudflareFixture {
	t.Helper()
	fixture := &cloudflareFixture{deletedIDs: make(map[string]bool)}
	fixture.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/accounts/"+testAccountID:
			assertBearer(t, r)
			writeFixtureJSON(t, w, apiEnvelopeBody(map[string]string{"id": testAccountID, "name": "Example Account"}, map[string]any{}))
		case r.URL.Path == "/api/accounts/"+testAccountID+"/r2/buckets":
			assertBearer(t, r)
			writeFixtureJSON(t, w, apiEnvelopeBody(map[string]any{"buckets": []map[string]string{{"name": "media-assets", "jurisdiction": "default"}}}, map[string]any{}))
		case r.URL.Path == "/api/accounts/"+testAccountID+"/tokens/verify":
			assertBearer(t, r)
			writeFixtureJSON(t, w, apiEnvelopeBody(map[string]string{"id": testBootstrapID, "status": "active"}, map[string]any{}))
		case r.URL.Path == "/api/accounts/"+testAccountID+"/tokens/permission_groups":
			assertBearer(t, r)
			if r.URL.Query().Get("scope") != "com.cloudflare.edge.r2.bucket" {
				t.Errorf("unexpected permission query: %s", r.URL.RawQuery)
			}
			writeFixtureJSON(t, w, apiEnvelopeBody([]map[string]string{{"id": "permission-id", "name": "Workers R2 Storage Bucket Item Write"}}, map[string]any{}))
		case r.URL.Path == "/api/accounts/"+testAccountID+"/tokens" && r.Method == http.MethodPost:
			assertBearer(t, r)
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			if err := json.NewDecoder(r.Body).Decode(&fixture.createBody); err != nil {
				t.Errorf("decode create body: %v", err)
			}
			policy := fixture.createBody["policies"].([]any)[0].(map[string]any)
			if _, exists := policy["id"]; exists {
				w.WriteHeader(http.StatusBadRequest)
				writeFixtureJSON(t, w, map[string]any{
					"success": false,
					"errors":  []map[string]any{{"code": 1000, "message": "Reusing existing policies is not supported by token creation"}},
				})
				return
			}
			writeFixtureJSON(t, w, apiEnvelopeBody(map[string]string{"id": testTokenID, "value": testGeneratedSecret}, map[string]any{}))
		case strings.HasPrefix(r.URL.Path, "/api/accounts/"+testAccountID+"/tokens/") && r.Method == http.MethodDelete:
			assertBearer(t, r)
			deletedID := strings.TrimPrefix(r.URL.Path, "/api/accounts/"+testAccountID+"/tokens/")
			fixture.mu.Lock()
			fixture.deletedIDs[deletedID] = true
			fixture.mu.Unlock()
			writeFixtureJSON(t, w, apiEnvelopeBody(map[string]string{"id": deletedID}, map[string]any{}))
		default:
			http.Error(w, `{"success":false}`, http.StatusNotFound)
		}
	}))
	t.Cleanup(fixture.server.Close)
	return fixture
}

//nolint:gocognit,gosec // End-to-end assertions keep the complete secret-boundary flow together with fake credentials.
func TestBrowserOnboardingEndToEndAndSecretBoundaries(t *testing.T) {
	t.Parallel()
	fixture := newCloudflareFixture(t)
	backends := &fakeBackends{}
	var proxies []string
	service := newTestService(t, fixture, backends, func(proxy string) *http.Client {
		proxies = append(proxies, proxy)
		return fixture.server.Client()
	})

	started, err := service.Start(context.Background(), StartRequest{
		IdentityLabel: "Account via Tokyo", ProxyURL: "http://proxy-user:proxy-password@203.0.113.7:3128",
	})
	if err != nil {
		t.Fatal(err)
	}
	authorization, err := url.Parse(started.AuthorizationURL)
	if err != nil {
		t.Fatal(err)
	}
	if authorization.Query().Get("to") != "/:account/api-tokens" || authorization.Query().Get("name") != bootstrapCredentialName || !strings.Contains(authorization.Query().Get("permissionGroupKeys"), "account_api_tokens") {
		t.Fatalf("unexpected guided token URL: %s", started.AuthorizationURL)
	}
	encoded, _ := json.Marshal(started)
	if strings.Contains(string(encoded), "proxy-password") || strings.Contains(string(encoded), started.ConnectorTicket) {
		t.Fatal("browser-facing start result exposed a connector secret")
	}

	payload, err := service.ConsumeConnectorTicket(started.ConnectorTicket)
	if err != nil {
		t.Fatal(err)
	}
	if payload.ProxyURL != "http://proxy-user:proxy-password@203.0.113.7:3128" || payload.ReportToken == "" {
		t.Fatalf("connector did not receive its private payload: %+v", payload)
	}
	if _, err := service.ConsumeConnectorTicket(started.ConnectorTicket); err == nil {
		t.Fatal("connector ticket was reusable")
	}
	if err := service.ReportConnector(context.Background(), &ConnectorReport{Token: payload.ReportToken, ConnectorVersion: ConnectorProtocolVersion, Status: "proxy_verified", ExitIP: "203.0.113.9"}); err != nil {
		t.Fatal(err)
	}
	if err := service.ReportConnector(context.Background(), &ConnectorReport{Token: payload.ReportToken, ConnectorVersion: ConnectorProtocolVersion, Status: "bootstrap_token", AccountID: testAccountID, BootstrapToken: testBootstrapToken}); err != nil {
		t.Fatal(err)
	}
	view, err := service.Status(started.Session.ID)
	if err != nil {
		t.Fatal(err)
	}
	safeView, _ := json.Marshal(view)
	if view.Status != "authorized" || len(view.Accounts) != 1 || len(view.Accounts[0].Buckets) != 1 || strings.Contains(view.Proxy, "proxy-user") || strings.Contains(string(safeView), testBootstrapToken) {
		t.Fatalf("unexpected safe session view: %+v", view)
	}

	created, err := service.Complete(context.Background(), CompleteRequest{
		SessionID: started.Session.ID, AccountID: testAccountID, Bucket: "media-assets", BackendName: "r2-browser-test", WarningPercent: 83,
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.AuthMode != "cloudflare_connector" || backends.connection.ProfileID != started.Session.ProfileID {
		t.Fatalf("connection metadata not preserved: %+v %+v", created, backends.connection)
	}
	wantHash := sha256.Sum256([]byte(testGeneratedSecret))
	if backends.request.AccessKeyID != testTokenID || backends.request.SecretAccessKey != hex.EncodeToString(wantHash[:]) || backends.request.ProxyURL != "http://proxy-user:proxy-password@203.0.113.7:3128" {
		t.Fatalf("incorrect durable R2 credentials: %+v", backends.request)
	}
	if len(proxies) != 2 || proxies[0] != backends.request.ProxyURL || proxies[1] != backends.request.ProxyURL {
		t.Fatalf("browser bootstrap and provisioning did not share the bound proxy: %v", proxies)
	}
	fixture.mu.Lock()
	policy := fixture.createBody["policies"].([]any)[0].(map[string]any)
	resource := policy["resources"].(map[string]any)
	_, reusesPolicy := policy["id"]
	permission := policy["permission_groups"].([]any)[0].(map[string]any)
	_, sendsPermissionName := permission["name"]
	bootstrapRevoked := fixture.deletedIDs[testBootstrapID]
	fixture.mu.Unlock()
	wantResource := "com.cloudflare.edge.r2.bucket." + testAccountID + "_default_media-assets"
	if resource[wantResource] != "*" || reusesPolicy || sendsPermissionName || !bootstrapRevoked {
		t.Fatalf("bucket token policy = %#v, bootstrap revoked = %v", policy, bootstrapRevoked)
	}
}

func TestProvisionRetriesWhileNewR2CredentialPropagates(t *testing.T) {
	t.Parallel()
	fixture := newCloudflareFixture(t)
	unauthorized := errors.Join(
		errors.New("check bucket access failed"),
		fakeHTTPStatusError{status: http.StatusUnauthorized},
	)
	backends := &fakeBackends{createErrs: []error{unauthorized, unauthorized}}
	service := newTestService(t, fixture, backends, func(string) *http.Client { return fixture.server.Client() })
	service.activationTimeout = 100 * time.Millisecond
	service.activationDelay = time.Millisecond
	started, _ := startAndAuthorizeConnector(t, service)

	created, err := service.Complete(context.Background(), CompleteRequest{
		SessionID: started.Session.ID, AccountID: testAccountID, Bucket: "media-assets", BackendName: "r2-eventual-iam",
	})
	if err != nil {
		t.Fatal(err)
	}
	backends.mu.Lock()
	createCalls := backends.createCalls
	backends.mu.Unlock()
	if created.Name != "r2-eventual-iam" || createCalls != 3 {
		t.Fatalf("created backend/calls = %q/%d, want r2-eventual-iam/3", created.Name, createCalls)
	}
	fixture.mu.Lock()
	generatedRevoked := fixture.deletedIDs[testTokenID]
	bootstrapRevoked := fixture.deletedIDs[testBootstrapID]
	fixture.mu.Unlock()
	if generatedRevoked || !bootstrapRevoked {
		t.Fatalf("generated token revoked=%v bootstrap revoked=%v", generatedRevoked, bootstrapRevoked)
	}
}

func TestProvisionRevokesGeneratedTokenWhenCredentialNeverActivates(t *testing.T) {
	t.Parallel()
	fixture := newCloudflareFixture(t)
	backends := &fakeBackends{createErr: fakeHTTPStatusError{status: http.StatusUnauthorized}}
	service := newTestService(t, fixture, backends, func(string) *http.Client { return fixture.server.Client() })
	service.activationTimeout = 20 * time.Millisecond
	service.activationDelay = time.Millisecond
	started, _ := startAndAuthorizeConnector(t, service)

	_, err := service.Complete(context.Background(), CompleteRequest{
		SessionID: started.Session.ID, AccountID: testAccountID, Bucket: "media-assets",
	})
	if err == nil || !strings.Contains(err.Error(), "IAM propagation") {
		t.Fatalf("activation timeout error = %v", err)
	}
	fixture.mu.Lock()
	generatedRevoked := fixture.deletedIDs[testTokenID]
	bootstrapRevoked := fixture.deletedIDs[testBootstrapID]
	fixture.mu.Unlock()
	if !generatedRevoked || bootstrapRevoked {
		t.Fatalf("generated token revoked=%v bootstrap revoked=%v", generatedRevoked, bootstrapRevoked)
	}
}

func TestProvisionFailureRevokesGeneratedTokenButKeepsBootstrapForRetry(t *testing.T) {
	t.Parallel()
	fixture := newCloudflareFixture(t)
	backends := &fakeBackends{createErr: errors.New("probe failed")}
	service := newTestService(t, fixture, backends, func(string) *http.Client { return fixture.server.Client() })
	started, _ := startAndAuthorizeConnector(t, service)
	_, err := service.Complete(context.Background(), CompleteRequest{SessionID: started.Session.ID, AccountID: testAccountID, Bucket: "media-assets"})
	if err == nil || !strings.Contains(err.Error(), "probe failed") {
		t.Fatalf("expected backend failure, got %v", err)
	}
	if backends.createCalls != 1 {
		t.Fatalf("non-authentication failure retried %d times", backends.createCalls)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if !fixture.deletedIDs[testTokenID] || fixture.deletedIDs[testBootstrapID] {
		t.Fatalf("generated token deleted=%v bootstrap deleted=%v", fixture.deletedIDs[testTokenID], fixture.deletedIDs[testBootstrapID])
	}
}

func TestConnectorRejectsOAuthAccessToken(t *testing.T) {
	t.Parallel()
	fixture := newCloudflareFixture(t)
	service := newTestService(t, fixture, &fakeBackends{}, func(string) *http.Client { return fixture.server.Client() })
	started, _ := service.Start(context.Background(), StartRequest{IdentityLabel: "invalid token"})
	payload, _ := service.ConsumeConnectorTicket(started.ConnectorTicket)
	err := service.ReportConnector(context.Background(), &ConnectorReport{Token: payload.ReportToken, ConnectorVersion: ConnectorProtocolVersion, Status: "bootstrap_token", AccountID: testAccountID, BootstrapToken: testOAuthToken})
	if err == nil || !strings.Contains(err.Error(), "cannot mint") {
		t.Fatalf("OAuth token error = %v", err)
	}
}

func TestConnectorRejectsLegacyAutomatedBrowserVersion(t *testing.T) {
	t.Parallel()
	fixture := newCloudflareFixture(t)
	service := newTestService(t, fixture, &fakeBackends{}, func(string) *http.Client { return fixture.server.Client() })
	started, err := service.Start(context.Background(), StartRequest{IdentityLabel: "legacy connector"})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := service.ConsumeConnectorTicket(started.ConnectorTicket)
	if err != nil {
		t.Fatal(err)
	}
	err = service.ReportConnector(context.Background(), &ConnectorReport{
		Token: payload.ReportToken, Status: "proxy_verified", ExitIP: "203.0.113.9",
	})
	if err == nil || !strings.Contains(err.Error(), "Connector 2.1") {
		t.Fatalf("legacy connector error = %v", err)
	}
	view, statusErr := service.Status(started.Session.ID)
	if statusErr != nil {
		t.Fatal(statusErr)
	}
	if view.Status != "error" || !strings.Contains(view.Error, "Connector 2.1") {
		t.Fatalf("legacy connector session = %+v", view)
	}
}

func TestSessionExpiryAndExistingProfileReuse(t *testing.T) {
	t.Parallel()
	fixture := newCloudflareFixture(t)
	now := time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)
	connection := &backendmanage.Connection{IdentityID: "cfi-0123456789abcdef01234567", ProfileID: "cf-0123456789abcdef", AuthMode: "cloudflare_connector"}
	backends := &fakeBackends{existing: backendmanage.AccountConnection{
		AccountID: testAccountID, AccountLabel: "Existing", ProxyURL: "socks5://user:secret@127.0.0.1:1080", Connection: connection,
	}}
	service := newTestService(t, fixture, backends, func(string) *http.Client { return fixture.server.Client() })
	service.now = func() time.Time { return now }
	started, err := service.Start(context.Background(), StartRequest{AccountID: testAccountID})
	if err != nil {
		t.Fatal(err)
	}
	authorization, _ := url.Parse(started.AuthorizationURL)
	if started.Session.ProfileID != connection.ProfileID || started.Session.IdentityID != connection.IdentityID || strings.Contains(started.Session.Proxy, "secret") || authorization.Query().Get("to") != "/"+testAccountID+"/api-tokens" {
		t.Fatalf("existing identity was not safely reused: %+v", started.Session)
	}
	now = now.Add(21 * time.Minute)
	if _, err := service.Status(started.Session.ID); err == nil {
		t.Fatal("expired session remained available")
	}
}

func startAndAuthorizeConnector(t *testing.T, service *Service) (StartResult, ConnectorPayload) {
	t.Helper()
	started, err := service.Start(context.Background(), StartRequest{IdentityLabel: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := service.ConsumeConnectorTicket(started.ConnectorTicket)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.ReportConnector(context.Background(), &ConnectorReport{Token: payload.ReportToken, ConnectorVersion: ConnectorProtocolVersion, Status: "proxy_verified", ExitIP: "203.0.113.9"}); err != nil {
		t.Fatal(err)
	}
	if err := service.ReportConnector(context.Background(), &ConnectorReport{Token: payload.ReportToken, ConnectorVersion: ConnectorProtocolVersion, Status: "bootstrap_token", AccountID: testAccountID, BootstrapToken: testBootstrapToken}); err != nil {
		t.Fatal(err)
	}
	return started, payload
}

func newTestService(t *testing.T, fixture *cloudflareFixture, backends BackendManager, clientFactory func(string) *http.Client) *Service {
	t.Helper()
	service, err := New(&Options{
		Config: config.CloudflareOAuthConfig{
			Enabled: true, RedirectURL: "https://center.example.test/ui/",
			SessionTTL: 20 * time.Minute, ConnectorScheme: "s3center",
		},
		Backends: backends, AuthorizationURL: fixture.server.URL + "/dashboard",
		APIBaseURL: fixture.server.URL + "/api", HTTPClient: clientFactory,
	})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func apiEnvelopeBody(result any, resultInfo map[string]any) map[string]any {
	return map[string]any{"success": true, "errors": []any{}, "result": result, "result_info": resultInfo}
}

func writeFixtureJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Errorf("encode fixture response: %v", err)
	}
}

func assertBearer(t *testing.T, r *http.Request) {
	t.Helper()
	if r.Header.Get("Authorization") != "Bearer "+testBootstrapToken {
		t.Errorf("unexpected bearer token")
	}
}
