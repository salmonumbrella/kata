package mcpserver

import (
	"net/http"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// NewHTTPHandler preserves legacy sessions and accepts core2026 requests on
// the same endpoint. The SDK validates the required header/metadata agreement.
// Authentication, Host validation and cross-origin protection remain the caller's
// responsibility so both protocol paths share the same transport authority.
func NewHTTPHandler(server *sdkmcp.Server) http.Handler {
	getServer := func(*http.Request) *sdkmcp.Server { return server }
	legacy := sdkmcp.NewStreamableHTTPHandler(getServer, &sdkmcp.StreamableHTTPOptions{
		SessionTimeout: 30 * time.Minute, PropagateRequestCancellation: true,
	})
	current := sdkmcp.NewStreamableHTTPHandler(getServer, &sdkmcp.StreamableHTTPOptions{
		Stateless: true, JSONResponse: true, PropagateRequestCancellation: true,
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("MCP-Protocol-Version") >= "2026-07-28" {
			current.ServeHTTP(w, r)
			return
		}
		legacy.ServeHTTP(w, r)
	})
}
