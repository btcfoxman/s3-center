// -------------------------------------------------------------------------------
// Backend Tests - S3 Client Configuration
//
// Author: Alex Freidah
//
// Unit tests for S3 backend client construction and option helpers.
// -------------------------------------------------------------------------------

package backend

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/afreidah/s3-orchestrator/internal/config"
)

func TestPresignedRequestJSONUsesPublicContractFieldNames(t *testing.T) {
	t.Parallel()
	raw, err := json.Marshal(&PresignedRequest{
		URL: "https://upstream.invalid/object", Method: http.MethodPut,
		Headers: http.Header{"X-Test": {"value"}}, ExpiresAt: time.Unix(1, 0).UTC(),
	})
	if err != nil {
		t.Fatalf("marshal PresignedRequest: %v", err)
	}
	var response map[string]json.RawMessage
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatalf("unmarshal PresignedRequest: %v", err)
	}
	for _, field := range []string{"url", "method", "headers", "expires_at"} {
		if _, ok := response[field]; !ok {
			t.Fatalf("public JSON field %q is missing from %s", field, raw)
		}
	}
	for _, field := range []string{"URL", "Method", "Headers", "ExpiresAt"} {
		if _, ok := response[field]; ok {
			t.Fatalf("internal JSON field %q leaked in %s", field, raw)
		}
	}
}

func TestPresignPutObjectReturnsBrowserSafeRequiredHeaders(t *testing.T) {
	t.Parallel()
	be := newTestBackend(t, func(http.ResponseWriter, *http.Request) {})

	request, err := be.PresignPutObject(t.Context(), "__objects/du_test.txt", "text/plain", "du_test", time.Minute)
	if err != nil {
		t.Fatalf("PresignPutObject: %v", err)
	}
	if request.Headers.Get("Content-Type") != "text/plain" {
		t.Fatalf("Content-Type header = %q, want text/plain", request.Headers.Get("Content-Type"))
	}
	if request.Headers.Get("X-Amz-Meta-Upload-Id") != "du_test" {
		t.Fatalf("upload marker header = %q, want du_test", request.Headers.Get("X-Amz-Meta-Upload-Id"))
	}
	if request.Headers.Get("Host") != "" {
		t.Fatalf("browser response must not require forbidden Host header: %v", request.Headers)
	}
	signedHeaders := urlValues(t, request.URL).Get("X-Amz-SignedHeaders")
	if !strings.Contains(signedHeaders, "x-amz-meta-upload-id") {
		t.Fatalf("signed headers = %q, want upload marker", signedHeaders)
	}
}

func urlValues(t *testing.T, rawURL string) url.Values {
	t.Helper()
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse presigned URL: %v", err)
	}
	return parsed.Query()
}

func TestNewBackendTransportUsesAccountProxy(t *testing.T) {
	t.Parallel()
	transport := newBackendTransport("socks5://user:secret@203.0.113.4:1080")
	requestURL, _ := url.Parse("https://example.r2.cloudflarestorage.com")
	proxyURL, err := transport.Proxy(&http.Request{URL: requestURL})
	if err != nil {
		t.Fatal(err)
	}
	if proxyURL == nil || proxyURL.Scheme != "socks5" || proxyURL.Host != "203.0.113.4:1080" {
		t.Fatalf("proxy URL = %v", proxyURL)
	}
}

// nonSeekableReader wraps a reader so it cannot be type-asserted to
// io.ReadSeeker, forcing PutObject's signed-payload materialization path.
type nonSeekableReader struct{ r io.Reader }

func (n nonSeekableReader) Read(p []byte) (int, error) { return n.r.Read(p) }

// TestPreparePutBody_UnsignedStreamsDirectly covers the unsigned-payload path:
// the body is passed through untouched and tagged with the unsigned option.
func TestPreparePutBody_UnsignedStreamsDirectly(t *testing.T) {
	t.Parallel()
	payload := []byte("prepare-put-body-payload")
	b := &S3Backend{unsignedPayload: true}

	got, opts, cleanup, err := b.preparePutBody(nonSeekableReader{r: bytes.NewReader(payload)}, int64(len(payload)))
	if err != nil {
		t.Fatalf("preparePutBody: %v", err)
	}
	defer cleanup()

	if len(opts) != 1 {
		t.Errorf("opts = %d, want 1 (withUnsignedPayload)", len(opts))
	}
	if data, _ := io.ReadAll(got); !bytes.Equal(data, payload) {
		t.Errorf("body = %q, want the original payload passed through", data)
	}
}

