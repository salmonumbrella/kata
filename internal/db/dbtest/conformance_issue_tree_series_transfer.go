package dbtest

import (
	"context"
	"database/sql"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
	"testing"
)

// Whole-series transfer includes the actual scheduler row, original series
// history and purged occurrence identities without recreating their bodies.
func RunIssueTreeSeriesTransfer(t *testing.T, source, destination db.Storage) {
	ctx := t.Context()
	from, err := source.CreateProject(ctx, "source-project")
	require.NoError(t, err)
	to, err := destination.CreateProject(ctx, "target-project")
	require.NoError(t, err)
	root, _, err := source.CreateIssue(ctx, db.CreateIssueParams{ProjectID: from.ID, Title: "First occurrence", Author: "original-author"})
	require.NoError(t, err)
	attached, err := source.CreateRecurrenceForIssue(ctx, db.CreateRecurrenceForIssueIn{IssueID: root.ID, Recurrence: db.CreateRecurrenceIn{ProjectID: from.ID, Actor: "original-author", Rule: "FREQ=DAILY", DTStart: "2026-10-12", Timezone: "UTC", Template: db.RecurrenceTemplate{Title: "Daily task"}}})
	require.NoError(t, err)
	materialized, err := source.MaterializeNext(ctx, attached.Recurrence.ID, "2026-10-13", "scheduler")
	require.NoError(t, err)
	purged, err := source.IssueByUID(ctx, materialized.NewIssueUID, db.IncludeDeletedYes)
	require.NoError(t, err)
	_, _, _, err = source.SoftDeleteIssue(ctx, purged.ID, "operator")
	require.NoError(t, err)
	tombstone, err := source.PurgeIssue(ctx, purged.ID, "operator", nil)
	require.NoError(t, err)
	requestUID, err := uid.New()
	require.NoError(t, err)
	request := db.MoveTreeRequest{SourceAuthorityUID: source.InstanceUID(), SourceProjectUID: from.UID, IssueUID: root.UID, DestinationAuthorityUID: destination.InstanceUID(), DestinationProjectUID: to.UID, RequestUID: requestUID, ActorAuthorityUID: source.InstanceUID(), Actor: "operator", MoveSeries: true}
	snapshot, err := source.ExportIssueTree(ctx, request)
	require.NoError(t, err)
	require.Len(t, snapshot.Portions[0].Recurrences, 1)
	require.Len(t, snapshot.Portions[0].Tombstones, 1)
	var seriesHistory bool
	for _, event := range snapshot.Portions[0].Events {
		if event.Type == "recurrence.created" {
			seriesHistory = true
		}
	}
	require.True(t, seriesHistory, "original series history belongs to the selection")
	accepted, err := destination.AcceptIssueTree(ctx, snapshot, request)
	require.NoError(t, err)
	q := destination.(interface {
		QueryRowContext(context.Context, string, ...any) *sql.Row
	})
	var seriesID, projectID int64
	var rule, author string
	require.NoError(t, q.QueryRowContext(ctx, `SELECT id,project_id,rrule,author FROM recurrences WHERE uid=$1`, attached.Recurrence.UID).Scan(&seriesID, &projectID, &rule, &author))
	require.Equal(t, to.ID, projectID)
	require.Equal(t, "FREQ=DAILY", rule)
	require.Equal(t, "original-author", author)
	var retainedUID string
	require.NoError(t, q.QueryRowContext(ctx, `SELECT uid FROM purge_log WHERE issue_uid=$1 AND recurrence_uid=$2`, purged.UID, attached.Recurrence.UID).Scan(&retainedUID))
	require.Equal(t, tombstone.UID, retainedUID)
	_, err = destination.IssueByUID(ctx, purged.UID, db.IncludeDeletedYes)
	require.ErrorIs(t, err, db.ErrNotFound)
	verified := db.WithMoveDestinationCommitVerifier(ctx, func(ctx context.Context, r db.MoveTreeRequest, provided db.MoveTreeResult) error {
		actual, err := destination.LookupIssueTreeAcceptance(ctx, r)
		if err != nil {
			return err
		}
		return db.ValidateCommittedMoveEvidence(provided, actual)
	})
	finalized, err := source.FinalizeIssueTree(verified, request, db.MoveTreePreview{SnapshotDigest: snapshot.Digest, Members: snapshot.Portions[0].Members}, accepted)
	require.NoError(t, err)
	require.Equal(t, "completed", finalized.State)
	src := source.(interface {
		QueryRowContext(context.Context, string, ...any) *sql.Row
	})
	var movedAuthority, movedProject, movedEvent string
	require.NoError(t, src.QueryRowContext(ctx, `SELECT moved_to_authority_uid,moved_to_project_uid,moved_event_uid FROM recurrences WHERE uid=$1`, attached.Recurrence.UID).Scan(&movedAuthority, &movedProject, &movedEvent))
	require.Equal(t, destination.InstanceUID(), movedAuthority)
	require.Equal(t, to.UID, movedProject)
	require.NotEmpty(t, movedEvent)
	future, err := destination.MaterializeNext(ctx, seriesID, "2026-10-14", "scheduler")
	require.NoError(t, err)
	require.NotEmpty(t, future.NewIssueUID)
	// Reversal retains the scheduler's original identity and tombstone, and
	// includes the occurrence materialized at its current authority.
	reverse := request
	reverse.SourceAuthorityUID, reverse.SourceProjectUID = destination.InstanceUID(), to.UID
	reverse.DestinationAuthorityUID, reverse.DestinationProjectUID = source.InstanceUID(), from.UID
	reverse.ActorAuthorityUID = destination.InstanceUID()
	reverse.RequestUID, err = uid.New()
	require.NoError(t, err)
	returnedSnapshot, err := destination.ExportIssueTree(ctx, reverse)
	require.NoError(t, err)
	returned, err := source.AcceptIssueTree(ctx, returnedSnapshot, reverse)
	require.NoError(t, err)
	returnVerified := db.WithMoveDestinationCommitVerifier(ctx, func(ctx context.Context, r db.MoveTreeRequest, proposed db.MoveTreeResult) error {
		actual, err := source.LookupIssueTreeAcceptance(ctx, r)
		if err != nil {
			return err
		}
		return db.ValidateCommittedMoveEvidence(proposed, actual)
	})
	finished, err := destination.FinalizeIssueTree(returnVerified, reverse, db.MoveTreePreview{SnapshotDigest: returnedSnapshot.Digest}, returned)
	require.NoError(t, err)
	require.Equal(t, "completed", finished.State)
	require.Len(t, finished.Applied, 2)
	var returnedSeriesID int64
	require.NoError(t, src.QueryRowContext(ctx, `SELECT id FROM recurrences WHERE uid=$1 AND moved_to_authority_uid IS NULL`, attached.Recurrence.UID).Scan(&returnedSeriesID))
	require.NoError(t, src.QueryRowContext(ctx, `SELECT uid FROM purge_log WHERE issue_uid=$1`, purged.UID).Scan(&retainedUID))
	require.Equal(t, tombstone.UID, retainedUID)
	_, err = source.IssueByUID(ctx, purged.UID, db.IncludeDeletedYes)
	require.ErrorIs(t, err, db.ErrNotFound)
	next, err := source.MaterializeNext(ctx, returnedSeriesID, "2026-10-15", "scheduler")
	require.NoError(t, err)
	require.NotEmpty(t, next.NewIssueUID)
}
