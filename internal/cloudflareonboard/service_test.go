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
	testAccountID = "0123456789abcdef0123456789abcdef"
	testTokenID   = "abcdef0123456789abcdef0123456789"
)

type fakeBackends struct {
	mu         sync.Mutex
	request    *backendmanage.CreateRequest
	connection backendmanage.Connection
	createErr  error
	existing   backendmanage.AccountConnection
}

func (f *fakeBackends) CreateProvisioned(_ context.Context, request *backendmanage.CreateRequest, connection backendmanage.Connection) (backendmanage.View, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	copyOfRequest := *request
	f.request, f.connection = &copyOfRequest, connection
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

type cloudflareFixture struct {
	server       *httptest.Server
	mu           sync.Mutex
	createBody   map[string]any
	deleteCalled bool
}

//nolint:gocognit,gosec // The local fake intentionally exercises every OAuth branch with non-production credentials.
func newCloudflareFixture(t *testing.T) *cloudflareFixture {
	t.Helper()
	fixture := &cloudflareFixture{}
	fixture.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/oauth/token":
			if user, password, ok := r.BasicAuth(); !ok || user != "client-id" || password != "client-secret" {
				t.Errorf("unexpected OAuth client authentication: %q %q %v", user, password, ok)
			}
			_ = r.ParseForm()
			if r.Form.Get("code") != "authorization-code" || r.Form.Get("code_verifier") == "" {
				t.Errorf("unexpected OAuth form: %v", r.Form)
			}
			writeFixtureJSON(t, w, map[string]any{"access_token": "oauth-access-token"})
		case r.URL.Path == "/api/accounts":
			assertBearer(t, r)
			writeFixtureJSON(t, w, apiEnvelopeBody([]map[string]string{{"id": testAccountID, "name": "Example Account"}}, map[string]any{"page": 1, "total_pages": 1}))
		case r.URL.Path == "/api/accounts/"+testAccountID+"/r2/buckets":
			assertBearer(t, r)
			writeFixtureJSON(t, w, apiEnvelopeBody(map[string]any{"buckets": []map[string]string{{"name": "media-assets", "jurisdiction": "default"}}}, map[string]any{}))
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
			writeFixtureJSON(t, w, apiEnvelopeBody(map[string]string{"id": testTokenID, "value": "one-time-token-value"}, map[string]any{}))
		case r.URL.Path == "/api/accounts/"+testAccountID+"/tokens/"+testTokenID && r.Method == http.MethodDelete:
			fixture.mu.Lock()
			fixture.deleteCalled = true
			fixture.mu.Unlock()
			writeFixtureJSON(t, w, apiEnvelopeBody(map[string]string{"id": testTokenID}, map[string]any{}))
		default:
			http.Error(w, `{"success":false}`, http.StatusNotFound)
		}
	}))
	t.Cleanup(fixture.server.Close)
	return fixture
}