// TestPreparePutBody_SignedSeekablePassthrough covers signed-payload mode with
// an already-seekable body: it is passed through unchanged, not re-materialized.
func TestPreparePutBody_SignedSeekablePassthrough(t *testing.T) {
	t.Parallel()
	payload := []byte("prepare-put-body-payload")
	b := &S3Backend{unsignedPayload: false}
	seekable := bytes.NewReader(payload)

	got, opts, cleanup, err := b.preparePutBody(seekable, int64(len(payload)))
	if err != nil {
		t.Fatalf("preparePutBody: %v", err)
	}
	defer cleanup()

	if opts != nil {
		t.Errorf("signed mode should add no API options, got %d", len(opts))
	}
	if got != io.Reader(seekable) {
		t.Error("a seekable body should be passed through unchanged, not re-materialized")
	}
}

// TestPreparePutBody_SignedNonSeekableMaterializes covers signed-payload mode
// with a non-seekable body: it is materialized to a seekable form that replays
// the original bytes.
func TestPreparePutBody_SignedNonSeekableMaterializes(t *testing.T) {
	t.Parallel()
	payload := []byte("prepare-put-body-payload")
	b := &S3Backend{unsignedPayload: false}

	got, opts, cleanup, err := b.preparePutBody(nonSeekableReader{r: bytes.NewReader(payload)}, int64(len(payload)))
	if err != nil {
		t.Fatalf("preparePutBody: %v", err)
	}
	defer cleanup()

	if opts != nil {
		t.Errorf("signed mode should add no API options, got %d", len(opts))
	}
	rs, ok := got.(io.ReadSeeker)
	if !ok {
		t.Fatalf("materialized body is %T, want a seekable io.ReadSeeker", got)
	}
	if data, _ := io.ReadAll(rs); !bytes.Equal(data, payload) {
		t.Errorf("materialized body = %q, want %q", data, payload)
	}
}

// TestPutObject_SignedPayloadNonSeekableBodyMaterializes pins #972: with
// unsigned_payload disabled and a non-seekable body, PutObject materializes the
// stream to a seekable form instead of io.ReadAll-ing the whole object into
// memory, and the backend still receives every byte.
func TestPutObject_SignedPayloadNonSeekableBodyMaterializes(t *testing.T) {
	t.Parallel()

	var got []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		w.Header().Set("ETag", `"abc123"`)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	be, err := NewS3Backend(t.Context(), &config.BackendConfig{
		Name:            "test",
		Endpoint:        srv.URL, // http:// forces signed payload
		Region:          "us-east-1",
		Bucket:          "test-bucket",
		AccessKeyID:     "AKID",
		SecretAccessKey: "secret",
		ForcePathStyle:  true, // reach the httptest server at /bucket/key
		DisableChecksum: true, // keep the wire body raw (no aws-chunked framing)
	})
	if err != nil {
		t.Fatalf("NewS3Backend: %v", err)
	}
	if be.unsignedPayload {
		t.Fatal("expected signed-payload mode for the http endpoint")
	}

	payload := bytes.Repeat([]byte("s3o-signed-payload-"), 4096) // ~76 KiB
	body := nonSeekableReader{r: bytes.NewReader(payload)}

	etag, err := be.PutObject(t.Context(), "dir/obj", body, int64(len(payload)), "text/plain", nil)
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	if etag == "" {
		t.Error("expected an ETag back")
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("backend received %d bytes, want %d (body dropped or truncated)", len(got), len(payload))
	}
}

