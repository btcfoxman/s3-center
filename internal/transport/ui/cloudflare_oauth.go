package ui

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/afreidah/s3-orchestrator/internal/cloudflareonboard"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
)

const maxOnboardingBodyBytes = 32 << 10

var cloudflareConnectorInfoPage = []byte(`<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width">
<title>S3 Center Cloudflare 隔离连接器</title><style>
body{margin:0;background:#07100f;color:#eaf5f0;font:15px/1.75 system-ui}.wrap{width:min(780px,calc(100% - 40px));margin:64px auto}.brand{color:#ff9b52;font:700 12px ui-monospace;letter-spacing:.18em}.card{margin-top:20px;padding:clamp(24px,5vw,48px);border:1px solid #29463e;border-radius:24px;background:#0f1d1b}h1{font-size:36px;line-height:1.15}h2{margin-top:30px;font-size:18px;color:#9af5cf}p,li{color:#b8ccc4}code{color:#ffad70}
</style></head><body><main class="wrap"><div class="brand">S3 CENTER / CLOUDFLARE</div><article class="card">
<h1>Cloudflare R2 隔离浏览器连接器</h1>
<p>连接器为每个 Cloudflare 身份建立独立的本机浏览器 Profile，并可绑定专属 HTTP、HTTPS 或 SOCKS5 代理。Cloudflare 登录 Cookie 只保存在该 Profile，不会上传到 S3 Center。</p>
<h2>接入方式</h2><p>Cloudflare 公共 OAuth 不提供创建 Account API Token 所需的权限，因此连接器会打开 Cloudflare 官方预填 Token 页面。管理员确认后，将页面显示的一次性引导 Token 通过加密 HTTPS 通道提交；S3 Center 经同一代理发现 R2 Bucket、创建仅限所选 Bucket 的凭据，并自动撤销引导 Token。</p>
<h2>数据与安全边界</h2><ul><li>引导 Token 只存在于短期服务端内存，不写入数据库或日志。</li><li>持久化内容仅包括加密后的 Bucket 级凭据、账号标识、代理设置及非敏感 Profile ID。</li><li>文件上传、下载与预签名访问均由调用方直接连接上游 R2；S3 Center 只承担索引、授权、额度决策和控制面调用。</li></ul>
<h2>使用条款</h2><p>仅连接你有权管理的 Cloudflare 账号。如页面提示自动撤销失败，或创建 Token 后放弃接入流程，应立即在 Cloudflare 的 Account API Tokens 页面手动撤销名为 <code>S3 Center temporary onboarding</code> 的 Token。</p>
<p>Publisher: <code>s3-center.aiid.qzz.io</code></p></article></main></body></html>`)

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
	completed, _ := h.cloudflareOnboarding.Status(request.SessionID)
	h.writeNoStoreJSON(w, http.StatusCreated, map[string]any{"ok": true, "account": view, "warning": completed.Error})
}

// handleCloudflareOAuthInfo is the public connector, privacy, and terms page.
// The legacy route name remains stable for bookmarked deployments.
func (h *Handler) handleCloudflareOAuthInfo(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set(headerContentType, contentTypeHTML)
	_, _ = w.Write(cloudflareConnectorInfoPage)
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
	if err := h.cloudflareOnboarding.ReportConnector(r.Context(), &report); err != nil {
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
	publicBase, err := url.Parse(h.cloudflareOnboarding.PublicURL())
	if err != nil || publicBase.Scheme == "" || publicBase.Host == "" {
		return "", errors.New("cloudflare onboarding public URL is invalid")
	}
	publicBase.Path = strings.TrimRight(h.prefix, "/") + suffix
	publicBase.RawPath = ""
	publicBase.RawQuery = query.Encode()
	publicBase.Fragment = ""
	return publicBase.String(), nil
}

func (h *Handler) cloudflareOnboardingAvailable(w http.ResponseWriter) bool {
	if h.cloudflareOnboarding == nil || !h.cloudflareOnboarding.Enabled() {
		httputil.WriteJSONError(w, http.StatusServiceUnavailable, "Cloudflare browser onboarding is disabled")
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
