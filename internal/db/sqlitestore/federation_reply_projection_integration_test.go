package sqlitestore

import (
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"testing"
)

// Reconciliation repairs every comment field from retained events, including
// typed reply identity and edit precision when the stored body already matches.
func TestFederationReconciliationRepairsReplyProjection(t *testing.T) {
	for _, path := range []string{"full", "incremental"} {
		t.Run(path, func(t *testing.T) {
			d, p, issue := incrementalTestStore(t)
			ctx := t.Context()
			target, _, err := d.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "tester", Body: "Finding"})
			require.NoError(t, err)
			reply, _, err := d.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "tester", Body: "Original", ReplyToUID: target.UID, ReplyKind: "reply"})
			require.NoError(t, err)
			// Use a real edit event so the expected timestamp comes from product history.
			edited, _, _, err := d.EditComment(ctx, db.EditCommentParams{IssueID: issue.ID, CommentUID: reply.UID, Body: "Updated", Actor: "tester"})
			require.NoError(t, err)
			require.NotNil(t, edited.EditedAt)
			_, err = d.ExecContext(ctx, "UPDATE comments SET reply_to_uid=NULL, reply_kind=NULL, edited_at=NULL WHERE uid=?", reply.UID)
			require.NoError(t, err)
			if path == "full" {
				require.NoError(t, d.MaterializeFederatedProject(ctx, p.ID))
			} else {
				ev := incrementalRemoteEvent(t, p, issue.UID, "issue.updated", `{"issue_uid":"`+issue.UID+`","title":"Updated title"}`, 9_000_000_000_000)
				_, err = d.IngestFederationEvents(ctx, db.FederationIngestParams{ProjectID: p.ID, SpokeInstanceUID: ev.OriginInstanceUID, Events: []db.FederationIngestEvent{{SourceEventID: 1, Event: ev}}})
				require.NoError(t, err)
			}
			comments, err := d.CommentsByIssue(ctx, issue.ID)
			require.NoError(t, err)
			var current db.Comment
			for _, comment := range comments {
				if comment.UID == reply.UID {
					current = comment
					break
				}
			}
			require.Equal(t, reply.UID, current.UID)
			require.Equal(t, reply.ReplyToUID, current.ReplyToUID)
			require.Equal(t, reply.ReplyKind, current.ReplyKind)
			require.Equal(t, edited.EditedAt, current.EditedAt)
			require.Equal(t, edited.Body, current.Body)
		})
	}
}