// TestWithUnsignedPayload_AddsAPIOption verifies the with unsigned payload adds apioption contract.
// Asserts that expected 1 API option, got.
func TestWithUnsignedPayload_AddsAPIOption(t *testing.T) {
	t.Parallel()
	var opts s3.Options
	withUnsignedPayload(&opts)

	if len(opts.APIOptions) != 1 {
		t.Fatalf("expected 1 API option, got %d", len(opts.APIOptions))
	}
}

// TestNewS3Backend_UnsignedPayloadDefaults verifies the new s3 backend unsigned payload defaults contract.
// Asserts that unexpected error:.
func TestNewS3Backend_UnsignedPayloadDefaults(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name            string
		endpoint        string
		unsignedPayload *bool
		wantUnsigned    bool
	}{
		{
			name:         "https defaults to unsigned",
			endpoint:     "https://example.com",
			wantUnsigned: true,
		},
		{
			name:         "http forces signed",
			endpoint:     "http://example.com",
			wantUnsigned: false,
		},
		{
			name:            "explicit false overrides https",
			endpoint:        "https://example.com",
			unsignedPayload: func() *bool { b := false; return &b }(),
			wantUnsigned:    false,
		},
		{
			name:            "explicit true with http is respected",
			endpoint:        "http://example.com",
			unsignedPayload: func() *bool { b := true; return &b }(),
			wantUnsigned:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend, err := NewS3Backend(t.Context(), &config.BackendConfig{
				Name:            "test",
				Endpoint:        tt.endpoint,
				Region:          "us-east-1",
				Bucket:          "test-bucket",
				AccessKeyID:     "AKID",
				SecretAccessKey: "secret",
				UnsignedPayload: tt.unsignedPayload,
			})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if backend.unsignedPayload != tt.wantUnsigned {
				t.Errorf("unsignedPayload = %v, want %v", backend.unsignedPayload, tt.wantUnsigned)
			}
		})
	}
}

// TestNewS3Backend_DisableChecksum verifies the new s3 backend disable checksum contract.
// Asserts that unexpected error:.
func TestNewS3Backend_DisableChecksum(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name            string
		disableChecksum bool
	}{
		{
			name:            "checksum disabled",
			disableChecksum: true,
		},
		{
			name:            "checksum enabled (default)",
			disableChecksum: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewS3Backend(t.Context(), &config.BackendConfig{
				Name:            "test",
				Endpoint:        "https://storage.googleapis.com",
				Region:          "us",
				Bucket:          "test-bucket",
				AccessKeyID:     "AKID",
				SecretAccessKey: "secret",
				DisableChecksum: tt.disableChecksum,
			})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// TestNewS3Backend_DefaultChainConstructs pins that a backend with
// CredentialSource=default_chain wires the AWS SDK default-chain
// provider instead of static keys. Construction must succeed; actual
// resolution happens lazily on first request and is exercised by the
// integration suite when run against an AWS-attached instance.
func TestNewS3Backend_DefaultChainConstructs(t *testing.T) {
	t.Parallel()
	be, err := NewS3Backend(t.Context(), &config.BackendConfig{
		Name:             "test-default-chain",
		Endpoint:         "https://s3.amazonaws.com",
		Region:           "us-east-1",
		Bucket:           "test-bucket",
		CredentialSource: config.CredentialSourceDefaultChain,
	})
	if err != nil {
		t.Fatalf("default_chain construction failed: %v", err)
	}
	if be == nil {
		t.Fatal("default_chain returned nil backend")
	}
}

// TestNewS3Backend_StripSDKHeaders verifies the new s3 backend strip sdkheaders contract.
// Asserts that unexpected error:.
func TestNewS3Backend_StripSDKHeaders(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name            string
		stripSDKHeaders bool
	}{
		{
			name:            "strip enabled",
			stripSDKHeaders: true,
		},
		{
			name:            "strip disabled (default)",
			stripSDKHeaders: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewS3Backend(t.Context(), &config.BackendConfig{
				Name:            "test",
				Endpoint:        "https://storage.googleapis.com",
				Region:          "auto",
				Bucket:          "test-bucket",
				AccessKeyID:     "AKID",
				SecretAccessKey: "secret",
				StripSDKHeaders: tt.stripSDKHeaders,
			})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}
