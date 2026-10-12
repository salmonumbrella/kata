package dbtest

import (
	"context"
	"database/sql"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
	"testing"
)

// A committed destination receipt is required before source finalization;
// changes to comments (not merely issue revision) leave the source intact.
func RunIssueTreeFinalization(t *testing.T, source, destination db.Storage) {
	ctx := t.Context()
	from, err := source.CreateProject(ctx, "source-project")
	require.NoError(t, err)
	to, err := destination.CreateProject(ctx, "target-project")
	require.NoError(t, err)
	root, _, err := source.CreateIssue(ctx, db.CreateIssueParams{ProjectID: from.ID, Title: "Root", Author: "author"})
	require.NoError(t, err)
	requestUID, err := uid.New()
	require.NoError(t, err)
	request := db.MoveTreeRequest{SourceAuthorityUID: source.InstanceUID(), SourceProjectUID: from.UID, IssueUID: root.UID, DestinationAuthorityUID: destination.InstanceUID(), DestinationProjectUID: to.UID, RequestUID: requestUID, ActorAuthorityUID: source.InstanceUID(), Actor: "operator"}
	snapshot, err := source.ExportIssueTree(ctx, request)
	require.NoError(t, err)
	preview := db.MoveTreePreview{SnapshotDigest: snapshot.Digest, Members: snapshot.Portions[0].Members}
	accepted, err := destination.AcceptIssueTree(ctx, snapshot, request)
	require.NoError(t, err)
	_, err = source.FinalizeIssueTree(ctx, request, preview, accepted)
	require.ErrorIs(t, err, db.ErrMoveCommitUnverified)
	verified := db.WithMoveDestinationCommitVerifier(ctx, func(ctx context.Context, r db.MoveTreeRequest, provided db.MoveTreeResult) error {
		actual, err := destination.LookupIssueTreeAcceptance(ctx, r)
		if err != nil {
			return err
		}
		require.Equal(t, provided.Applied, actual.Applied)
		return nil
	})
	result, err := source.FinalizeIssueTree(verified, request, preview, accepted)
	require.NoError(t, err)
	require.Equal(t, "completed", result.State)
	require.Equal(t, []string{root.UID}, result.Finalized)
	receipt, err := source.LookupIssueTreeSourceReceipt(ctx, request)
	require.NoError(t, err)
	require.Equal(t, request.RequestUID, receipt.RequestUID)
	require.Equal(t, snapshot.Digest, receipt.SnapshotDigest)
	require.Equal(t, accepted.SnapshotDigest, receipt.SnapshotDigest)
	require.Equal(t, root.UID, receipt.Route.IssueUID)
	require.Equal(t, destination.InstanceUID(), receipt.Route.AuthorityUID)
	require.Equal(t, to.UID, receipt.Route.ProjectUID)
	changedRequest := request
	changedRequest.RequestUID, err = uid.New()
	require.NoError(t, err)
	_, err = source.LookupIssueTreeSourceReceipt(ctx, changedRequest)
	require.ErrorIs(t, err, db.ErrMoveConflict)

	q := source.(interface {
		QueryRowContext(context.Context, string, ...any) *sql.Row
	})
	var authority, project, short, eventUID string
	require.NoError(t, q.QueryRowContext(ctx, `SELECT moved_to_authority_uid,moved_to_project_uid,moved_to_short_id,moved_event_uid FROM issues WHERE uid=$1`, root.UID).Scan(&authority, &project, &short, &eventUID))
	require.Equal(t, destination.InstanceUID(), authority)
	require.Equal(t, to.UID, project)
	require.Equal(t, accepted.Applied[0].ShortID, short)
	require.NotEmpty(t, eventUID)
	retry, err := source.FinalizeIssueTree(verified, request, preview, accepted)
	require.NoError(t, err)
	require.Equal(t, result.Finalized, retry.Finalized)
}

func RunIssueTreeChangedSourceFinalization(t *testing.T, source, destination db.Storage) {
	ctx := t.Context()
	from, err := source.CreateProject(ctx, "source-project")
	require.NoError(t, err)
	to, err := destination.CreateProject(ctx, "target-project")
	require.NoError(t, err)
	root, _, err := source.CreateIssue(ctx, db.CreateIssueParams{ProjectID: from.ID, Title: "Root", Author: "author"})
	require.NoError(t, err)
	comment, _, err := source.CreateComment(ctx, db.CreateCommentParams{IssueID: root.ID, Author: "author", Body: "original"})
	require.NoError(t, err)
	requestUID, err := uid.New()
	require.NoError(t, err)
	request := db.MoveTreeRequest{SourceAuthorityUID: source.InstanceUID(), SourceProjectUID: from.UID, IssueUID: root.UID, DestinationAuthorityUID: destination.InstanceUID(), DestinationProjectUID: to.UID, RequestUID: requestUID, ActorAuthorityUID: source.InstanceUID(), Actor: "operator"}
	snapshot, err := source.ExportIssueTree(ctx, request)
	require.NoError(t, err)
	accepted, err := destination.AcceptIssueTree(ctx, snapshot, request)
	require.NoError(t, err)
	q := source.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
		QueryRowContext(context.Context, string, ...any) *sql.Row
	})
	_, err = q.ExecContext(ctx, `UPDATE comments SET body='source changed' WHERE uid=$1`, comment.UID)
	require.NoError(t, err)
	verified := db.WithMoveDestinationCommitVerifier(ctx, func(ctx context.Context, r db.MoveTreeRequest, _ db.MoveTreeResult) error {
		_, err := destination.LookupIssueTreeAcceptance(ctx, r)
		return err
	})
	result, err := source.FinalizeIssueTree(verified, request, db.MoveTreePreview{SnapshotDigest: snapshot.Digest, Members: snapshot.Portions[0].Members}, accepted)
	require.ErrorIs(t, err, db.ErrMoveConflict)
	require.Equal(t, "conflict", result.State)
	require.Empty(t, result.Finalized)
	var moved *string
	require.NoError(t, q.QueryRowContext(ctx, `SELECT moved_to_authority_uid FROM issues WHERE uid=$1`, root.UID).Scan(&moved))
	require.Nil(t, moved)
	comments, err := source.CommentsByIssue(ctx, root.ID)
	require.NoError(t, err)
	require.Equal(t, "source changed", comments[0].Body)
}
