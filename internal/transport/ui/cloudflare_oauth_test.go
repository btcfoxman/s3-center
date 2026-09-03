package ui

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/backendmanage"
	"github.com/afreidah/s3-orchestrator/internal/cloudflareonboard"
	"github.com/afreidah/s3-orchestrator/internal/config"
)

type onboardingBackendStub struct{}

func (onboardingBackendStub) CreateProvisioned(context.Context, *backendmanage.CreateRequest, backendmanage.Connection) (backendmanage.View, error) {
	return backendmanage.View{}, errors.New("not used")
}
func (onboardingBackendStub) ConnectionForAccount(string) (backendmanage.AccountConnection, error) {
	return backendmanage.AccountConnection{}, errors.New("not used")
}
func (onboardingBackendStub) HasBackend(string) bool { return false }

func TestCloudflareStartUsesTrustedOriginAndOneTimeSecretTicket(t *testing.T) {
	t.Parallel()
	service, err := cloudflareonboard.New(&cloudflareonboard.Options{
		Config: config.CloudflareOAuthConfig{
			Enabled: true, ClientID: "client", ClientSecret: "secret",
			RedirectURL: "https://trusted.example/ui/",
			Scopes:      []string{"scope.read"}, SessionTTL: 20 * time.Minute,
			ConnectorScheme: "s3center", TokenEndpointAuthMethod: "client_secret_basic",
		},
		Backends: onboardingBackendStub{},
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := &Handler{log: slog.Default(), cloudflareOnboarding: service, prefix: "/ui"}
	body := `{"identity_label":"Tokyo identity","proxy_url":"http://proxy-user:proxy-pass@203.0.113.5:8080"}`
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "https://attacker.invalid/ui/api/cloudflare-onboarding/start", strings.NewReader(body))
	recorder := httptest.NewRecorder()
	handler.handleCloudflareOnboardingStart(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("start status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "proxy-user") || strings.Contains(recorder.Body.String(), "proxy-pass") {
		t.Fatal("start response leaked proxy credentials")
	}
	var started struct {
		ConnectorURL string `json:"connector_url"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	launch, err := url.Parse(started.ConnectorURL)
	if err != nil {
		t.Fatal(err)
	}
	ticketURL := launch.Query().Get("ticket_url")
	if !strings.HasPrefix(ticketURL, "https://trusted.example/ui/connector/cloudflare/session?ticket=") || strings.Contains(ticketURL, "attacker.invalid") {
		t.Fatalf("untrusted connector ticket URL: %s", ticketURL)
	}

	request = httptest.NewRequestWithContext(t.Context(), http.MethodGet, ticketURL, nil)
	recorder = httptest.NewRecorder()
	handler.handleCloudflareConnectorSession(recorder, request)
	if recorder.Code != http.StatusOK || recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("ticket response = %d %q", recorder.Code, recorder.Header().Get("Cache-Control"))
	}
	if !strings.Contains(recorder.Body.String(), "proxy-user:proxy-pass") || !strings.Contains(recorder.Body.String(), "https://trusted.example/ui/connector/cloudflare/report") {
		t.Fatalf("connector did not receive its private payload: %s", recorder.Body.String())
	}

	request = httptest.NewRequestWithContext(t.Context(), http.MethodGet, ticketURL, nil)
	recorder = httptest.NewRecorder()
	handler.handleCloudflareConnectorSession(recorder, request)
	if recorder.Code != http.StatusGone {
		t.Fatalf("reused ticket status = %d, want %d", recorder.Code, http.StatusGone)
	}
}

func TestCloudflareConnectorArchiveIsComplete(t *testing.T) {
	t.Parallel()
	archive, err := cloudflareConnectorArchive()
	if err != nil {
		t.Fatal(err)
	}
	reader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		t.Fatal(err)
	}
	files := make(map[string]bool, len(reader.File))
	for _, file := range reader.File {
		files[file.Name] = true
		if strings.Contains(file.Name, "node_modules") {
			t.Fatalf("archive contains installed dependencies: %s", file.Name)
		}
	}
	for _, name := range []string{"connector.mjs", "launcher.cmd", "install.ps1", "uninstall.ps1", "package.json", "package-lock.json", "README.md"} {
		fullName := "s3-center-cloudflare-connector/" + name
		if !files[fullName] {
			t.Errorf("archive missing %s", fullName)
		}
	}
}
