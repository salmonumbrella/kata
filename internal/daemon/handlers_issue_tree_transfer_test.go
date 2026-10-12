package daemon_test

import (
	"bytes"
	"encoding/json/v2"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/uid"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"sync/atomic"
	"testing"
)

// A source daemon commits the complete tree at an explicitly configured,
// separately authenticated authority, verifies that commit, then finalizes.
func TestTypedIssueTreeRemoteHTTP(t *testing.T) {
	destination := testenv.New(t, testenv.WithAuthToken("destination-token"))
	destinationOrigin, err := url.Parse(destination.URL)
	require.NoError(t, err)
	proxyHandler := httputil.NewSingleHostReverseProxy(destinationOrigin)
	originalDirector := proxyHandler.Director
	proxyHandler.Director = func(r *http.Request) { originalDirector(r); r.Host = destinationOrigin.Host }
	proxy := httptest.NewServer(proxyHandler)
	defer proxy.Close()
	source := testenv.New(t, testenv.WithAuthToken("source-token"), func(cfg *daemon.ServerConfig) {
		cfg.FederationCatalog = []config.CatalogDaemonConfig{{Name: "destination", URL: proxy.URL, Token: "destination-token", InstanceUID: destination.DB.InstanceUID()}}
	})
	from, err := source.DB.CreateProject(t.Context(), "source-project")
	require.NoError(t, err)
	to, err := destination.DB.CreateProject(t.Context(), "destination-project")
	require.NoError(t, err)
	root, _, err := source.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: from.ID, Title: "Root", Author: "original-author"})
	require.NoError(t, err)
	child, _, err := source.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: from.ID, Title: "Completed child", Author: "original-author"})
	require.NoError(t, err)
	_, err = source.DB.CreateLink(t.Context(), db.CreateLinkParams{FromIssueID: child.ID, ToIssueID: root.ID, Type: "parent", Author: "original-author"})
	require.NoError(t, err)
	_, _, _, err = source.DB.CloseIssue(t.Context(), child.ID, "done", "original-author", "", nil)
	require.NoError(t, err)
	requestUID, err := uid.New()
	require.NoError(t, err)
	move := db.MoveTreeRequest{SourceAuthorityUID: source.DB.InstanceUID(), SourceProjectUID: from.UID, IssueUID: root.UID, DestinationAuthorityUID: destination.DB.InstanceUID(), DestinationProjectUID: to.UID, RequestUID: requestUID, ActorAuthorityUID: source.DB.InstanceUID(), Actor: "operator"}
	raw, err := json.Marshal(map[string]any{"request": move})
	require.NoError(t, err)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, source.URL+"/api/v1/issue-moves", bytes.NewReader(raw))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer source-token")
	response, err := source.HTTP.Do(request)
	require.NoError(t, err)
	body := readClose(t, response)
	require.Equal(t, http.StatusOK, response.StatusCode, string(body))
	var result struct {
		Result db.MoveTreeResult `json:"result"`
	}
	require.NoError(t, json.Unmarshal(body, &result))
	require.Equal(t, "completed", result.Result.State)
	require.Len(t, result.Result.Applied, 2)
	require.Len(t, result.Result.Finalized, 2)
	for _, applied := range result.Result.Applied {
		require.NotEmpty(t, applied.CommittedEventUID)
		require.NotEmpty(t, applied.CommittedEventHash)
	}
	copied, err := destination.DB.IssueByUID(t.Context(), root.UID, db.IncludeDeletedYes)
	require.NoError(t, err)
	require.Equal(t, "original-author", copied.Author)
	require.Equal(t, to.ID, copied.ProjectID)
	var authority, project string
	require.NoError(t, source.DB.QueryRowContext(t.Context(), `SELECT moved_to_authority_uid,moved_to_project_uid FROM issues WHERE uid=$1`, root.UID).Scan(&authority, &project))
	require.Equal(t, destination.DB.InstanceUID(), authority)
	require.Equal(t, to.UID, project)
	retryRequest, err := http.NewRequestWithContext(t.Context(), http.MethodPost, source.URL+"/api/v1/issue-moves", bytes.NewReader(raw))
	require.NoError(t, err)
	retryRequest.Header.Set("Content-Type", "application/json")
	retryRequest.Header.Set("Authorization", "Bearer source-token")
	retried, err := source.HTTP.Do(retryRequest)
	require.NoError(t, err)
	retryBody := readClose(t, retried)
	require.Equal(t, 200, retried.StatusCode, string(retryBody))
	var retry struct {
		Result db.MoveTreeResult `json:"result"`
	}
	require.NoError(t, json.Unmarshal(retryBody, &retry))
	require.Equal(t, "completed", retry.Result.State)
	require.Equal(t, result.Result.Applied, retry.Result.Applied)
	require.ElementsMatch(t, result.Result.Finalized, retry.Result.Finalized)
	// A finalized retry with the complete destination origin unavailable is
	// outcome-unknown, not an opaque internal error or an inferred rollback.
	proxy.Close()
	unavailableRequest, err := http.NewRequestWithContext(t.Context(), http.MethodPost, source.URL+"/api/v1/issue-moves", bytes.NewReader(raw))
	require.NoError(t, err)
	unavailableRequest.Header.Set("Content-Type", "application/json")
	unavailableRequest.Header.Set("Authorization", "Bearer source-token")
	unavailable, err := source.HTTP.Do(unavailableRequest)
	require.NoError(t, err)
	unavailableBody := readClose(t, unavailable)
	require.Equal(t, 200, unavailable.StatusCode, string(unavailableBody))
	var unknown struct {
		Result db.MoveTreeResult `json:"result"`
	}
	require.NoError(t, json.Unmarshal(unavailableBody, &unknown))
	require.Equal(t, "outcome-unknown", unknown.Result.State)
	require.Equal(t, move.RequestUID, unknown.Result.RequestUID)
	require.NotEmpty(t, unknown.Result.RetryAction)
}

