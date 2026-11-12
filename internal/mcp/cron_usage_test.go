package mcpserver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	kataclient "go.kenn.io/kata/pkg/client"
)

func TestCronRunUsageWireAndVersion(t *testing.T) {
	const uid = "01HAAAAAAAAAAAAAAAAAAAAAAA"
	const body = `{"run_uid":"01HAAAAAAAAAAAAAAAAAAAAAAA","state":"complete","sessions":[],"sessions_incomplete":false,"estimated":false,"fetched_at":"2026-08-11T00:00:00Z","cost_microdollars":9007199254740993}`
	version := "0.46.0"
	requests := 0
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/health":
			_, _ = fmt.Fprintf(w, `{"ok":true,"api_schema_version":%q}`, version)
		case "/api/v1/projects/42/cron/runs/" + uid + "/usage":
			requests++
			require.Equal(t, "true", r.URL.Query().Get("refresh"))
			_, _ = w.Write([]byte(body))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(daemon.Close)
	client, err := kataclient.NewWithHTTPClient(daemon.URL, daemon.Client())
	require.NoError(t, err)
	server, err := New(Options{Client: client, ProjectID: 42, ProjectName: "spoke-project", CronUsage: true, StartupSections: []string{"cron"}})
	require.NoError(t, err)
	var wire bytes.Buffer
	session := connectServerSession(t, server, nil, &wire)
	call := func() *sdkmcp.CallToolResult {
		result, callErr := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.cron_run_usage", Arguments: map[string]any{"uid": strings.ToLower(uid), "refresh": true}})
		require.NoError(t, callErr)
		return result
	}
	result := call()
	require.False(t, result.IsError)
	require.Len(t, result.Content, 1)
	text := result.Content[0].(*sdkmcp.TextContent).Text
	for _, raw := range []string{text, wire.String()} {
		require.Contains(t, raw, "9007199254740993")
	}
	lines := bytes.Split(bytes.TrimSpace(wire.Bytes()), []byte("\n"))
	var envelope struct {
		Result struct {
			StructuredContent json.RawMessage `json:"structuredContent"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(lines[len(lines)-1], &envelope))
	require.Contains(t, string(envelope.Result.StructuredContent), "9007199254740993")
	decoder := json.NewDecoder(strings.NewReader(string(envelope.Result.StructuredContent)))
	decoder.UseNumber()
	var wireBody map[string]any
	require.NoError(t, decoder.Decode(&wireBody))
	require.Equal(t, json.Number("9007199254740993"), wireBody["cost_microdollars"])
	decoder = json.NewDecoder(strings.NewReader(text))
	decoder.UseNumber()
	var got map[string]any
	require.NoError(t, decoder.Decode(&got))
	require.Equal(t, json.Number("9007199254740993"), got["cost_microdollars"])
	require.Equal(t, 1, requests)
	for _, replacement := range []string{"0.45.0", "invalid", ""} {
		version = replacement
		refused := call()
		require.True(t, refused.IsError)
		require.Contains(t, refused.Content[0].(*sdkmcp.TextContent).Text, "0.46.0")
		require.Contains(t, refused.Content[0].(*sdkmcp.TextContent).Text, "upgrade")
		require.Equal(t, 1, requests)
	}
}

func TestCronRunUsageCatalogs(t *testing.T) {
	for _, enabled := range []bool{true, false, true} {
		options := defaultCatalogOptions(inertTestClient(t))
		options.CronUsage = enabled
		session := connectRawTestServerWithOptions(t, options)
		for _, loaded := range []bool{false, true} {
			if loaded {
				cronOK(t, session, "load_cron", map[string]any{})
			}
			search := searchToolSession(t, session, "cron run usage", 10)
			var names []string
			for _, item := range search.Results {
				names = append(names, item.Name)
			}
			require.Equal(t, enabled, containsCronUsage(names))
			tools, err := session.ListTools(t.Context(), nil)
			require.NoError(t, err)
			require.Equal(t, enabled && loaded, containsCronUsage(toolNames(tools.Tools)))
		}
	}
}

func containsCronUsage(names []string) bool {
	for _, name := range names {
		if name == "kata.cron_run_usage" {
			return true
		}
	}
	return false
}
