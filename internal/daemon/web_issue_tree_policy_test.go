package daemon

import (
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWebLocalIssueTreeRoutePolicy(t *testing.T) {
	require.True(t, webLocalSPARequestAllowed(httptest.NewRequest(http.MethodPost, "/api/v1/issue-moves", nil)), "ordinary browser moves use the same complete-tree endpoint")
	require.True(t, webLocalSPARequestAllowed(httptest.NewRequest(http.MethodGet, "/api/v1/instance", nil)), "move requests resolve the selected daemon's authority")
	for _, path := range []string{"/api/v1/issue-moves/accept", "/api/v1/issue-moves/finalize", "/api/v1/issue-moves/export", "/api/v1/issue-moves/lookup"} {
		require.False(t, webLocalSPARequestAllowed(httptest.NewRequest(http.MethodPost, path, nil)), "browser clients cannot use internal transfer authority directly")
	}
}
