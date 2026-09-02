package ui

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"html/template"
	"io/fs"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/afreidah/s3-orchestrator/internal/cloudflareonboard"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
)

const maxOnboardingBodyBytes = 32 << 10

var cloudflareCallbackPage = template.Must(template.New("cloudflare-callback").Parse(`<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>{{.Title}}</title><style>body{margin:0;min-height:100vh;display:grid;place-items:center;background:#07100f;color:#eaf5f0;font:16px system-ui}.card{max-width:580px;margin:24px;padding:36px;border:1px solid #29463e;border-radius:22px;background:#0f1d1b;box-shadow:0 24px 70px #0008}b{display:block;color:#f48120;font:12px ui-monospace;letter-spacing:.16em;text-transform:uppercase}h1{font-size:32px;margin:14px 0}p{color:#b8ccc4;line-height:1.7}</style><main class="card"><b>S3 Center / Cloudflare</b><h1>{{.Title}}</h1><p>{{.Message}}</p></main></html>`))

func (h *Handler) handleCloudflareOnboardingStart(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	if !h.cloudflareOnboardingAvailable(w) || !httputil.RequireMethod(w, r, http.MethodPost) {
		return
	}
	var request cloudflareonboard.StartRequest
	if !decodeOnboardingJSON(w, r, &request) {
		return
	}
	result, err := h.cloudflareOnboarding.Start(r.Context(), request)
	if err != nil {
		h.log.WarnContext(r.Context(), "Cloudflare onboarding start failed", "error", err)
		handleOnboardingError(w, err)
		return
	}
	ticketURL, err := h.trustedCloudflareURL("/connector/cloudflare/session", url.Values{"ticket": {result.ConnectorTicket}})
	if err != nil {
		handleOnboardingError(w, err)
		return
	}
	launch := &url.URL{Scheme: h.cloudflareOnboarding.ConnectorScheme(), Host: "cloudflare-login"}
	launch.RawQuery = url.Values{"ticket_url": {ticketURL}}.Encode()
	h.log.InfoContext(r.Context(), "Cloudflare onboarding session started",
		"session_id", result.Session.ID, "profile_id", result.Session.ProfileID, "proxy", result.Session.Proxy)
	h.writeNoStoreJSON(w, http.StatusCreated, map[string]any{
		"session": result.Session, "connector_url": launch.String(),
		"connector_download_url": strings.TrimRight(h.prefix, "/") + "/connector/download",
	})
}

func (h *Handler) handleCloudflareOnboardingStatus(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	if !h.cloudflareOnboardingAvailable(w) || !httputil.RequireMethod(w, r, http.MethodGet) {
		return
	}
	view, err := h.cloudflareOnboarding.Status(r.URL.Query().Get("id"))
	if err != nil {
		handleOnboardingError(w, err)
		return
	}
	h.writeNoStoreJSON(w, http.StatusOK, map[string]any{"session": view})
}

func (h *Handler) handleCloudflareOnboardingComplete(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	if !h.cloudflareOnboardingAvailable(w) || !httputil.RequireMethod(w, r, http.MethodPost) {
		return
	}
	var request cloudflareonboard.CompleteRequest
	if !decodeOnboardingJSON(w, r, &request) {
		return
	}
	view, err := h.cloudflareOnboarding.Complete(r.Context(), request)
	if err != nil {
		h.log.WarnContext(r.Context(), "Cloudflare onboarding provisioning failed",
			"session_id", request.SessionID, "account_id", request.AccountID, "bucket", request.Bucket, "error", err)
		handleOnboardingError(w, err)
		return
	}
	h.log.InfoContext(r.Context(), "Cloudflare R2 bucket provisioned",
		"backend", view.Name, "account_id", view.AccountID, "bucket", view.Bucket, "profile_id", view.ProfileID)
	h.writeNoStoreJSON(w, http.StatusCreated, map[string]any{"ok": true, "account": view})
}