//nolint:gocognit,gosec // End-to-end assertions intentionally keep the complete secret-boundary flow together with fake credentials.
func TestOAuthOnboardingEndToEndAndSecretBoundaries(t *testing.T) {
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
	if authorization.Query().Get("code_challenge_method") != "S256" || authorization.Query().Get("state") == "" || authorization.Query().Get("scope") != "account.read r2.write tokens.write" {
		t.Fatalf("unexpected authorization URL: %s", started.AuthorizationURL)
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
	if err := service.ReportConnector(ConnectorReport{Token: payload.ReportToken, Status: "proxy_verified", ExitIP: "203.0.113.9"}); err != nil {
		t.Fatal(err)
	}

	state := authorization.Query().Get("state")
	if err := service.Authorize(context.Background(), state, "authorization-code", ""); err != nil {
		t.Fatal(err)
	}
	if err := service.Authorize(context.Background(), state, "authorization-code", ""); err == nil {
		t.Fatal("OAuth state was reusable")
	}
	view, err := service.Status(started.Session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if view.Status != "authorized" || len(view.Accounts) != 1 || len(view.Accounts[0].Buckets) != 1 || strings.Contains(view.Proxy, "proxy-user") {
		t.Fatalf("unexpected safe session view: %+v", view)
	}

	created, err := service.Complete(context.Background(), CompleteRequest{
		SessionID: started.Session.ID, AccountID: testAccountID, Bucket: "media-assets", BackendName: "r2-oauth-test", WarningPercent: 83,
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.AuthMode != "cloudflare_oauth" || backends.connection.ProfileID != started.Session.ProfileID {
		t.Fatalf("connection metadata not preserved: %+v %+v", created, backends.connection)
	}
	wantHash := sha256.Sum256([]byte("one-time-token-value"))
	if backends.request.AccessKeyID != testTokenID || backends.request.SecretAccessKey != hex.EncodeToString(wantHash[:]) || backends.request.ProxyURL != "http://proxy-user:proxy-password@203.0.113.7:3128" {
		t.Fatalf("incorrect durable R2 credentials: %+v", backends.request)
	}
	if len(proxies) != 2 || proxies[0] != backends.request.ProxyURL || proxies[1] != backends.request.ProxyURL {
		t.Fatalf("OAuth and provisioning did not share the bound proxy: %v", proxies)
	}
	fixture.mu.Lock()
	resource := fixture.createBody["policies"].([]any)[0].(map[string]any)["resources"].(map[string]any)
	fixture.mu.Unlock()
	wantResource := "com.cloudflare.edge.r2.bucket." + testAccountID + "_default_media-assets"
	if resource[wantResource] != "*" {
		t.Fatalf("bucket token resource = %#v", resource)
	}
}

func TestProvisionFailureRevokesGeneratedToken(t *testing.T) {
	t.Parallel()
	fixture := newCloudflareFixture(t)
	backends := &fakeBackends{createErr: errors.New("probe failed")}
	service := newTestService(t, fixture, backends, func(string) *http.Client { return fixture.server.Client() })
	started, _ := service.Start(context.Background(), StartRequest{IdentityLabel: "failure case"})
	authorization, _ := url.Parse(started.AuthorizationURL)
	if err := service.Authorize(context.Background(), authorization.Query().Get("state"), "authorization-code", ""); err != nil {
		t.Fatal(err)
	}
	_, err := service.Complete(context.Background(), CompleteRequest{SessionID: started.Session.ID, AccountID: testAccountID, Bucket: "media-assets"})
	if err == nil || !strings.Contains(err.Error(), "probe failed") {
		t.Fatalf("expected backend failure, got %v", err)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if !fixture.deleteCalled {
		t.Fatal("unused Cloudflare token was not revoked")
	}
}

func TestSessionExpiryAndExistingProfileReuse(t *testing.T) {
	t.Parallel()
	fixture := newCloudflareFixture(t)
	now := time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)
	connection := &backendmanage.Connection{IdentityID: "cfi-0123456789abcdef01234567", ProfileID: "cf-0123456789abcdef", AuthMode: "cloudflare_oauth"}
	backends := &fakeBackends{existing: backendmanage.AccountConnection{
		AccountID: testAccountID, AccountLabel: "Existing", ProxyURL: "socks5://user:secret@127.0.0.1:1080", Connection: connection,
	}}
	service := newTestService(t, fixture, backends, func(string) *http.Client { return fixture.server.Client() })
	service.now = func() time.Time { return now }
	started, err := service.Start(context.Background(), StartRequest{AccountID: testAccountID})
	if err != nil {
		t.Fatal(err)
	}
	if started.Session.ProfileID != connection.ProfileID || started.Session.IdentityID != connection.IdentityID || strings.Contains(started.Session.Proxy, "secret") {
		t.Fatalf("existing identity was not safely reused: %+v", started.Session)
	}
	now = now.Add(21 * time.Minute)
	if _, err := service.Status(started.Session.ID); err == nil {
		t.Fatal("expired session remained available")
	}
}

func newTestService(t *testing.T, fixture *cloudflareFixture, backends BackendManager, clientFactory func(string) *http.Client) *Service {
	t.Helper()
	service, err := New(&Options{
		Config: config.CloudflareOAuthConfig{
			Enabled: true, ClientID: "client-id", ClientSecret: "client-secret",
			RedirectURL: "https://center.example.test/ui/oauth/cloudflare/callback",
			Scopes:      []string{"account.read", "r2.write", "tokens.write"}, SessionTTL: 20 * time.Minute,
			ConnectorScheme: "s3center", TokenEndpointAuthMethod: "client_secret_basic",
		},
		Backends: backends, AuthorizationURL: fixture.server.URL + "/oauth/authorize",
		TokenURL: fixture.server.URL + "/oauth/token", APIBaseURL: fixture.server.URL + "/api", HTTPClient: clientFactory,
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
	if r.Header.Get("Authorization") != "Bearer oauth-access-token" {
		t.Errorf("unexpected bearer token: %q", r.Header.Get("Authorization"))
	}
}
