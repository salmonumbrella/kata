package main

import (
	"bytes"
	"encoding/json"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/kata/internal/mcpevents"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	internalclient "go.kenn.io/kata/internal/client"
	mcpserver "go.kenn.io/kata/internal/mcp"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

func TestMCPEventsBindingStdioRetainsRestrictedBackendPrincipal(t *testing.T) {
	resolved := internalclient.ResolvedDaemon{BaseURL: "https://DAEMON.example:443/", Token: "synthetic-backend-token", Network: "tcp"}
	info := generated.InstanceResponse{InstanceUID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", Auth: generated.AuthInfoOut{Kind: "token", Actor: new("restricted-reader")}}
	binding, err := mcpEventsBinding(resolved, info, "example-agent", "", mcpserver.ScopeBound, []string{info.InstanceUID})
	require.NoError(t, err)
	require.Equal(t, "https://daemon.example", binding.BackendOrigin)
	require.NotEmpty(t, binding.BackendCredentialFingerprint)
	require.NotEmpty(t, binding.BackendPrincipal)
	require.Contains(t, binding.TransportPrincipal, "stdio:")
	raw, err := json.Marshal(binding)
	require.NoError(t, err)
	require.NotContains(t, string(raw), resolved.Token)
	changed := resolved
	changed.Token = "synthetic-replacement-token"
	rotated, err := mcpEventsBinding(changed, info, "example-agent", "", mcpserver.ScopeBound, []string{info.InstanceUID})
	require.NoError(t, err)
	require.NotEqual(t, binding.BackendCredentialFingerprint, rotated.BackendCredentialFingerprint)
	info.Auth.Actor = new("different-reader")
	changedPrincipal, err := mcpEventsBinding(resolved, info, "example-agent", "", mcpserver.ScopeBound, []string{info.InstanceUID})
	require.NoError(t, err)
	require.NotEqual(t, binding.BackendPrincipal, changedPrincipal.BackendPrincipal)
}

func TestMCPEventsBindingPinsInboundCredentialAndScope(t *testing.T) {
	resolved := internalclient.ResolvedDaemon{BaseURL: "http://127.0.0.1:7777", Network: "tcp"}
	info := generated.InstanceResponse{InstanceUID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", Auth: generated.AuthInfoOut{Kind: "local"}}
	first, err := mcpEventsBinding(resolved, info, "example-agent", "synthetic-inbound-token", mcpserver.ScopeAll, nil)
	require.NoError(t, err)
	next, err := mcpEventsBinding(resolved, info, "example-agent", "rotated-inbound-token", mcpserver.ScopeAll, nil)
	require.NoError(t, err)
	require.NotEqual(t, first.TransportPrincipal, next.TransportPrincipal)
	require.Equal(t, first.BackendCredentialFingerprint, next.BackendCredentialFingerprint)
	require.Equal(t, "all", first.ScopeMode)
	encoded, err := json.Marshal(first)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "synthetic-inbound-token")
}

func TestMCPEventsStateDirectoryRequiresAbsolutePath(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	command := newRootCmd()
	command.SetArgs([]string{"mcp", "serve", "--events-state-dir", "relative"})
	require.ErrorContains(t, command.Execute(), "--events-state-dir must be an absolute path")
}

type cliEventListParams struct{ sdkmcp.ParamsBase }
type cliEventListResult struct {
	sdkmcp.ResultBase
	Events []mcpevents.Definition `json:"events"`
}

func TestMCPEventsHTTPStartupOpensDurableService(t *testing.T) {
	t.Setenv("KATA_AUTH_TOKEN", "")
	t.Setenv("KATA_SERVER", "")
	t.Setenv("KATA_HOME", t.TempDir())
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveMCPTestHealth(w, r) {
			return
		}
		if r.URL.Path == "/api/v1/instance" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"instance_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAV","auth":{"kind":"local"},"version":"test"}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer backend.Close()
	stateDir := filepath.Join(t.TempDir(), "events")
	endpoint := startMCPHTTPTestServer(t, backend.URL, "--events-state-dir", stateDir)
	client := newMCPHTTPTestClient()
	require.NoError(t, sdkmcp.AddSendingCustomMethod[*cliEventListParams, *cliEventListResult](client, "events/list"))
	session, err := client.Connect(t.Context(), newAuthenticatedMCPHTTPTestTransport(endpoint), nil)
	require.NoError(t, err)
	defer session.Close()
	result, err := sdkmcp.CallCustomMethod[*cliEventListParams, *cliEventListResult](t.Context(), session, "events/list", &cliEventListParams{})
	require.NoError(t, err)
	require.Len(t, result.Events, 1)
	require.Equal(t, "kata.inbox.changed", result.Events[0].Name)
	// A native2026 client must work through the real CLI listener too, while
	// the legacy session above remains connected for tool-list notifications.
	raw := []byte(`{"jsonrpc":"2.0","id":5,"method":"events/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, endpoint, bytes.NewReader(raw))
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer test-mcp-token")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("MCP-Protocol-Version", "2026-07-28")
	request.Header.Set("Mcp-Method", "events/list")
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode, "%s", body)
	require.Contains(t, string(body), "kata.inbox.changed")

	_, err = mcpevents.OpenState(stateDir, mcpevents.Binding{})
	require.ErrorIs(t, err, mcpevents.ErrStateLocked, "the process must retain exclusive ownership beyond a tool call")
	identity, err := os.ReadFile(filepath.Join(stateDir, "identity.json"))
	require.NoError(t, err)
	require.NotContains(t, string(identity), "test-mcp-token", "runtime binding must not persist the inbound bearer")
}

func TestMCPEventsBindingKeepsConfiguredAllowlist(t *testing.T) {
	const secondUID = "01BX5ZZKBKACTAV9WEVGEMMVRZ"
	info := generated.InstanceResponse{InstanceUID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", Auth: generated.AuthInfoOut{Kind: "local"}}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/instance":
			_ = json.NewEncoder(w).Encode(info)
		case "/api/v1/projects":
			_, _ = io.WriteString(w, `{"projects":[{"id":1,"uid":"01ARZ3NDEKTSV4RRFFQ69G5FAV","name":"spoke-project"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer backend.Close()
	client, err := kataclient.NewWithHTTPClient(backend.URL, backend.Client())
	require.NoError(t, err)
	scope, err := mcpserver.NewAllowlistScope([]mcpserver.ProjectIdentity{{ID: 1, UID: info.InstanceUID, Name: "spoke-project"}, {ID: 2, UID: secondUID, Name: "hub-project"}})
	require.NoError(t, err)
	resolved := internalclient.ResolvedDaemon{BaseURL: backend.URL, Network: "tcp"}
	dir := filepath.Join(t.TempDir(), "events")
	service, err := configureMCPEvents(t.Context(), dir, resolved, client, scope, "example-agent", "synthetic-inbound")
	require.NoError(t, err)
	require.NoError(t, service.Close())
	binding, err := mcpEventsBinding(resolved, info, "example-agent", "synthetic-inbound", mcpserver.ScopeAllowlist, []string{info.InstanceUID, secondUID})
	require.NoError(t, err)
	reopened, err := mcpevents.OpenState(dir, binding)
	require.NoError(t, err, "temporary catalog losses must not change the configured authority binding")
	require.NoError(t, reopened.Close())
}
