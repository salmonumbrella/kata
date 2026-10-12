package daemon_test

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/uid"
)

// A retained source child is a route to its actual authority. A subsequent
// parent move must collect current child work before one destination commit.
func TestTypedIssueTreeDistributedChild(t *testing.T) {
	for _, access := range []string{"authorized", "inaccessible", "source-finalization-rollback"} {
		t.Run(access, func(t *testing.T) { runTypedIssueTreeDistributedChild(t, access) })
	}
}
func runTypedIssueTreeDistributedChild(t *testing.T, access string) {
	open := func() *sqlitestore.Store {
		s, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "kata.db"))
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, s.Close()) })
		return s
	}
	first, second, destination := open(), open(), open()
	from, err := first.CreateProject(t.Context(), "source-project")
	require.NoError(t, err)
	intermediate, err := second.CreateProject(t.Context(), "intermediate-project")
	require.NoError(t, err)
	to, err := destination.CreateProject(t.Context(), "destination-project")
	require.NoError(t, err)
	root, _, err := first.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: from.ID, Title: "Parent", Author: "original-author"})
	require.NoError(t, err)
	child, _, err := first.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: from.ID, Title: "Child", Author: "original-author"})
	require.NoError(t, err)
	_, err = first.CreateLink(t.Context(), db.CreateLinkParams{FromIssueID: child.ID, ToIssueID: root.ID, Type: "parent", Author: "original-author"})
	require.NoError(t, err)
	requestUID, err := uid.New()
	require.NoError(t, err)
	move := db.MoveTreeRequest{SourceAuthorityUID: first.InstanceUID(), SourceProjectUID: from.UID, IssueUID: child.UID, DestinationAuthorityUID: second.InstanceUID(), DestinationProjectUID: intermediate.UID, Selection: "issue-only", RequestUID: requestUID, ActorAuthorityUID: first.InstanceUID(), Actor: "operator"}
	snapshot, err := first.ExportIssueTree(t.Context(), move)
	require.NoError(t, err)
	accepted, err := second.AcceptIssueTree(t.Context(), snapshot, move)
	require.NoError(t, err)
	verified := db.WithMoveDestinationCommitVerifier(t.Context(), func(ctx context.Context, r db.MoveTreeRequest, proposed db.MoveTreeResult) error {
		actual, err := second.LookupIssueTreeAcceptance(ctx, r)
		if err != nil {
			return err
		}
		return db.ValidateCommittedMoveEvidence(proposed, actual)
	})
	_, err = first.FinalizeIssueTree(verified, move, db.MoveTreePreview{SnapshotDigest: snapshot.Digest}, accepted)
	require.NoError(t, err)
	currentChild, err := second.IssueByUID(t.Context(), child.UID, db.IncludeDeletedYes)
	require.NoError(t, err)
	_, _, err = second.CreateComment(t.Context(), db.CreateCommentParams{IssueID: currentChild.ID, Body: "Work at the actual child authority", Author: "comment-author"})
	require.NoError(t, err)
	destinationURL := serveIssueMoveStore(t, daemon.ServerConfig{DB: destination, Auth: config.AuthConfig{Token: "destination-token"}})
	destinationRoute := config.CatalogDaemonConfig{Name: "destination", URL: destinationURL, Token: "destination-token", InstanceUID: destination.InstanceUID()}
	secondURL := serveIssueMoveStore(t, daemon.ServerConfig{DB: second, Auth: config.AuthConfig{Token: "second-token"}, FederationCatalog: []config.CatalogDaemonConfig{destinationRoute}})
	secondToken := "second-token"
	if access == "inaccessible" {
		secondToken = "revoked-token"
	}
	firstURL := serveIssueMoveStore(t, daemon.ServerConfig{DB: first, Auth: config.AuthConfig{Token: "source-token"}, FederationCatalog: []config.CatalogDaemonConfig{destinationRoute, {Name: "child-authority", URL: secondURL, Token: secondToken, InstanceUID: second.InstanceUID()}}})
	move.RequestUID, err = uid.New()
	require.NoError(t, err)
	move.IssueUID = root.UID
	move.Selection = ""
	move.DestinationAuthorityUID = destination.InstanceUID()
	move.DestinationProjectUID = to.UID
	if access == "source-finalization-rollback" {
		_, err = first.ExecContext(t.Context(), `CREATE TRIGGER reject_source_move BEFORE INSERT ON events WHEN NEW.type='issue.moved' AND NEW.issue_uid='`+root.UID+`' BEGIN SELECT RAISE(ABORT,'source write fault'); END`)
		require.NoError(t, err)
	}
	raw, err := json.Marshal(map[string]any{"request": move})
	require.NoError(t, err)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, firstURL+"/api/v1/issue-moves", bytes.NewReader(raw))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer source-token")
	response, err := (&http.Client{}).Do(request)
	require.NoError(t, err)
	body := readClose(t, response)
	if access == "inaccessible" {
		require.Equal(t, 404, response.StatusCode, string(body))
		var unavailable struct {
			Error struct {
				Code    string         `json:"code"`
				Message string         `json:"message"`
				Data    map[string]any `json:"data"`
			} `json:"error"`
		}
		require.NoError(t, json.Unmarshal(body, &unavailable))
		require.Equal(t, "issue_move_unavailable", unavailable.Error.Code)
		require.Equal(t, "selected work is unavailable", unavailable.Error.Message)
		require.Empty(t, unavailable.Error.Data)
		for _, selected := range []db.Issue{root, child} {
			_, err := destination.IssueByUID(t.Context(), selected.UID, db.IncludeDeletedYes)
			require.ErrorIs(t, err, db.ErrNotFound)
		}
		return
	}
	require.Equal(t, 200, response.StatusCode, string(body))
	var out struct {
		Result db.MoveTreeResult `json:"result"`
	}
	require.NoError(t, json.Unmarshal(body, &out))
	if access == "source-finalization-rollback" {
		require.Equal(t, "partial", out.Result.State)
		require.ElementsMatch(t, []string{child.UID}, out.Result.Finalized)
		require.Len(t, out.Result.Applied, 2)
		originalEvidence := out.Result.Applied
		_, err = first.ExecContext(t.Context(), `DROP TRIGGER reject_source_move`)
		require.NoError(t, err)
		request, err = http.NewRequestWithContext(t.Context(), http.MethodPost, firstURL+"/api/v1/issue-moves", bytes.NewReader(raw))
		require.NoError(t, err)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer source-token")
		response, err = (&http.Client{}).Do(request)
		require.NoError(t, err)
		body = readClose(t, response)
		require.Equal(t, 200, response.StatusCode, string(body))
		require.NoError(t, json.Unmarshal(body, &out))
		require.Equal(t, originalEvidence, out.Result.Applied, "resume must retain the actual committed destination events")
	}
	require.Equal(t, "completed", out.Result.State)
	require.Len(t, out.Result.Applied, 2)
	require.ElementsMatch(t, []string{root.UID, child.UID}, out.Result.Finalized)
	receivedChild, err := destination.IssueByUID(t.Context(), child.UID, db.IncludeDeletedYes)
	require.NoError(t, err)
	comments, err := destination.CommentsByIssue(t.Context(), receivedChild.ID)
	require.NoError(t, err)
	require.Len(t, comments, 1)
	require.Equal(t, "Work at the actual child authority", comments[0].Body)
	var moved string
	require.NoError(t, second.QueryRowContext(t.Context(), `SELECT moved_to_authority_uid FROM issues WHERE uid=$1`, child.UID).Scan(&moved))
	require.Equal(t, destination.InstanceUID(), moved)
}
