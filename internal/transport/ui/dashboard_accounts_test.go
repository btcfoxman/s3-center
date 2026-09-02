package ui

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/afreidah/s3-orchestrator/internal/backendmanage"
	"github.com/afreidah/s3-orchestrator/internal/proxy/dashboard"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

func TestCloudflareUsage_GroupsBucketsByAccountAndRaisesWarning(t *testing.T) {
	const accountID = "0123456789abcdef0123456789abcdef"
	views := []backendmanage.View{
		{
			Name: "r2-a", Provider: "cloudflare_r2", AccountLabel: "Team R2",
			AccountID: accountID, Bucket: "bucket-a", QuotaGroup: "cf-" + accountID,
			FreeTierBytes: 10_000_000_000, AdmissibleBytes: 8_000_000_000,
			WarningPercent: 80, Proxy: "http://proxy-a.example:8080", Managed: true,
		},
		{
			Name: "r2-b", Provider: "cloudflare_r2", AccountLabel: "Team R2",
			AccountID: accountID, Bucket: "bucket-b", QuotaGroup: "cf-" + accountID,
			FreeTierBytes: 10_000_000_000, AdmissibleBytes: 8_000_000_000,
			WarningPercent: 80, Proxy: "http://proxy-b.example:8080", Managed: true,
		},
	}
	data := &dashboard.Data{
		QuotaStats: map[string]core.QuotaStat{
			"r2-a": {BytesUsed: 4_000_000_000, OrphanBytes: 500_000_000},
			"r2-b": {BytesUsed: 4_000_000_000},
		},
		UsageStats: map[string]core.UsageStat{
			"r2-a": {APIRequests: 12},
			"r2-b": {APIRequests: 8},
		},
	}

	accounts := cloudflareUsage(views, data)
	require.Len(t, accounts, 1)
	account := accounts[0]
	assert.Equal(t, "Team R2", account.Label)
	assert.Equal(t, []string{"r2-a", "r2-b"}, account.Backends)
	assert.Equal(t, []string{"bucket-a", "bucket-b"}, account.Buckets)
	assert.Equal(t, int64(8_500_000_000), account.UsedBytes)
	assert.Equal(t, int64(500_000_000), account.OrphanBytes)
	assert.Equal(t, int64(20), account.ObservedRequests)
	assert.Equal(t, int64(1_500_000_000), account.FreeRemainingBytes)
	assert.Zero(t, account.SafeRemainingBytes)
	assert.InDelta(t, 85, account.StoragePercent, 0.001)
	assert.Equal(t, "warning", account.State)
	assert.Equal(t, "混合出口", account.Proxy)
	assert.True(t, account.ProxyEditable)
	assert.Equal(t, accountID, account.AccountID)
}

func TestCloudflareUsage_MarksFreeTierOverflowCritical(t *testing.T) {
	const accountID = "fedcba9876543210fedcba9876543210"
	accounts := cloudflareUsage([]backendmanage.View{{
		Name: "r2-full", Provider: "cloudflare_r2", AccountID: accountID,
		Bucket: "full-bucket", QuotaGroup: "cf-" + accountID,
		FreeTierBytes: 10_000, AdmissibleBytes: 8_000, WarningPercent: 80,
	}}, &dashboard.Data{
		QuotaStats: map[string]core.QuotaStat{"r2-full": {BytesUsed: 10_001}},
		UsageStats: map[string]core.UsageStat{},
	})

	require.Len(t, accounts, 1)
	assert.Equal(t, "critical", accounts[0].State)
	assert.Zero(t, accounts[0].FreeRemainingBytes)
}

func TestCloudflareUsage_AllowsProxyEditingWhenAccountContainsStaticBackend(t *testing.T) {
	const accountID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	accounts := cloudflareUsage([]backendmanage.View{
		{Name: "managed", Provider: "cloudflare_r2", AccountID: accountID, Managed: true},
		{Name: "static", Provider: "cloudflare_r2", AccountID: accountID, Managed: false},
	}, &dashboard.Data{QuotaStats: map[string]core.QuotaStat{}, UsageStats: map[string]core.UsageStat{}})

	require.Len(t, accounts, 1)
	assert.True(t, accounts[0].Managed)
	assert.True(t, accounts[0].ProxyEditable)
}

func TestCloudflareUsage_DisablesProxyEditingWithoutAccountID(t *testing.T) {
	t.Parallel()
	accounts := cloudflareUsage([]backendmanage.View{
		{Name: "legacy", Provider: "cloudflare_r2", QuotaGroup: "legacy", Managed: false},
	}, &dashboard.Data{QuotaStats: map[string]core.QuotaStat{}, UsageStats: map[string]core.UsageStat{}})

	require.Len(t, accounts, 1)
	assert.False(t, accounts[0].ProxyEditable)
}
