package dbtest

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// Finalized source rows remain audit data; ordinary read/search/mutation paths
// must not treat their old copies as current work.
func RunMovedIssueAdmission(t *testing.T, store db.Storage) {
	t.Helper()
	ctx := t.Context()
	source, err := store.CreateProject(ctx, "admission-source")
	require.NoError(t, err)
	destination, err := store.CreateProject(ctx, "admission-destination")
	require.NoError(t, err)
	issue, event, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "Relocated sentinel", Author: "tester", Owner: new("old-holder")})
	require.NoError(t, err)
	active, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "Active control", Author: "tester"})
	require.NoError(t, err)
	_, err = store.CreateLink(ctx, db.CreateLinkParams{FromIssueID: issue.ID, ToIssueID: active.ID, Type: "blocks", Author: "tester"})
	require.NoError(t, err)
	_, err = store.AddLabel(ctx, issue.ID, "old-source-label", "tester")
	require.NoError(t, err)
	q := store.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
		QueryRowContext(context.Context, string, ...any) *sql.Row
	})
	_, err = q.ExecContext(ctx, `UPDATE issues SET location_generation=2,moved_to_authority_uid=$1,moved_to_project_uid=$2,moved_to_short_id=$3,moved_event_uid=$4 WHERE id=$5`, "01AAAAAAAAAAAAAAAAAAAAAAAA", destination.UID, issue.ShortID, event.UID, issue.ID)
	require.NoError(t, err)
	t.Run("id", func(t *testing.T) { _, err := store.IssueByID(ctx, issue.ID); require.ErrorIs(t, err, db.ErrNotFound) })
	t.Run("uid", func(t *testing.T) {
		_, err := store.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
		require.ErrorIs(t, err, db.ErrNotFound)
	})
	t.Run("reference", func(t *testing.T) {
		_, err := store.IssueByShortID(ctx, source.ID, issue.ShortID, db.IncludeDeletedYes)
		require.ErrorIs(t, err, db.ErrNotFound)
	})
	t.Run("list", func(t *testing.T) {
		rows, err := store.ListIssues(ctx, db.ListIssuesParams{ProjectID: source.ID})
		require.NoError(t, err)
		require.Len(t, rows, 1)
		require.Equal(t, active.UID, rows[0].UID)
	})
	t.Run("count", func(t *testing.T) {
		count, err := store.CountIssues(ctx, db.ListIssuesParams{ProjectID: source.ID})
		require.NoError(t, err)
		require.Equal(t, int64(1), count)
	})
	t.Run("search", func(t *testing.T) {
		rows, err := store.SearchFTS(ctx, db.SearchFTSParams{ProjectID: source.ID, Query: "sentinel", Limit: 10})
		require.NoError(t, err)
		require.Empty(t, rows)
	})
	t.Run("global-count", func(t *testing.T) {
		count, err := store.CountAllIssues(ctx, db.ListAllIssuesParams{})
		require.NoError(t, err)
		require.Equal(t, int64(1), count)
	})
	t.Run("global-list", func(t *testing.T) {
		rows, err := store.ListAllIssues(ctx, db.ListAllIssuesParams{Limit: 10})
		require.NoError(t, err)
		require.Len(t, rows, 1)
		require.Equal(t, active.UID, rows[0].UID)
	})
	t.Run("ready", func(t *testing.T) {
		rows, err := store.ReadyIssues(ctx, source.ID, 10, db.ReadyIssuesFilter{})
		require.NoError(t, err)
		require.Len(t, rows, 1)
		require.Equal(t, active.UID, rows[0].UID)
	})
	t.Run("global-ready", func(t *testing.T) {
		rows, err := store.ReadyIssuesGlobal(ctx, 10, db.ReadyIssuesFilter{})
		require.NoError(t, err)
		require.Len(t, rows, 1)
		require.Equal(t, active.UID, rows[0].UID)
	})
	t.Run("embedding-feed", func(t *testing.T) {
		rows, err := store.ListIssueContent(ctx, 0, 1)
		require.NoError(t, err)
		require.Len(t, rows, 1)
		require.Equal(t, active.UID, rows[0].UID)
	})
	t.Run("browser", func(t *testing.T) {
		data, err := store.(db.UIStore).ReadUISnapshot(ctx, db.UISnapshotQuery{View: "all-open", Limit: 10})
		require.NoError(t, err)
		require.Len(t, data.Issues, 1)
		require.Equal(t, active.UID, data.Issues[0].Issue.UID)
		seen := false
		for _, project := range data.Projects {
			if project.Project.UID == source.UID {
				seen = true
				require.Equal(t, 1, project.Stats.Open)
			}
		}
		require.True(t, seen, "source project remains in the browser catalog")
	})
	t.Run("browser-references", func(t *testing.T) {
		data, err := store.(db.UIStore).ReadUIReferences(ctx, db.UIReferencesQuery{ProjectUID: source.UID, Limit: 10})
		require.NoError(t, err)
		require.Len(t, data.Issues, 1)
		require.Equal(t, active.UID, data.Issues[0].UID)
		require.Empty(t, data.Owners)
		require.Empty(t, data.Labels)
	})
	t.Run("edit", func(t *testing.T) {
		_, _, changed, err := store.EditIssue(ctx, db.EditIssueParams{IssueID: issue.ID, Title: new("Wrong owner edit"), Actor: "tester"})
		require.ErrorIs(t, err, db.ErrNotFound)
		require.False(t, changed)
	})
	current, err := store.IssueByID(ctx, active.ID)
	require.NoError(t, err)
	require.Equal(t, active.UID, current.UID)
	var title string
	require.NoError(t, q.QueryRowContext(ctx, `SELECT title FROM issues WHERE id=$1`, issue.ID).Scan(&title))
	require.Equal(t, issue.Title, title, "retained audit copy was not changed")
}