// Faults are injected at an actual HTTP boundary, after the receiver has run
// its native transaction. Assertions describe the committed copies left behind.
func TestTypedIssueTreeHTTPFaults(t *testing.T) {
	for _, fault := range []string{"lost-accept-response", "source-comment-after-accept", "unreachable-acceptance", "redirect"} {
		t.Run(fault, func(t *testing.T) {
			destination := testenv.New(t, testenv.WithAuthToken("destination-token"))
			var source *testenv.Env
			var root db.Issue
			var credentialWrong atomic.Bool
			var credentialSeen atomic.Bool
			var escaped atomic.Int64
			redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { escaped.Add(1); w.WriteHeader(200) }))
			defer redirect.Close()
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				credentialSeen.Store(true)
				if r.Header.Get("Authorization") != "Bearer destination-token" {
					credentialWrong.Store(true)
				}
				if fault == "redirect" {
					http.Redirect(w, r, redirect.URL, http.StatusTemporaryRedirect)
					return
				}
				if fault == "unreachable-acceptance" && r.URL.Path != "/api/v1/instance" {
					w.WriteHeader(503)
					return
				}
				req, err := http.NewRequestWithContext(r.Context(), r.Method, destination.URL+r.URL.Path, r.Body)
				require.NoError(t, err)
				req.Header = r.Header.Clone()
				response, err := destination.HTTP.Do(req)
				require.NoError(t, err)
				defer response.Body.Close()
				if r.URL.Path == "/api/v1/issue-moves/accept" && response.StatusCode == 200 {
					if fault == "source-comment-after-accept" {
						_, _, err := source.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: root.ID, Author: "operator", Body: "Concurrent source comment"})
						require.NoError(t, err)
					}
					if fault == "lost-accept-response" {
						conn, _, err := w.(http.Hijacker).Hijack()
						require.NoError(t, err)
						conn.Close()
						return
					}
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(response.StatusCode)
				_, err = io.Copy(w, response.Body)
				require.NoError(t, err)
			}))
			defer proxy.Close()
			source = testenv.New(t, testenv.WithAuthToken("source-token"), func(cfg *daemon.ServerConfig) {
				cfg.FederationCatalog = []config.CatalogDaemonConfig{{Name: "destination", URL: proxy.URL, Token: "destination-token", InstanceUID: destination.DB.InstanceUID()}}
			})
			from, err := source.DB.CreateProject(t.Context(), "source-project")
			require.NoError(t, err)
			to, err := destination.DB.CreateProject(t.Context(), "destination-project")
			require.NoError(t, err)
			root, _, err = source.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: from.ID, Title: "Selected root", Author: "operator"})
			require.NoError(t, err)
			requestUID, err := uid.New()
			require.NoError(t, err)
			move := db.MoveTreeRequest{SourceAuthorityUID: source.DB.InstanceUID(), SourceProjectUID: from.UID, IssueUID: root.UID, DestinationAuthorityUID: destination.DB.InstanceUID(), DestinationProjectUID: to.UID, RequestUID: requestUID, ActorAuthorityUID: source.DB.InstanceUID(), Actor: "operator"}
			raw, err := json.Marshal(map[string]any{"request": move})
			require.NoError(t, err)
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, source.URL+"/api/v1/issue-moves", bytes.NewReader(raw))
			require.NoError(t, err)
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", "Bearer source-token")
			response, err := source.HTTP.Do(request)
			require.NoError(t, err)
			body := readClose(t, response)
			require.True(t, credentialSeen.Load())
			require.False(t, credentialWrong.Load(), "global source credential reached remote target")
			require.Zero(t, escaped.Load(), "followed redirect")
			if fault == "redirect" {
				require.NotEqual(t, 200, response.StatusCode)
				_, err = destination.DB.IssueByUID(t.Context(), root.UID, db.IncludeDeletedYes)
				require.ErrorIs(t, err, db.ErrNotFound)
				return
			}
			require.Equal(t, 200, response.StatusCode, string(body))
			var result struct {
				Result db.MoveTreeResult `json:"result"`
			}
			require.NoError(t, json.Unmarshal(body, &result))
			var moved *string
			require.NoError(t, source.DB.QueryRowContext(t.Context(), `SELECT moved_to_authority_uid FROM issues WHERE uid=$1`, root.UID).Scan(&moved))
			switch fault {
			case "lost-accept-response":
				require.Equal(t, "completed", result.Result.State)
				require.NotNil(t, moved)
				require.Len(t, result.Result.Applied, 1)
			case "source-comment-after-accept":
				require.Equal(t, "partial", result.Result.State)
				require.Nil(t, moved)
				require.Len(t, result.Result.Applied, 1)
				require.Len(t, result.Result.Conflicts, 1)
				_, err = destination.DB.IssueByUID(t.Context(), root.UID, db.IncludeDeletedYes)
				require.NoError(t, err)
				retryRequest, err := http.NewRequestWithContext(t.Context(), http.MethodPost, source.URL+"/api/v1/issue-moves", bytes.NewReader(raw))
				require.NoError(t, err)
				retryRequest.Header.Set("Content-Type", "application/json")
				retryRequest.Header.Set("Authorization", "Bearer source-token")
				retryResponse, err := source.HTTP.Do(retryRequest)
				require.NoError(t, err)
				retryBody := readClose(t, retryResponse)
				require.Equal(t, 200, retryResponse.StatusCode, string(retryBody))
				var retry struct {
					Result db.MoveTreeResult `json:"result"`
				}
				require.NoError(t, json.Unmarshal(retryBody, &retry))
				require.Equal(t, "partial", retry.Result.State, "changed source cannot finalize against older accepted content")
				require.NoError(t, source.DB.QueryRowContext(t.Context(), `SELECT moved_to_authority_uid FROM issues WHERE uid=$1`, root.UID).Scan(&moved))
				require.Nil(t, moved)
			case "unreachable-acceptance":
				require.Equal(t, "outcome-unknown", result.Result.State)
				require.Nil(t, moved)
				require.Empty(t, result.Result.Applied)
				require.NotEmpty(t, result.Result.UnknownRoots)
			}
		})
	}
}
