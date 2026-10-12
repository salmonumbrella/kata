package daemon_test

import (
	"bytes"
	"encoding/json/v2"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/uid"
	"net/http"
	"testing"
)

// Setup and other clients use UID endpoints independently of workspace paths.
func TestTypedIssueTreeEndpoint(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("test-token"))
	source, target, root := seedMovePair(t, env)
	child, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.ID, Title: "Completed child", Author: "original-author"})
	require.NoError(t, err)
	_, err = env.DB.CreateLink(t.Context(), db.CreateLinkParams{FromIssueID: child.ID, ToIssueID: root.ID, Type: "parent", Author: "original-author"})
	require.NoError(t, err)
	_, _, _, err = env.DB.CloseIssue(t.Context(), child.ID, "done", "original-author", "", nil)
	require.NoError(t, err)
	requestUID, err := uid.New()
	require.NoError(t, err)
	move := db.MoveTreeRequest{SourceAuthorityUID: env.DB.InstanceUID(), SourceProjectUID: source.UID, IssueUID: root.UID, DestinationAuthorityUID: env.DB.InstanceUID(), DestinationProjectUID: target.UID, RequestUID: requestUID, ActorAuthorityUID: env.DB.InstanceUID(), Actor: "tester"}
	call := func(dry bool, token string) *http.Response {
		raw, err := json.Marshal(map[string]any{"request": move, "dry_run": dry})
		require.NoError(t, err)
		request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, env.URL+"/api/v1/issue-moves", bytes.NewReader(raw))
		require.NoError(t, err)
		request.Header.Set("Content-Type", "application/json")
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		response, err := env.HTTP.Do(request)
		require.NoError(t, err)
		return response
	}
	response := call(true, "")
	raw := readClose(t, response)
	require.Equal(t, http.StatusUnauthorized, response.StatusCode, string(raw))
	response = call(true, "test-token")
	raw = readClose(t, response)
	require.Equal(t, http.StatusOK, response.StatusCode, string(raw))
	var preview struct {
		Preview db.MoveTreePreview `json:"preview"`
	}
	require.NoError(t, json.Unmarshal(raw, &preview))
	require.Len(t, preview.Preview.Members, 2)
	unchanged, err := env.DB.IssueByID(t.Context(), root.ID)
	require.NoError(t, err)
	require.Equal(t, source.ID, unchanged.ProjectID)
	move.PreviewDigest = preview.Preview.Digest
	response = call(false, "test-token")
	raw = readClose(t, response)
	require.Equal(t, http.StatusOK, response.StatusCode, string(raw))
	var result struct {
		Result db.MoveTreeResult `json:"result"`
	}
	require.NoError(t, json.Unmarshal(raw, &result))
	require.Equal(t, "completed", result.Result.State)
	require.Equal(t, requestUID, result.Result.RequestUID)
	require.Len(t, result.Result.Applied, 2)
	for _, applied := range result.Result.Applied {
		require.NotEmpty(t, applied.CommittedEventUID)
		require.NotEmpty(t, applied.CommittedEventHash)
	}
	response = call(false, "test-token")
	raw = readClose(t, response)
	require.Equal(t, http.StatusOK, response.StatusCode, string(raw))
	var retry struct {
		Result db.MoveTreeResult `json:"result"`
	}
	require.NoError(t, json.Unmarshal(raw, &retry))
	require.Equal(t, result.Result.Applied, retry.Result.Applied)
}

// An exact request retry returns evidence without replaying mutation hooks.
func TestTypedIssueTreeRetryDoesNotDeliverAgain(t *testing.T) {
	sink := &recordingSink{}
	env := testenv.New(t, testenv.WithAuthToken("test-token"), func(cfg *daemon.ServerConfig) { cfg.Hooks = sink })
	source, target, root := seedMovePair(t, env)
	requestUID, err := uid.New()
	require.NoError(t, err)
	move := db.MoveTreeRequest{SourceAuthorityUID: env.DB.InstanceUID(), SourceProjectUID: source.UID, IssueUID: root.UID, DestinationAuthorityUID: env.DB.InstanceUID(), DestinationProjectUID: target.UID, RequestUID: requestUID, ActorAuthorityUID: env.DB.InstanceUID(), Actor: "tester"}
	raw, err := json.Marshal(map[string]any{"request": move})
	require.NoError(t, err)
	call := func() {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, env.URL+"/api/v1/issue-moves", bytes.NewReader(raw))
		require.NoError(t, err)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer test-token")
		response, err := env.HTTP.Do(request)
		require.NoError(t, err)
		body := readClose(t, response)
		require.Equal(t, http.StatusOK, response.StatusCode, string(body))
	}
	call()
	require.Len(t, sink.snapshot(), 1)
	call()
	require.Len(t, sink.snapshot(), 1, "an existing committed fact is not a new mutation")
}

// A user token must receive the committed result rather than a post-commit 404.
func TestTypedIssueTreeScopedPrincipalGetsCommit(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("owner-token"), testenv.WithRequireTokenIdentity())
	source, target, root := seedMovePair(t, env)
	_, _, err := env.DB.CreateAPIToken(t.Context(), db.CreateAPITokenParams{PlaintextToken: "member-token", Actor: "member", AdminActor: "admin"})
	require.NoError(t, err)
	requestUID, err := uid.New()
	require.NoError(t, err)
	move := db.MoveTreeRequest{SourceAuthorityUID: env.DB.InstanceUID(), SourceProjectUID: source.UID, IssueUID: root.UID, DestinationAuthorityUID: env.DB.InstanceUID(), DestinationProjectUID: target.UID, RequestUID: requestUID, ActorAuthorityUID: env.DB.InstanceUID(), Actor: "member"}
	raw, err := json.Marshal(map[string]any{"request": move})
	require.NoError(t, err)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, env.URL+"/api/v1/issue-moves", bytes.NewReader(raw))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer member-token")
	response, err := env.HTTP.Do(request)
	require.NoError(t, err)
	body := readClose(t, response)
	require.Equal(t, http.StatusOK, response.StatusCode, string(body))
	var result struct {
		Result db.MoveTreeResult `json:"result"`
	}
	require.NoError(t, json.Unmarshal(body, &result))
	require.Equal(t, "completed", result.Result.State)
	require.Equal(t, requestUID, result.Result.RequestUID)
	moved, err := env.DB.IssueByUID(t.Context(), root.UID, db.IncludeDeletedYes)
	require.NoError(t, err)
	require.Equal(t, target.ID, moved.ProjectID)
}
