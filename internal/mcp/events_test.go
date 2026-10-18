package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/mcpevents"
	kataclient "go.kenn.io/kata/pkg/client"
)

const wireProjectUID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"

type wireEventSource struct{}

func (wireEventSource) Resolve(_ context.Context, a mcpevents.Arguments) (mcpevents.Scope, error) {
	return mcpevents.Scope{Arguments: a, Projects: []mcpevents.Project{{UID: wireProjectUID, ID: 42, Name: "spoke-project"}}}, nil
}
func (wireEventSource) Authorize(context.Context, mcpevents.Scope) error { return nil }

func (wireEventSource) AuthorizeOccurrence(context.Context, mcpevents.Scope, mcpevents.Payload) error {
	return nil
}
func (wireEventSource) Head(context.Context, mcpevents.Scope) (mcpevents.Position, error) {
	return mcpevents.Position{wireProjectUID: 0}, nil
}
func (wireEventSource) Read(_ context.Context, _ mcpevents.Scope, p mcpevents.Position, _ int) (mcpevents.SourcePage, error) {
	return mcpevents.SourcePage{Positions: p}, nil
}

func newWireEventService(t *testing.T) *mcpevents.Service {
	t.Helper()
	state, err := mcpevents.OpenState(filepath.Join(t.TempDir(), "events"), mcpevents.Binding{BackendOrigin: "https://daemon.example", DaemonUID: wireProjectUID, BackendCredentialFingerprint: "synthetic-credential-hash", BackendPrincipal: "synthetic-principal", TransportPrincipal: "synthetic-http-principal", Actor: "example-agent", ScopeMode: "bound", ProjectUIDs: []string{wireProjectUID}})
	require.NoError(t, err)
	service, err := mcpevents.NewService(mcpevents.ServiceOptions{State: state, Source: wireEventSource{}, Webhook: mcpevents.NewWebhookClient()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })
	return service
}

func eventWireServer(t *testing.T, service *mcpevents.Service) *httptest.Server {
	t.Helper()
	server, err := New(Options{Client: &kataclient.Client{}, ProjectID: 42, ProjectName: "spoke-project", Actor: "example-agent", Version: "test-version", Events: service})
	require.NoError(t, err)
	httpServer := httptest.NewServer(sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return server }, &sdkmcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true}))
	t.Cleanup(httpServer.Close)
	return httpServer
}

func eventWireCall(t *testing.T, server *httptest.Server, method string, params any) map[string]any {
	t.Helper()
	data, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	require.NoError(t, err)
	return eventWireRaw(t, server, data)
}
func eventWireRaw(t *testing.T, server *httptest.Server, data []byte) map[string]any {
	t.Helper()
	// Core 2026-07-28 carries the negotiated version in each request. Preserve
	// nested raw bytes so duplicate argument names still exercise the server.
	var envelope map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(data, &envelope))
	var method string
	require.NoError(t, json.Unmarshal(envelope["method"], &method))
	params := bytes.TrimSpace(envelope["params"])
	meta := []byte(`{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}`)
	if method == "initialize" {
		// initialize remains available to legacy protocol clients.
	} else if bytes.Equal(params, []byte(`{}`)) {
		envelope["params"] = append(meta, '}')
	} else {
		envelope["params"] = append(append(meta, ','), params[1:]...)
	}
	data, err := json.Marshal(envelope)
	require.NoError(t, err)
	request, err := http.NewRequestWithContext(t.Context(), "POST", server.URL, bytes.NewReader(data))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("MCP-Protocol-Version", "2026-07-28")
	if method == "initialize" {
		request.Header.Set("MCP-Protocol-Version", "2025-11-25")
	}
	request.Header.Set("Mcp-Method", method)
	response, err := server.Client().Do(request)
	require.NoError(t, err)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(body, &decoded), "%s", body)
	if decoded["error"] == nil {
		require.Equalf(t, 200, response.StatusCode, "%s", body)
	} else {
		require.Contains(t, []int{200, 400, 404}, response.StatusCode)
	}
	return decoded
}

