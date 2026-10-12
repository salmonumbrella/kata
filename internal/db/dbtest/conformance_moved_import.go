package dbtest

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// A retained provider mapping suppresses source polling even after its old
// issue body is purged. It must never recreate source work or emit mutations.
func RunMovedImportSuppression(t *testing.T, store db.Storage) {
	for _, purged := range []bool{false, true} {
		name := "retained"
		if purged {
			name = "purged"
		}
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			project, err := store.CreateProject(ctx, "provider-"+name)
			require.NoError(t, err)
			destination, err := store.CreateProject(ctx, "provider-target-"+name)
			require.NoError(t, err)
			instant := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
			p := db.ImportBatchParams{ProjectID: project.ID, Source: "example-provider", Actor: "importer", Items: []db.ImportItem{{ExternalID: "source-1", Title: "Original body", Author: "tester", Status: "open", CreatedAt: instant, UpdatedAt: instant}}}
			initial, _, err := store.ImportBatch(ctx, p)
			require.NoError(t, err)
			require.Equal(t, 1, initial.Created)
			mapping, err := store.ImportMappingBySource(ctx, project.ID, p.Source, "issue", "source-1")
			require.NoError(t, err)
			require.NotNil(t, mapping.IssueID)
			issue, err := store.IssueByID(ctx, *mapping.IssueID)
			require.NoError(t, err)
			q := store.(interface {
				ExecContext(context.Context, string, ...any) (sql.Result, error)
				QueryRowContext(context.Context, string, ...any) *sql.Row
			})
			_, err = q.ExecContext(ctx, `UPDATE import_mappings SET moved_issue_uid=$1,moved_to_authority_uid=$2,moved_to_project_uid=$3 WHERE id=$4`, issue.UID, "01AAAAAAAAAAAAAAAAAAAAAAAA", destination.UID, mapping.ID)
			require.NoError(t, err)
			if purged {
				_, _, _, err = store.SoftDeleteIssue(ctx, issue.ID, "tester")
				require.NoError(t, err)
				_, err = store.PurgeIssue(ctx, issue.ID, "tester", nil)
				require.NoError(t, err)
			}
			p.Items[0].Title = "Wrong source recreation"
			p.Items[0].UpdatedAt = instant.Add(time.Hour)
			p.Items[0].Comments = []db.ImportComment{{ExternalID: "new-comment", Author: "tester", Body: "Wrong owner", CreatedAt: instant}}
			p.Items[0].Labels = []string{"wrong-source"}
			result, events, err := store.ImportBatch(ctx, p)
			require.NoError(t, err, "moved suppression is a successful skipped observation, including purged owners")
			require.Zero(t, result.Created)
			require.Zero(t, result.Updated)
			require.Equal(t, 1, result.Unchanged)
			require.Empty(t, events)
			require.Len(t, result.Items, 1)
			require.Equal(t, "moved", result.Items[0].Reason)
			var title string
			err = q.QueryRowContext(ctx, `SELECT title FROM issues WHERE uid=$1`, issue.UID).Scan(&title)
			if purged {
				require.ErrorIs(t, err, sql.ErrNoRows)
			} else {
				require.NoError(t, err)
				require.Equal(t, "Original body", title)
			}
		})
	}
}

// Actual local moves retain source suppression, not just destination mappings.
// Reverse moves reclaim that same external identity without duplicating work.
func RunMoveTreeProviderSourcePolling(t *testing.T, store db.Storage) {
	ctx := t.Context()
	source, err := store.CreateProject(ctx, "poll-source")
	require.NoError(t, err)
	destination, err := store.CreateProject(ctx, "poll-target")
	require.NoError(t, err)
	instant := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	p := db.ImportBatchParams{ProjectID: source.ID, Source: "example-provider", Actor: "importer", Items: []db.ImportItem{{ExternalID: "source-1", Title: "Original imported body", Author: "tester", Status: "open", CreatedAt: instant, UpdatedAt: instant}}}
	_, _, err = store.ImportBatch(ctx, p)
	require.NoError(t, err)
	original, err := store.ImportMappingBySource(ctx, source.ID, p.Source, "issue", "source-1")
	require.NoError(t, err)
	require.NotNil(t, original.IssueID)
	issue, err := store.IssueByID(ctx, *original.IssueID)
	require.NoError(t, err)
	requestUID, err := uid.New()
	require.NoError(t, err)
	preview, err := store.PreviewIssueTree(ctx, db.MoveTreeRequest{SourceAuthorityUID: store.InstanceUID(), SourceProjectUID: source.UID, IssueUID: issue.UID, DestinationAuthorityUID: store.InstanceUID(), DestinationProjectUID: destination.UID, RequestUID: requestUID, ActorAuthorityUID: store.InstanceUID(), Actor: "tester"})
	require.NoError(t, err)
	require.NotEmpty(t, preview.ProviderActions, "preview explains the imported issue's provider ownership")
	require.Equal(t, "preserve-provider", preview.ProviderActions[0].Kind)
	mover := store.(interface {
		MoveIssueTreeLocal(context.Context, db.MoveIssueTreeIn) (db.MoveIssueTreeOut, error)
	})
	moved, err := mover.MoveIssueTreeLocal(ctx, db.MoveIssueTreeIn{MoveIssueProjectIn: db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: destination.ID, IfMatchRev: issue.Revision, Actor: "tester"}})
	require.NoError(t, err)
	p.Items[0].Title = "Wrong source duplication"
	p.Items[0].UpdatedAt = instant.Add(time.Hour)
	result, events, err := store.ImportBatch(ctx, p)
	require.NoError(t, err)
	require.Zero(t, result.Created)
	require.Zero(t, result.Updated)
	require.Empty(t, events)
	require.Len(t, result.Items, 1)
	require.Equal(t, "moved", result.Items[0].Reason)
	suppression, err := store.ImportMappingBySource(ctx, source.ID, p.Source, "issue", "source-1")
	require.NoError(t, err)
	require.Equal(t, issue.UID, *suppression.MovedIssueUID)
	require.Nil(t, suppression.IssueID)
	current, err := store.ImportMappingBySource(ctx, destination.ID, p.Source, "issue", "source-1")
	require.NoError(t, err)
	require.Equal(t, original.ID, current.ID)
	require.Nil(t, current.MovedIssueUID)
	returned, err := mover.MoveIssueTreeLocal(ctx, db.MoveIssueTreeIn{MoveIssueProjectIn: db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: destination.ID, ToProjectID: source.ID, IfMatchRev: moved.Root.NewRevision, Actor: "tester"}})
	require.NoError(t, err, "reverse moves reclaim their own suppression, never another identity")
	require.Equal(t, issue.UID, returned.Root.Issue.UID)
	current, err = store.ImportMappingBySource(ctx, source.ID, p.Source, "issue", "source-1")
	require.NoError(t, err)
	require.Equal(t, original.ID, current.ID)
	require.Nil(t, current.MovedIssueUID)
	p.ProjectID = destination.ID
	result, events, err = store.ImportBatch(ctx, p)
	require.NoError(t, err)
	require.Zero(t, result.Created)
	require.Zero(t, result.Updated)
	require.Empty(t, events)
	rows, err := store.ListAllIssues(ctx, db.ListAllIssuesParams{Limit: 10})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, issue.UID, rows[0].UID)
}
