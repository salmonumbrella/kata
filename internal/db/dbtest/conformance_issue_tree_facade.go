package dbtest

import (
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
	"testing"
)

func RunUIDMoveTreeFacade(t *testing.T, d db.Storage) {
	ctx := t.Context()
	source, err := d.CreateProject(ctx, "source-project")
	require.NoError(t, err)
	target, err := d.CreateProject(ctx, "target-project")
	require.NoError(t, err)
	root, _, err := d.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "Root", Author: "author"})
	require.NoError(t, err)
	child, _, err := d.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "Completed child", Author: "author"})
	require.NoError(t, err)
	_, err = d.CreateLink(ctx, db.CreateLinkParams{FromIssueID: child.ID, ToIssueID: root.ID, Type: "parent", Author: "author"})
	require.NoError(t, err)
	_, _, _, err = d.CloseIssue(ctx, child.ID, "done", "author", "", nil)
	require.NoError(t, err)
	requestUID, err := uid.New()
	require.NoError(t, err)
	request := db.MoveTreeRequest{SourceAuthorityUID: d.InstanceUID(), SourceProjectUID: source.UID, IssueUID: root.UID, DestinationAuthorityUID: d.InstanceUID(), DestinationProjectUID: target.UID, RequestUID: requestUID, ActorAuthorityUID: d.InstanceUID(), Actor: "operator"}
	preview, err := d.PreviewIssueTree(ctx, request)
	require.NoError(t, err)
	require.Len(t, preview.Members, 2)
	require.Len(t, preview.Relations, 1, "preview includes the selected tree parent relationship")
	require.NotEmpty(t, preview.Digest)
	request.PreviewDigest = preview.Digest
	result, err := d.MoveIssueTree(ctx, request)
	require.NoError(t, err)
	require.Equal(t, "completed", result.State)
	require.Len(t, result.Applied, 2)
	for _, applied := range result.Applied {
		require.NotEmpty(t, applied.CommittedEventUID)
		require.NotEmpty(t, applied.CommittedEventHash)
		events, err := d.EventsByUIDs(ctx, target.ID, []string{applied.CommittedEventUID})
		require.NoError(t, err)
		require.Len(t, events, 1)
		require.Equal(t, events[0].ContentHash, applied.CommittedEventHash)
	}
	retry, err := d.MoveIssueTree(ctx, request)
	require.NoError(t, err)
	require.Equal(t, result.Applied, retry.Applied)
	// Selecting a tree already at its destination commits no move evidence.
	noOp := request
	noOp.SourceProjectUID = target.UID
	noOp.PreviewDigest = ""
	noOp.RequestUID, err = uid.New()
	require.NoError(t, err)
	before, err := d.MaxEventID(ctx)
	require.NoError(t, err)
	unchanged, err := d.MoveIssueTree(ctx, noOp)
	require.NoError(t, err)
	require.Equal(t, "completed", unchanged.State)
	require.Empty(t, unchanged.Applied, "a no-op must not fabricate committed mappings")
	require.Empty(t, unchanged.Finalized)
	after, err := d.MaxEventID(ctx)
	require.NoError(t, err)
	require.Equal(t, before, after)
}
