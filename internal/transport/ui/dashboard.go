// -------------------------------------------------------------------------------
// UI Handler - Dashboard HTML and JSON
//
// Author: Alex Freidah
//
// Server-side rendered HTML dashboard plus its JSON twin. The HTML
// renderer composes the page from the same dashboard data the API
// returns, then folds in the configuration summary the template needs
// to decide which admin-action buttons to render and ensures every
// configured bucket shows up as a top-level directory even when empty.
// Interactive form controls in the template carry aria-labels so the
// rendered page passes accessibility checks.
// -------------------------------------------------------------------------------

package ui

import (
	"bytes"
	"context"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/afreidah/s3-orchestrator/internal/backendmanage"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/proxy/dashboard"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
)

// dashboardPage holds all data passed to the dashboard template.
type dashboardPage struct {
	Version                string
	DBHealthy              bool
	Data                   *dashboard.Data
	Buckets                []string
	Config                 configSummary
	Integrity              integrityStats
	TotalBytesUsed         int64
	TotalBytesLimit        int64
	TotalOrphanBytes       int64
	CloudflareAccounts     []cloudflareAccountUsage
	ManagedBackendsEnabled bool
	CloudflareOAuthEnabled bool
	IndexOnly              bool
	LifecyclePolicies      []core.LifecyclePolicy
	LifecyclePolicyEnabled bool
	LifecyclePolicyError   string
	StaticLifecycleRules   int
}

type cloudflareAccountUsage struct {
	Label              string
	AccountID          string
	AccountIDShort     string
	QuotaGroup         string
	Backends           []string
	Buckets            []string
	Proxy              string
	UsedBytes          int64
	OrphanBytes        int64
	FreeTierBytes      int64
	AdmissibleBytes    int64
	FreeRemainingBytes int64
	SafeRemainingBytes int64
	ObservedRequests   int64
	WarningPercent     int
	StoragePercent     float64
	State              string
	Managed            bool
	ProxyEditable      bool
	ProfileID          string
	AuthMode           string
	Paused             bool
	PauseEditable      bool
}

func cloudflareUsage(views []backendmanage.View, data *dashboard.Data) []cloudflareAccountUsage {
	groups := make(map[string]*cloudflareAccountUsage)
	for idx := range views {
		mergeCloudflareUsage(groups, &views[idx], data)
	}
	result := make([]cloudflareAccountUsage, 0, len(groups))
	for _, account := range groups {
		finalizeCloudflareUsage(account)
		result = append(result, *account)
	}
	sort.Slice(result, func(i, j int) bool { return strings.Compare(result[i].Label, result[j].Label) < 0 })
	return result
}

func mergeCloudflareUsage(groups map[string]*cloudflareAccountUsage, view *backendmanage.View, data *dashboard.Data) {
	if view.Provider != "cloudflare_r2" && view.AccountID == "" {
		return
	}
	key := cloudflareAccountKey(view)
	account := groups[key]
	if account == nil {
		account = newCloudflareAccountUsage(view, key)
		groups[key] = account
	} else if account.Proxy != view.Proxy {
		account.Proxy = "混合出口"
	}
	account.Managed = account.Managed || view.Managed
	account.ProxyEditable = account.ProxyEditable && view.AccountID != ""
	account.Paused = account.Paused || view.Paused
	account.PauseEditable = account.PauseEditable && view.AccountID != ""
	if account.ProfileID == "" {
		account.ProfileID = view.ProfileID
	}
	if account.AuthMode == "" {
		account.AuthMode = view.AuthMode
	}
	account.Backends = append(account.Backends, view.Name)
	account.Buckets = append(account.Buckets, view.Bucket)
	stat := data.QuotaStats[view.Name]
	account.UsedBytes += stat.BytesUsed + stat.OrphanBytes
	account.OrphanBytes += stat.OrphanBytes
	account.ObservedRequests += data.UsageStats[view.Name].APIRequests
}

func cloudflareAccountKey(view *backendmanage.View) string {
	if view.AccountID != "" {
		return view.AccountID
	}
	if view.QuotaGroup != "" {
		return view.QuotaGroup
	}
	return view.Name
}

