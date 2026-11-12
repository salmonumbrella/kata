package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	kataclient "go.kenn.io/kata/pkg/client"
)

func TestCronRunUsageWireAndVersion(t *testing.T) {
	const uid = "01HAAAAAAAAAAAAAAAAAAAAAAA"
	const body = `{"run_uid":"01HAAAAAAAAAAAAAAAAAAAAAAA","state":"complete","sessions":[{"agent":"pi","session_id":"example-session","state":"priced","canonical_session_id":"example-session","cost_microdollars":9007199254740993,"cost_source":"reported"}],"sessions_incomplete":false,"estimated":false,"fetched_at":"2026-08-11T00:00:00Z","cost_microdollars":9007199254740993}`
	version := CronUsageAPIFloor
	requests := 0
	query := ""
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/health":
			_, _ = fmt.Fprintf(w, `{"ok":true,"api_schema_version":%q}`, version)
		case "/api/v1/projects":
			_, _ = w.Write([]byte(`{"projects":[{"id":42,"uid":"01HBBBBBBBBBBBBBBBBBBBBBBB","name":"spoke-project","metadata":{},"revision":1,"created_at":"2026-08-11T00:00:00Z"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(daemon.Close)
	longDaemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/projects/42/cron/runs/"+uid+"/usage" {
			http.NotFound(w, r)
			return
		}
		requests++
		query = r.URL.Query().Get("refresh")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(longDaemon.Close)
	client, err := kataclient.NewWithHTTPClient(daemon.URL, daemon.Client())
	require.NoError(t, err)
	longClient, err := kataclient.NewWithHTTPClient(longDaemon.URL, longDaemon.Client())
	require.NoError(t, err)
	options := defaultCatalogOptions(client)
	options.Scope = NewAllScope()
	options.LongRunningClient = longClient
	options.CronUsage = true
	options.StartupSections = []string{"cron"}
	server, err := New(options)
	require.NoError(t, err)
	var wire bytes.Buffer
	session := connectServerSession(t, server, nil, &wire)
	call := func() *sdkmcp.CallToolResult {
		result, callErr := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.cron_run_usage", Arguments: map[string]any{"project": "spoke-project", "uid": strings.ToLower(uid), "refresh": true}})
		require.NoError(t, callErr)
		return result
	}
	result := call()
	require.False(t, result.IsError)
	require.Len(t, result.Content, 1)
	text := result.Content[0].(*sdkmcp.TextContent).Text
	lines := bytes.Split(bytes.TrimSpace(wire.Bytes()), []byte("\n"))
	var envelope struct {
		Result struct {
			StructuredContent json.RawMessage `json:"structuredContent"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(lines[len(lines)-1], &envelope))
	decode := func(raw string) any {
		t.Helper()
		decoder := json.NewDecoder(strings.NewReader(raw))
		decoder.UseNumber()
		var value any
		require.NoError(t, decoder.Decode(&value))
		return value
	}
	require.Equal(t, decode(body), decode(text))
	require.Equal(t, decode(body), decode(string(envelope.Result.StructuredContent)))
	require.Equal(t, 1, requests)
	require.Equal(t, "true", query)
	for _, replacement := range []string{"0.45.0", ""} {
		version = replacement
		refused := call()
		require.True(t, refused.IsError)
		require.Contains(t, refused.Content[0].(*sdkmcp.TextContent).Text, CronUsageAPIFloor)
		require.Contains(t, refused.Content[0].(*sdkmcp.TextContent).Text, "upgrade")
		require.Equal(t, 1, requests)
	}
}

func TestCronRunUsageCatalogs(t *testing.T) {
	for _, enabled := range []bool{true, false, true} {
		options := defaultCatalogOptions(inertTestClient(t))
		options.CronUsage = enabled
		changed := make(chan struct{}, 1)
		session := connectRawTestServerWithClientOptions(t, options, &sdkmcp.ClientOptions{
			ToolListChangedHandler: func(context.Context, *sdkmcp.ToolListChangedRequest) {
				select {
				case changed <- struct{}{}:
				default:
				}
			},
		})
		search := searchToolSession(t, session, "cron run usage", 10)
		var names []string
		for _, item := range search.Results {
			names = append(names, item.Name)
		}
		require.Equal(t, enabled, slices.Contains(names, "kata.cron_run_usage"))
		before, err := session.ListTools(t.Context(), nil)
		require.NoError(t, err)
		require.NotContains(t, toolNames(before.Tools), "kata.cron_run_usage")
		loaded := cronOK(t, session, "load_cron", map[string]any{})
		require.Equal(t, enabled, slices.Contains(loaded["tools"].([]any), any("kata.cron_run_usage")))
		select {
		case <-changed:
		case <-time.After(time.Second):
			t.Fatal("cron loader did not announce its catalog change")
		case <-t.Context().Done():
			t.Fatal("context canceled waiting for catalog change")
		}
		tools, err := session.ListTools(t.Context(), nil)
		require.NoError(t, err)
		require.Equal(t, enabled, slices.Contains(toolNames(tools.Tools), "kata.cron_run_usage"))
		for _, tool := range tools.Tools {
			if tool.Name == "kata.cron_run_usage" {
				require.NotNil(t, tool.Annotations)
				require.NotNil(t, tool.Annotations.OpenWorldHint)
				require.True(t, *tool.Annotations.OpenWorldHint, "usage reaches the external AgentsView system")
			}
		}
	}
}