func RunMovedIssueMutations(t *testing.T, store db.Storage) {
	t.Run("soft-deleted-label", func(t *testing.T) {
		ctx := t.Context()
		project, err := store.CreateProject(ctx, "deleted-label-source")
		require.NoError(t, err)
		issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Retained label target", Author: "tester"})
		require.NoError(t, err)
		_, _, _, err = store.SoftDeleteIssue(ctx, issue.ID, "tester")
		require.NoError(t, err)
		_, err = store.AddLabel(ctx, issue.ID, "retained-label", "tester")
		require.NoError(t, err, "eventless labels retain their existing soft-deleted-row contract")
	})
	t.Helper()
	ctx := t.Context()
	for _, operation := range []string{"close", "reopen", "delete", "restore", "comment", "claim", "label", "link"} {
		t.Run(operation, func(t *testing.T) {
			source, err := store.CreateProject(ctx, "mutation-"+operation)
			require.NoError(t, err)
			target, err := store.CreateProject(ctx, "target-"+operation)
			require.NoError(t, err)
			issue, event, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "Audit copy", Author: "tester"})
			require.NoError(t, err)
			peer, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "Active peer", Author: "tester"})
			require.NoError(t, err)
			q := store.(interface {
				ExecContext(context.Context, string, ...any) (sql.Result, error)
				QueryRowContext(context.Context, string, ...any) *sql.Row
			})
			_, err = q.ExecContext(ctx, `UPDATE issues SET location_generation=2,moved_to_authority_uid=$1,moved_to_project_uid=$2,moved_to_short_id=$3,moved_event_uid=$4 WHERE id=$5`, "01AAAAAAAAAAAAAAAAAAAAAAAA", target.UID, issue.ShortID, event.UID, issue.ID)
			require.NoError(t, err)
			switch operation {
			case "close":
				_, _, _, err = store.CloseIssue(ctx, issue.ID, "done", "tester", "An old source cannot close the relocated issue.", nil)
			case "reopen":
				_, _, _, err = store.ReopenIssue(ctx, issue.ID, "tester")
			case "delete":
				_, _, _, err = store.SoftDeleteIssue(ctx, issue.ID, "tester")
			case "restore":
				_, _, _, err = store.RestoreIssue(ctx, issue.ID, "tester")
			case "comment":
				_, _, err = store.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "tester", Body: "Wrong source"})
			case "claim":
				_, err = store.ClaimOwner(ctx, db.ClaimOwnerParams{IssueID: issue.ID, Actor: "tester", Force: true})
			case "label":
				_, err = store.AddLabel(ctx, issue.ID, "wrong-source", "tester")
			case "link":
				_, err = store.CreateLink(ctx, db.CreateLinkParams{FromIssueID: issue.ID, ToIssueID: peer.ID, Type: "related", Author: "tester"})
			}
			require.ErrorIs(t, err, db.ErrNotFound, "every ordinary mutation fences the finalized copy")
			var revision int64
			var status string
			require.NoError(t, q.QueryRowContext(ctx, `SELECT revision,status FROM issues WHERE id=$1`, issue.ID).Scan(&revision, &status))
			require.Equal(t, issue.Revision, revision)
			require.Equal(t, "open", status)
		})
	}
}