func newCloudflareAccountUsage(view *backendmanage.View, key string) *cloudflareAccountUsage {
	label := view.AccountLabel
	if label == "" {
		label = view.Name
	}
	shortID := view.AccountID
	if len(shortID) > 14 {
		shortID = shortID[:6] + "…" + shortID[len(shortID)-6:]
	}
	return &cloudflareAccountUsage{
		Label: label, AccountID: view.AccountID, AccountIDShort: shortID, QuotaGroup: key, Proxy: view.Proxy,
		FreeTierBytes: view.FreeTierBytes, AdmissibleBytes: view.AdmissibleBytes,
		WarningPercent: view.WarningPercent, Managed: view.Managed, ProxyEditable: view.AccountID != "",
		Paused: view.Paused, PauseEditable: view.AccountID != "",
		ProfileID: view.ProfileID, AuthMode: view.AuthMode,
	}
}

func finalizeCloudflareUsage(account *cloudflareAccountUsage) {
	sort.Strings(account.Backends)
	sort.Strings(account.Buckets)
	account.Buckets = slicesCompact(account.Buckets)
	account.FreeRemainingBytes = max(0, account.FreeTierBytes-account.UsedBytes)
	account.SafeRemainingBytes = max(0, account.AdmissibleBytes-account.UsedBytes)
	if account.FreeTierBytes > 0 {
		account.StoragePercent = float64(account.UsedBytes) / float64(account.FreeTierBytes) * 100
	}
	account.State = "healthy"
	if account.FreeTierBytes > 0 && account.UsedBytes >= account.FreeTierBytes {
		account.State = "critical"
	} else if account.WarningPercent > 0 && account.StoragePercent >= float64(account.WarningPercent) {
		account.State = "warning"
	}
}

func slicesCompact(values []string) []string {
	if len(values) < 2 {
		return values
	}
	out := values[:1]
	for _, value := range values[1:] {
		if value != out[len(out)-1] {
			out = append(out, value)
		}
	}
	return out
}

// counterVecTotal sums the current value of every label combination on
// a Prometheus CounterVec. Used by the dashboard to surface integrity
// check / error totals without scraping /metrics. Returns 0 on any
// collection error - the dashboard treats "no data" the same as "zero
// activity," which is the operator-meaningful interpretation.
func counterVecTotal(v *prometheus.CounterVec) float64 {
	ch := make(chan prometheus.Metric, 64)
	v.Collect(ch)
	close(ch)
	var total float64
	for m := range ch {
		var pb dto.Metric
		if err := m.Write(&pb); err != nil {
			continue
		}
		if c := pb.GetCounter(); c != nil {
			total += c.GetValue()
		}
	}
	return total
}

// configSummary holds non-sensitive configuration for display. The Enabled
// flags drive which admin-action buttons render in the dashboard.
type configSummary struct {
	RoutingStrategy           string
	ReplicationFactor         int
	RebalanceEnabled          bool
	RebalanceStrategy         string
	RateLimitEnabled          bool
	EncryptionEnabled         bool
	CompressionEnabled        bool
	IntegrityEnabled          bool
	IntegrityVerifyOnRead     bool
	IntegrityScrubberInterval time.Duration
}

// integrityStats is the dashboard's snapshot of the s3o_integrity_*
// Prometheus counters. Numeric totals are summed across label values
// (operation labels) so the UI shows a single overall count.
type integrityStats struct {
	ChecksTotal float64
	ErrorsTotal float64
}