func TestMCPEventsDiscoveryEnabledAndDisabled(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "disabled", true: "enabled"}[enabled], func(t *testing.T) {
			var service *mcpevents.Service
			if enabled {
				service = newWireEventService(t)
			}
			server := eventWireServer(t, service)
			for _, method := range []string{"server/discover", "initialize"} {
				params := map[string]any{}
				if method == "initialize" {
					params = map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "fixture", "version": "test"}}
				}
				response := eventWireCall(t, server, method, params)
				result := response["result"].(map[string]any)
				if method == "server/discover" {
					require.Equal(t, "complete", result["resultType"])
					meta, ok := result["_meta"].(map[string]any)
					require.True(t, ok, "core2026 discovery must retain SDK annotations")
					info := meta["io.modelcontextprotocol/serverInfo"].(map[string]any)
					require.Equal(t, "kata", info["name"])
				}
				capabilities := result["capabilities"].(map[string]any)
				_, has := capabilities["events"]
				require.Equal(t, enabled, has)
			}
			listed := eventWireCall(t, server, "events/list", map[string]any{})
			if !enabled {
				require.Contains(t, listed, "error")
				return
			}
			require.NotContains(t, listed, "error")
			definitions := listed["result"].(map[string]any)["events"].([]any)
			require.Len(t, definitions, 1)
			definition := definitions[0].(map[string]any)
			require.Equal(t, "kata.inbox.changed", definition["name"])
			require.Equal(t, []any{"webhook"}, definition["delivery"])
		})
	}
}

func TestMCPEventsWireRejectsUnknownAndDuplicateFields(t *testing.T) {
	server := eventWireServer(t, newWireEventService(t))
	for _, params := range []string{`{"unknown":true}`, `{"name":"kata.inbox.changed","name":"other","arguments":{"for":"reader"}}`} {
		method := "events/subscribe"
		if params == `{"unknown":true}` {
			method = "events/list"
		}
		response := eventWireRaw(t, server, []byte(`{"jsonrpc":"2.0","id":1,"method":"`+method+`","params":`+params+`}`))
		require.Equal(t, float64(-32602), response["error"].(map[string]any)["code"])
	}
	response := eventWireCall(t, server, "events/subscribe", map[string]any{"name": "kata.inbox.changed", "arguments": map[string]any{"for": "reader", "unknown": "forbidden"}, "delivery": map[string]any{"mode": "webhook", "url": "https://receiver.example/events", "secret": "whsec_invalid"}})
	require.Equal(t, float64(-32602), response["error"].(map[string]any)["code"])
}

func TestMCPEventsDeliveryDiagnosticsAreScopedAndAnnotated(t *testing.T) {
	service := newWireEventService(t)
	session := connectTestServerWithOptions(t, Options{Client: &kataclient.Client{}, ProjectID: 42, ProjectName: "spoke-project", Actor: "example-agent", Version: "test-version", Events: service})
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.event_subscriptions", Arguments: map[string]any{}})
	require.NoError(t, err)
	require.False(t, result.IsError)
	data := mustJSON(t, result.StructuredContent)
	require.JSONEq(t, `{"subscriptions":[]}`, string(data))
	listed, err := session.ListTools(t.Context(), nil)
	require.NoError(t, err)
	seen := map[string]bool{}
	for _, tool := range listed.Tools {
		switch tool.Name {
		case "kata.event_subscriptions":
			require.True(t, tool.Annotations.ReadOnlyHint)
			seen[tool.Name] = true
		case "kata.event_delivery_retry":
			require.False(t, tool.Annotations.ReadOnlyHint)
			seen[tool.Name] = true
		}
	}
	require.Len(t, seen, 2)
	callToolError(t, session, "kata.event_delivery_retry", map[string]any{"id": "unknown"})
}
