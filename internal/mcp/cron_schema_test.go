package mcpserver

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// MCP tool outputSchema must describe an object, including pointer responses.
func TestCronOutputSchemasAreObjects(t *testing.T) {
	session := connectRawTestServerWithOptions(t, Options{
		Client: inertTestClient(t), ProjectID: 42, ProjectName: "spoke-project",
		Actor: "example-agent", Version: "test", CronUsage: true, StartupSections: []string{"cron"},
	})
	result, err := session.ListTools(t.Context(), nil)
	require.NoError(t, err)
	foundUsage := false
	for _, tool := range result.Tools {
		if tool.Name == "kata.cron_run_usage" {
			foundUsage = true
			require.NotNil(t, tool.OutputSchema)
		}
		if tool.OutputSchema == nil {
			continue
		}
		data, err := json.Marshal(tool.OutputSchema)
		require.NoError(t, err)
		var schema map[string]any
		require.NoError(t, json.Unmarshal(data, &schema))
		require.Equal(t, "object", schema["type"], tool.Name)
	}
	require.True(t, foundUsage)
}