// handleDashboard renders the HTML dashboard page.
func (h *Handler) handleDashboard(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)

	data, err := h.dashboardOps.GetData(r.Context())
	if err != nil {
		h.log.ErrorContext(r.Context(), "failed to get dashboard data", "error", err)
		http.Error(w, "Failed to load dashboard data", http.StatusInternalServerError)
		return
	}

	cfg := h.cfg.Load()
	bucketNames := make([]string, len(cfg.Buckets))
	for i, b := range cfg.Buckets {
		bucketNames[i] = b.Name
	}

	// Ensure every configured bucket appears as a top-level directory in the
	// object tree, even when the bucket has no files yet.
	existing := make(map[string]bool, len(data.TopLevelEntries.Entries))
	for _, e := range data.TopLevelEntries.Entries {
		existing[e.Name] = true
	}
	for _, name := range bucketNames {
		dirName := name + "/"
		if !existing[dirName] {
			data.TopLevelEntries.Entries = append(data.TopLevelEntries.Entries, core.DirEntry{
				Name:  dirName,
				IsDir: true,
			})
		}
	}

	var totalUsed, totalLimit, totalOrphan int64
	unlimited := false
	for _, stat := range data.QuotaStats {
		totalUsed += stat.BytesUsed
		totalOrphan += stat.OrphanBytes
		if stat.BytesLimit == 0 {
			unlimited = true
		}
		totalLimit += stat.BytesLimit
	}
	if unlimited {
		totalLimit = 0
	}

	lifecyclePolicies, lifecyclePolicyEnabled, lifecyclePolicyError := h.lifecyclePolicyPageData(r.Context())
	page := dashboardPage{
		Version:                telemetry.Version,
		DBHealthy:              h.dbHealthy(),
		Data:                   data,
		Buckets:                bucketNames,
		TotalBytesUsed:         totalUsed,
		TotalBytesLimit:        totalLimit,
		TotalOrphanBytes:       totalOrphan,
		ManagedBackendsEnabled: h.backendManagement != nil && h.backendManagement.Enabled(),
		CloudflareOAuthEnabled: h.cloudflareOnboarding != nil && h.cloudflareOnboarding.Enabled(),
		IndexOnly:              cfg.DirectUpload.IndexOnly,
		LifecyclePolicies:      lifecyclePolicies,
		LifecyclePolicyEnabled: lifecyclePolicyEnabled,
		LifecyclePolicyError:   lifecyclePolicyError,
		StaticLifecycleRules:   len(cfg.Lifecycle.Rules),
		Config: configSummary{
			RoutingStrategy:           string(cfg.RoutingStrategy),
			ReplicationFactor:         cfg.Replication.Factor,
			RebalanceEnabled:          cfg.Rebalance.Enabled,
			RebalanceStrategy:         cfg.Rebalance.Strategy,
			RateLimitEnabled:          cfg.RateLimit.Enabled,
			EncryptionEnabled:         cfg.Encryption.Enabled,
			CompressionEnabled:        cfg.Compression.Enabled,
			IntegrityEnabled:          cfg.Integrity.Enabled,
			IntegrityVerifyOnRead:     cfg.Integrity.VerifyOnRead,
			IntegrityScrubberInterval: cfg.Integrity.ScrubberInterval,
		},
		Integrity: integrityStats{
			ChecksTotal: counterVecTotal(telemetry.IntegrityChecksTotal),
			ErrorsTotal: counterVecTotal(telemetry.IntegrityErrorsTotal),
		},
	}
	if h.backendManagement != nil {
		page.CloudflareAccounts = cloudflareUsage(h.backendManagement.List(), data)
	}
	var buf bytes.Buffer
	if err := h.templates.ExecuteTemplate(&buf, "dashboard.html", page); err != nil {
		h.log.ErrorContext(r.Context(), "failed to render dashboard", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	w.Header().Set(headerContentType, contentTypeHTML)
	_, _ = buf.WriteTo(w)
}

func (h *Handler) lifecyclePolicyPageData(ctx context.Context) ([]core.LifecyclePolicy, bool, string) {
	if h.lifecyclePolicies == nil {
		return nil, false, ""
	}
	policies, err := h.lifecyclePolicies.ListLifecyclePolicies(ctx)
	if err != nil {
		h.log.ErrorContext(ctx, "failed to load lifecycle policies for dashboard", "error", err)
		return nil, true, "无法读取自动清理策略，请检查数据库状态"
	}
	return policies, true, ""
}

// handleAPIDashboard returns dashboard data as JSON.
func (h *Handler) handleAPIDashboard(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)

	data, err := h.dashboardOps.GetData(r.Context())
	if err != nil {
		h.log.ErrorContext(r.Context(), "failed to get dashboard data", "error", err)
		httputil.WriteJSONError(w, http.StatusInternalServerError, "failed to load data")
		return
	}

	httputil.WriteJSON(w, http.StatusOK, data)
}
