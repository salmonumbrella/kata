package dbtest

import (
	"context"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
	"testing"
)

// Reverse movement is a new audited move. It replaces only a finalized source
// projection and preserves work subsequently accepted by the current authority.
func RunIssueTreeReverse(t *testing.T, first, second db.Storage) {
	ctx := t.Context()
	from, err := first.CreateProject(ctx, "first-project")
	require.NoError(t, err)
	to, err := second.CreateProject(ctx, "second-project")
	require.NoError(t, err)
	root, _, err := first.CreateIssue(ctx, db.CreateIssueParams{ProjectID: from.ID, Title: "Original", Author: "original-author"})
	require.NoError(t, err)
	child, _, err := first.CreateIssue(ctx, db.CreateIssueParams{ProjectID: from.ID, Title: "Completed child", Author: "original-author"})
	require.NoError(t, err)
	_, err = first.CreateLink(ctx, db.CreateLinkParams{FromIssueID: child.ID, ToIssueID: root.ID, Type: "parent", Author: "original-author"})
	require.NoError(t, err)
	_, _, _, err = first.CloseIssue(ctx, child.ID, "done", "original-author", "", nil)
	require.NoError(t, err)
	comment, _, err := first.CreateComment(ctx, db.CreateCommentParams{IssueID: root.ID, Author: "original-author", Body: "Original comment"})
	require.NoError(t, err)
	for _, label := range []string{"retained-label", "removed-label"} {
		_, err = first.AddLabel(ctx, root.ID, label, "original-author")
		require.NoError(t, err)
	}
	_, err = first.UpsertImportMapping(ctx, db.ImportMappingParams{Source: "example-source", ObjectType: "issue", ExternalID: "external-root", ProjectID: from.ID, IssueID: &root.ID})
	require.NoError(t, err)
	transfer := func(source, destination db.Storage, sourceProject, destinationProject db.Project) db.MoveTreeResult {
		requestUID, err := uid.New()
		require.NoError(t, err)
		request := db.MoveTreeRequest{SourceAuthorityUID: source.InstanceUID(), SourceProjectUID: sourceProject.UID, IssueUID: root.UID, DestinationAuthorityUID: destination.InstanceUID(), DestinationProjectUID: destinationProject.UID, RequestUID: requestUID, ActorAuthorityUID: source.InstanceUID(), Actor: "operator"}
		snapshot, err := source.ExportIssueTree(ctx, request)
		require.NoError(t, err)
		require.Equal(t, source.InstanceUID(), snapshot.Portions[0].Members[0].AuthorityUID, "current location is independent of historical owner namespace")
		accepted, err := destination.AcceptIssueTree(ctx, snapshot, request)
		require.NoError(t, err)
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
		require.Len(t, finalized.Applied, 2)
		return finalized
	}
	forward := transfer(first, second, from, to)
	moved, err := second.IssueByUID(ctx, root.UID, db.IncludeDeletedYes)
	require.NoError(t, err)
	edited, _, _, err := second.EditIssue(ctx, db.EditIssueParams{IssueID: moved.ID, Actor: "operator", Title: new("Updated at current authority")})
	require.NoError(t, err)
	_, _, err = second.CreateComment(ctx, db.CreateCommentParams{IssueID: edited.ID, Author: "another-author", Body: "New accepted work"})
	require.NoError(t, err)
	_, err = second.AddLabel(ctx, moved.ID, "new-label", "another-author")
	require.NoError(t, err)
	require.NoError(t, second.RemoveLabel(ctx, moved.ID, "removed-label"))
	reverse := transfer(second, first, to, from)
	require.NotEqual(t, forward.RequestUID, reverse.RequestUID)
	restored, err := first.IssueByUID(ctx, root.UID, db.IncludeDeletedYes)
	require.NoError(t, err)
	require.Equal(t, "Updated at current authority", restored.Title)
	require.Equal(t, "original-author", restored.Author)
	labels, err := first.LabelsByIssue(ctx, restored.ID)
	require.NoError(t, err)
	var labelNames []string
	for _, label := range labels {
		labelNames = append(labelNames, label.Label)
	}
	require.ElementsMatch(t, []string{"retained-label", "new-label"}, labelNames)
	mapping, err := first.ImportMappingBySource(ctx, from.ID, "example-source", "issue", "external-root")
	require.NoError(t, err)
	require.NotNil(t, mapping.IssueID)
	require.Equal(t, restored.ID, *mapping.IssueID)

	returnedChild, err := first.IssueByUID(ctx, child.UID, db.IncludeDeletedYes)
	require.NoError(t, err)
	require.Equal(t, "closed", returnedChild.Status)
	graph, err := first.LinksByIssue(ctx, restored.ID)
	require.NoError(t, err)
	require.Len(t, graph, 1)
	comments, err := first.CommentsByIssue(ctx, restored.ID)
	require.NoError(t, err)
	require.Len(t, comments, 2)
	require.Equal(t, comment.UID, comments[0].UID)
	require.Equal(t, "New accepted work", comments[1].Body)
}
