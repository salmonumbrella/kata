package dbtest

import (
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
	"testing"
)

// Acceptance commits the selected tree once, preserving original history and
// comment identity despite unrelated numeric IDs in the destination database.
func RunIssueTreeAcceptance(t *testing.T, source, destination db.Storage) {
	ctx := t.Context()
	from, err := source.CreateProject(ctx, "source-project")
	require.NoError(t, err)
	to, err := destination.CreateProject(ctx, "target-project")
	require.NoError(t, err)
	_, _, err = destination.CreateIssue(ctx, db.CreateIssueParams{ProjectID: to.ID, Title: "Existing", Author: "other-author"})
	require.NoError(t, err)
	root, created, err := source.CreateIssue(ctx, db.CreateIssueParams{ProjectID: from.ID, Title: "Root", Body: "original content", Author: "original-author"})
	require.NoError(t, err)
	child, _, err := source.CreateIssue(ctx, db.CreateIssueParams{ProjectID: from.ID, Title: "Completed child", Author: "original-author"})
	require.NoError(t, err)
	_, err = source.CreateLink(ctx, db.CreateLinkParams{FromIssueID: child.ID, ToIssueID: root.ID, Type: "parent", Author: "original-author"})
	require.NoError(t, err)
	_, _, _, err = source.CloseIssue(ctx, child.ID, "done", "original-author", "", nil)
	require.NoError(t, err)
	comment, _, err := source.CreateComment(ctx, db.CreateCommentParams{IssueID: root.ID, Author: "comment-author", Teammate: "researcher", Body: "preserved comment"})
	require.NoError(t, err)
	requestUID, err := uid.New()
	require.NoError(t, err)
	request := db.MoveTreeRequest{SourceAuthorityUID: source.InstanceUID(), SourceProjectUID: from.UID, IssueUID: root.UID, DestinationAuthorityUID: destination.InstanceUID(), DestinationProjectUID: to.UID, RequestUID: requestUID, ActorAuthorityUID: source.InstanceUID(), Actor: "operator"}
	snapshot, err := source.ExportIssueTree(ctx, request)
	require.NoError(t, err)
	accepted, err := destination.AcceptIssueTree(ctx, snapshot, request)
	require.NoError(t, err)
	require.Equal(t, "partial", accepted.State, "destination commit alone is not a finalized move")
	require.Len(t, accepted.Applied, 2)
	require.Empty(t, accepted.Finalized)
	moved, err := destination.IssueByUID(ctx, root.UID, db.IncludeDeletedYes)
	require.NoError(t, err)
	require.Equal(t, "original-author", moved.Author)
	require.Equal(t, to.ID, moved.ProjectID)
	history, err := destination.EventsByUIDs(ctx, to.ID, []string{created.UID})
	require.NoError(t, err)
	require.Len(t, history, 1)
	require.Equal(t, created.ContentHash, history[0].ContentHash)
	comments, err := destination.CommentsByIssue(ctx, moved.ID)
	require.NoError(t, err)
	require.Len(t, comments, 1)
	require.Equal(t, comment.UID, comments[0].UID)
	require.Equal(t, comment.Author, comments[0].Author)
	retained, err := source.IssueByUID(ctx, root.UID, db.IncludeDeletedYes)
	require.NoError(t, err)
	require.Equal(t, from.ID, retained.ProjectID)
	retry, err := destination.AcceptIssueTree(ctx, snapshot, request)
	require.NoError(t, err)
	require.Equal(t, accepted.Applied, retry.Applied)
	_, _, err = destination.CreateComment(ctx, db.CreateCommentParams{IssueID: moved.ID, Author: "operator", Body: "destination changed after acceptance"})
	require.NoError(t, err)
	conflict, err := destination.AcceptIssueTree(ctx, snapshot, request)
	require.ErrorIs(t, err, db.ErrMoveConflict)
	require.Equal(t, "conflict", conflict.State)
}