// The callback intentionally does not require an administration cookie. OAuth
// state is high-entropy, single-session authority and is validated by Service.
func (h *Handler) handleCloudflareOAuthCallback(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	w.Header().Set("Cache-Control", "no-store")
	if !h.cloudflareOnboardingAvailableHTML(w) {
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	err := h.cloudflareOnboarding.Authorize(r.Context(), r.URL.Query().Get("state"), r.URL.Query().Get("code"), r.URL.Query().Get("error"))
	status, title, message := http.StatusOK, "Authorization complete", "The isolated Cloudflare profile is connected. Return to S3 Center to select an R2 bucket."
	if err != nil {
		status, title, message = http.StatusBadRequest, "Authorization failed", err.Error()
		h.log.WarnContext(r.Context(), "Cloudflare OAuth callback failed", "error", err)
	}
	var page bytes.Buffer
	if executeErr := cloudflareCallbackPage.Execute(&page, struct {
		Title   string
		Message string
	}{Title: title, Message: message}); executeErr != nil {
		h.log.ErrorContext(r.Context(), "render Cloudflare OAuth callback", "error", executeErr)
		http.Error(w, "failed to render callback", http.StatusInternalServerError)
		return
	}
	w.Header().Set(headerContentType, contentTypeHTML)
	w.WriteHeader(status)
	_, _ = w.Write(page.Bytes())
}

// handleCloudflareOAuthInfo is the public publisher, privacy, and terms page
// referenced by the Cloudflare OAuth client registration.
func (h *Handler) handleCloudflareOAuthInfo(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set(headerContentType, contentTypeHTML)
	_, _ = w.Write([]byte(`<!doctype html><html lang="zh-CN"><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>S3 Center Cloudflare Connector</title><style>body{margin:0;background:#07100f;color:#eaf5f0;font:15px system-ui;line-height:1.75}.wrap{width:min(760px,calc(100% - 40px));margin:64px auto}.brand{color:#ff9b52;font:700 12px ui-monospace;letter-spacing:.18em}.card{margin-top:20px;padding:clamp(24px,5vw,48px);border:1px solid #29463e;border-radius:24px;background:#0f1d1b}h1{font-size:36px;line-height:1.15}h2{margin-top:32px;font-size:18px;color:#9af5cf}p,li{color:#b8ccc4}code{color:#ffad70}</style><main class="wrap"><div class="brand">S3 CENTER / CLOUDFLARE</div><article class="card"><h1>Cloudflare R2 隔离授权连接器</h1><p>S3 Center 使用 Cloudflare OAuth 帮助管理员发现其明确授权的账号和 R2 Bucket，并创建仅限所选 Bucket 的存储凭据。</p><h2>隐私与数据保留</h2><ul><li>Cloudflare OAuth Access Token 仅在服务端内存中保留于短期接入会话，不写入数据库。</li><li>完成接入后，只加密保存 Bucket 级 R2 凭据、账号标识、代理设置和非敏感 Profile 标识。</li><li>Cloudflare 登录 Cookie 仅保存在管理员本机的独立浏览器 Profile，不上传至 S3 Center。</li><li>不会出售、共享或用于广告目的；数据仅用于管理员请求的 R2 管理操作。</li></ul><h2>使用条款</h2><p>仅应连接你有权管理的 Cloudflare 账号。管理员可随时在 Cloudflare 中撤销 OAuth 授权或 R2 Token，也可在 S3 Center 删除对应存储内容。服务按现状提供，实际额度与计费以 Cloudflare 为准。</p><h2>安全边界</h2><p>文件上传和下载使用短期预签名 URL，数据流量由调用方直接访问上游 R2；S3 Center 只负责索引、授权、额度决策与必要的控制面调用。</p><p>Publisher: <code>s3-center.aiid.qzz.io</code></p></article></main></html>`))
}

func (h *Handler) handleCloudflareConnectorSession(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	w.Header().Set("Cache-Control", "no-store")
	if !h.cloudflareOnboardingAvailable(w) || !httputil.RequireMethod(w, r, http.MethodGet) {
		return
	}
	payload, err := h.cloudflareOnboarding.ConsumeConnectorTicket(r.URL.Query().Get("ticket"))
	if err != nil {
		handleOnboardingError(w, err)
		return
	}
	payload.ReportURL, err = h.trustedCloudflareURL("/connector/cloudflare/report", nil)
	if err != nil {
		handleOnboardingError(w, err)
		return
	}
	h.writeNoStoreJSON(w, http.StatusOK, payload)
}

func (h *Handler) handleCloudflareConnectorReport(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	w.Header().Set("Cache-Control", "no-store")
	if !h.cloudflareOnboardingAvailable(w) || !httputil.RequireMethod(w, r, http.MethodPost) {
		return
	}
	var report cloudflareonboard.ConnectorReport
	if !decodeOnboardingJSON(w, r, &report) {
		return
	}
	if err := h.cloudflareOnboarding.ReportConnector(report); err != nil {
		handleOnboardingError(w, err)
		return
	}
	h.writeNoStoreJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (h *Handler) handleCloudflareConnectorDownload(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	if !h.cloudflareOnboardingAvailable(w) || !httputil.RequireMethod(w, r, http.MethodGet) {
		return
	}
	archive, err := cloudflareConnectorArchive()
	if err != nil {
		h.log.ErrorContext(r.Context(), "build connector archive", "error", err)
		http.Error(w, "failed to build connector package", http.StatusInternalServerError)
		return
	}
	w.Header().Set(headerContentType, "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="s3-center-cloudflare-connector.zip"`)
	w.Header().Set("Cache-Control", "private, max-age=300")
	_, _ = w.Write(archive)
}

func cloudflareConnectorArchive() ([]byte, error) {
	var output bytes.Buffer
	writer := zip.NewWriter(&output)
	err := fs.WalkDir(embeddedFS, "connector", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return walkErr
		}
		content, err := embeddedFS.ReadFile(name)
		if err != nil {
			return err
		}
		header := &zip.FileHeader{Name: path.Join("s3-center-cloudflare-connector", strings.TrimPrefix(name, "connector/")), Method: zip.Deflate}
		header.SetMode(0o644)
		file, err := writer.CreateHeader(header)
		if err != nil {
			return err
		}
		_, err = file.Write(content)
		return err
	})
	if closeErr := writer.Close(); err == nil {
		err = closeErr
	}
	return output.Bytes(), err
}

func (h *Handler) trustedCloudflareURL(suffix string, query url.Values) (string, error) {
	callback, err := url.Parse(h.cloudflareOnboarding.RedirectURL())
	if err != nil || callback.Scheme == "" || callback.Host == "" {
		return "", errors.New("cloudflare OAuth redirect URL is invalid")
	}
	callback.Path = strings.TrimRight(h.prefix, "/") + suffix
	callback.RawPath = ""
	callback.RawQuery = query.Encode()
	callback.Fragment = ""
	return callback.String(), nil
}

func (h *Handler) cloudflareOnboardingAvailable(w http.ResponseWriter) bool {
	if h.cloudflareOnboarding == nil || !h.cloudflareOnboarding.Enabled() {
		httputil.WriteJSONError(w, http.StatusServiceUnavailable, "Cloudflare OAuth onboarding is disabled")
		return false
	}
	return true
}

func (h *Handler) cloudflareOnboardingAvailableHTML(w http.ResponseWriter) bool {
	if h.cloudflareOnboarding == nil || !h.cloudflareOnboarding.Enabled() {
		http.Error(w, "Cloudflare OAuth onboarding is disabled", http.StatusServiceUnavailable)
		return false
	}
	return true
}

func decodeOnboardingJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxOnboardingBodyBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		httputil.WriteJSONError(w, http.StatusBadRequest, "invalid request")
		return false
	}
	return true
}

func handleOnboardingError(w http.ResponseWriter, err error) {
	status := http.StatusUnprocessableEntity
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "invalid or expired"), strings.Contains(message, "invalid, expired"), strings.Contains(message, "no longer exists"):
		status = http.StatusGone
	case strings.Contains(message, "already exists"):
		status = http.StatusConflict
	case strings.Contains(message, "disabled"):
		status = http.StatusServiceUnavailable
	}
	httputil.WriteJSONError(w, status, err.Error())
}

func (h *Handler) writeNoStoreJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Cache-Control", "no-store")
	httputil.WriteJSON(w, status, value)
}
