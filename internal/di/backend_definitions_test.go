package di

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/afreidah/s3-orchestrator/internal/backendmanage"
	"github.com/afreidah/s3-orchestrator/internal/config"
)

func TestMergeBackendDefinitionsManagedDefinitionAdoptsStaticSeed(t *testing.T) {
	t.Parallel()
	staticDefinitions := []backendmanage.Definition{
		{Version: 1, Backend: config.BackendConfig{Name: "primary", ProxyURL: "http://old.example:8080"}},
		{Version: 1, Backend: config.BackendConfig{Name: "secondary"}},
	}
	managedDefinitions := []backendmanage.Definition{
		{Version: 1, Managed: true, Backend: config.BackendConfig{Name: "primary", ProxyURL: "socks5://new.example:1080"}},
		{Version: 1, Managed: true, Backend: config.BackendConfig{Name: "dynamic"}},
	}

	definitions := mergeBackendDefinitions(staticDefinitions, managedDefinitions)

	require.Len(t, definitions, 3)
	assert.Equal(t, []string{"primary", "secondary", "dynamic"}, []string{
		definitions[0].Backend.Name, definitions[1].Backend.Name, definitions[2].Backend.Name,
	})
	assert.True(t, definitions[0].Managed)
	assert.Equal(t, "socks5://new.example:1080", definitions[0].Backend.ProxyURL)
}
