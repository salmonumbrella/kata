package dbtest

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// Writer failures after the first inserted event must return only committed
// evidence. Fault DDL exists solely in the disposable fixture stores.
func RunIssueTreeRolledBackEvidence(t *testing.T, source, destination db.Storage, sourcePG, destinationPG bool) {
	ctx := t.Context()
	from, err := source.CreateProject(ctx, "source-project")
	require.NoError(t, err)
	to, err := destination.CreateProject(ctx, "destination-project")
	require.NoError(t, err)
	root, _, err := source.CreateIssue(ctx, db.CreateIssueParams{ProjectID: from.ID, Title: "Root", Author: "operator"})
	require.NoError(t, err)
	child, _, err := source.CreateIssue(ctx, db.CreateIssueParams{ProjectID: from.ID, Title: "Child", Author: "operator"})
	require.NoError(t, err)
	_, err = source.CreateLink(ctx, db.CreateLinkParams{FromIssueID: child.ID, ToIssueID: root.ID, Type: "parent", Author: "operator"})
	require.NoError(t, err)
	requestUID, err := uid.New()
	require.NoError(t, err)
	r := db.MoveTreeRequest{SourceAuthorityUID: source.InstanceUID(), SourceProjectUID: from.UID, IssueUID: root.UID, DestinationAuthorityUID: destination.InstanceUID(), DestinationProjectUID: to.UID, RequestUID: requestUID, ActorAuthorityUID: source.InstanceUID(), Actor: "operator"}
	snapshot, err := source.ExportIssueTree(ctx, r)
	require.NoError(t, err)
	fail := func(store db.Storage, postgres bool, eventType, issueUID string) func() {
		q := store.(interface {
			ExecContext(context.Context, string, ...any) (sql.Result, error)
		})
		if postgres {
			_, err := q.ExecContext(ctx, fmt.Sprintf(`CREATE FUNCTION issue_move_test_abort() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.type='%s' AND NEW.issue_uid='%s' THEN RAISE EXCEPTION 'injected move writer failure'; END IF; RETURN NEW; END $$`, eventType, issueUID))
			require.NoError(t, err)
			_, err = q.ExecContext(ctx, `CREATE TRIGGER issue_move_test_fault BEFORE INSERT ON events FOR EACH ROW EXECUTE FUNCTION issue_move_test_abort()`)
			require.NoError(t, err)
		} else {
			_, err := q.ExecContext(ctx, fmt.Sprintf(`CREATE TRIGGER issue_move_test_fault BEFORE INSERT ON events WHEN NEW.type='%s' AND NEW.issue_uid='%s' BEGIN SELECT RAISE(ABORT,'injected move writer failure'); END`, eventType, issueUID))
			require.NoError(t, err)
		}
		return func() {
			statement := "DROP TRIGGER issue_move_test_fault"
			if postgres {
				statement += " ON events"
			}
			_, err := q.ExecContext(ctx, statement)
			require.NoError(t, err)
			if postgres {
				_, err = q.ExecContext(ctx, `DROP FUNCTION issue_move_test_abort()`)
				require.NoError(t, err)
			}
		}
	}
	secondUID := snapshot.Portions[0].Issues[1].UID
	restore := fail(destination, destinationPG, "issue.move_received", secondUID)
	rejected, err := destination.AcceptIssueTree(ctx, snapshot, r)
	require.Error(t, err)
	require.Empty(t, rejected.Applied)
	require.Empty(t, rejected.Events)
	restore()
	for _, selected := range []db.Issue{root, child} {
		_, err := destination.IssueByUID(ctx, selected.UID, db.IncludeDeletedYes)
		require.ErrorIs(t, err, db.ErrNotFound)
	}
	accepted, err := destination.AcceptIssueTree(ctx, snapshot, r)
	require.NoError(t, err)
	require.Len(t, accepted.Applied, 2)
	verified := db.WithMoveDestinationCommitVerifier(ctx, func(ctx context.Context, r db.MoveTreeRequest, proposed db.MoveTreeResult) error {
		actual, err := destination.LookupIssueTreeAcceptance(ctx, r)
		if err != nil {
			return err
		}
		return db.ValidateCommittedMoveEvidence(proposed, actual)
	})
	preview := db.MoveTreePreview{SnapshotDigest: snapshot.Digest}
	restore = fail(source, sourcePG, "issue.moved", snapshot.Portions[0].Members[1].IssueUID)
	rejected, err = source.FinalizeIssueTree(verified, r, preview, accepted)
	require.Error(t, err)
	require.Empty(t, rejected.Finalized)
	require.Empty(t, rejected.Events)
	require.Equal(t, accepted.Applied, rejected.Applied)
	restore()
	_, err = source.LookupIssueTreeSourceReceipt(ctx, r)
	require.ErrorIs(t, err, db.ErrNotFound)
	finished, err := source.FinalizeIssueTree(verified, r, preview, accepted)
	require.NoError(t, err)
	require.Equal(t, "completed", finished.State)
	require.Len(t, finished.Finalized, 2)
}
