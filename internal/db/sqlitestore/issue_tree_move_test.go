package sqlitestore_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
)

// The default selection includes every descendant, irrespective of lifecycle.
// A blocks edge does not make its other endpoint part of the child tree.
func TestMoveIssueTreeIncludesCompletedAndDeletedChildren(t *testing.T) {
	d := openTestDB(t)
	ctx := t.Context()
	source, err := d.CreateProject(ctx, "source-project")
	require.NoError(t, err)
	target, err := d.CreateProject(ctx, "destination-project")
	require.NoError(t, err)
	root := seedIssueInProject(t, d, source.ID, "Root", "tester")
	child := seedIssueInProject(t, d, source.ID, "Child", "tester")
	completed := seedIssueInProject(t, d, source.ID, "Completed grandchild", "tester")
	deleted := seedIssueInProject(t, d, source.ID, "Deleted child", "tester")
	unrelated := seedIssueInProject(t, d, source.ID, "Unrelated blocker", "tester")
	seedLink(t, d, child.ID, root.ID, "parent", "tester")
	seedLink(t, d, completed.ID, child.ID, "parent", "tester")
	seedLink(t, d, deleted.ID, root.ID, "parent", "tester")
	seedLink(t, d, unrelated.ID, root.ID, "blocks", "tester")
	_, err = d.ExecContext(ctx, `UPDATE issues SET status='closed', closed_reason='done', closed_at='2026-10-01T00:00:00Z' WHERE id=?`, completed.ID)
	require.NoError(t, err)
	_, err = d.ExecContext(ctx, `UPDATE issues SET deleted_at='2026-10-01T00:00:00Z' WHERE id=?`, deleted.ID)
	require.NoError(t, err)

	result, err := d.MoveIssueTreeLocal(ctx, db.MoveIssueTreeIn{MoveIssueProjectIn: db.MoveIssueProjectIn{
		IssueID: root.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: root.Revision, Actor: "tester",
	}})
	require.NoError(t, err)
	require.Len(t, result.Issues, 4)
	require.Len(t, result.Events, 4)
	for _, original := range []db.Issue{root, child, completed, deleted} {
		var projectID int64
		var uid, status string
		require.NoError(t, d.QueryRowContext(ctx, `SELECT project_id, uid, status FROM issues WHERE id=?`, original.ID).Scan(&projectID, &uid, &status))
		assert.Equal(t, target.ID, projectID)
		assert.Equal(t, original.UID, uid)
		if original.ID == completed.ID {
			assert.Equal(t, "closed", status)
		}
	}
	var deletedAt string
	require.NoError(t, d.QueryRowContext(ctx, `SELECT deleted_at FROM issues WHERE id=?`, deleted.ID).Scan(&deletedAt))
	assert.Equal(t, "2026-10-01T00:00:00Z", deletedAt)
	retained, err := d.IssueByID(ctx, unrelated.ID)
	require.NoError(t, err)
	assert.Equal(t, source.ID, retained.ProjectID)
	var movedEvents int
	require.NoError(t, d.QueryRowContext(ctx, `SELECT count(*) FROM events WHERE type='issue.moved'`).Scan(&movedEvents))
	assert.Equal(t, 4, movedEvents)
}

func TestMoveIssueTreeChildMappingCollisionRollsBackWholeTree(t *testing.T) {
	d := openTestDB(t)
	ctx := t.Context()
	source, err := d.CreateProject(ctx, "source-project")
	require.NoError(t, err)
	target, err := d.CreateProject(ctx, "destination-project")
	require.NoError(t, err)
	root := seedIssueInProject(t, d, source.ID, "Root", "tester")
	child := seedIssueInProject(t, d, source.ID, "Child", "tester")
	existing := seedIssueInProject(t, d, target.ID, "Existing", "tester")
	seedLink(t, d, child.ID, root.ID, "parent", "tester")
	for _, mapping := range []db.ImportMappingParams{
		{ProjectID: source.ID, Source: "example-provider", ExternalID: "same-external-id", ObjectType: "issue", IssueID: &child.ID},
		{ProjectID: target.ID, Source: "example-provider", ExternalID: "same-external-id", ObjectType: "issue", IssueID: &existing.ID},
	} {
		_, err := d.UpsertImportMapping(ctx, mapping)
		require.NoError(t, err)
	}
	_, err = d.MoveIssueTreeLocal(ctx, db.MoveIssueTreeIn{MoveIssueProjectIn: db.MoveIssueProjectIn{
		IssueID: root.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: root.Revision, Actor: "tester",
	}})
	require.ErrorIs(t, err, db.ErrProjectMergeImportMappingCollision)
	for _, original := range []db.Issue{root, child} {
		retained, err := d.IssueByID(ctx, original.ID)
		require.NoError(t, err)
		assert.Equal(t, source.ID, retained.ProjectID)
		assert.Equal(t, original.Revision, retained.Revision)
	}
	var movedEvents int
	require.NoError(t, d.QueryRowContext(ctx, `SELECT count(*) FROM events WHERE type='issue.moved'`).Scan(&movedEvents))
	assert.Zero(t, movedEvents)
}

// Discovering a child in another project cannot widen the admitted projects.
func TestMoveIssueTreeRejectsHiddenChildWithoutPartialResult(t *testing.T) {
	d := openTestDB(t)
	ctx := t.Context()
	source, err := d.CreateProject(ctx, "source-project")
	require.NoError(t, err)
	target, err := d.CreateProject(ctx, "destination-project")
	require.NoError(t, err)
	hidden, err := d.CreateProject(ctx, "private-project")
	require.NoError(t, err)
	root := seedIssueInProject(t, d, source.ID, "Root", "tester")
	child := seedIssueInProject(t, d, hidden.ID, "Hidden child", "tester")
	seedLink(t, d, child.ID, root.ID, "parent", "tester")
	limited := db.WithAuthorizedProjects(ctx, []string{source.UID, target.UID})
	result, err := d.MoveIssueTreeLocal(limited, db.MoveIssueTreeIn{MoveIssueProjectIn: db.MoveIssueProjectIn{
		IssueID: root.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: root.Revision, Actor: "tester",
	}})
	require.ErrorIs(t, err, db.ErrNotFound)
	assert.Empty(t, result.Issues)
	assert.Empty(t, result.Events)
	for _, original := range []db.Issue{root, child} {
		retained, err := d.IssueByID(ctx, original.ID)
		require.NoError(t, err)
		assert.Equal(t, original.ProjectID, retained.ProjectID)
	}
}

// A descendant already at the destination keeps its revision and reference.
func TestMoveIssueTreeRetainsChildAlreadyAtDestination(t *testing.T) {
	d := openTestDB(t)
	ctx := t.Context()
	source, err := d.CreateProject(ctx, "source-project")
	require.NoError(t, err)
	target, err := d.CreateProject(ctx, "destination-project")
	require.NoError(t, err)
	root := seedIssueInProject(t, d, source.ID, "Root", "tester")
	child := seedIssueInProject(t, d, target.ID, "Child already there", "tester")
	seedLink(t, d, child.ID, root.ID, "parent", "tester")
	result, err := d.MoveIssueTreeLocal(ctx, db.MoveIssueTreeIn{MoveIssueProjectIn: db.MoveIssueProjectIn{
		IssueID: root.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: root.Revision, Actor: "tester",
	}})
	require.NoError(t, err)
	retained, err := d.IssueByID(ctx, child.ID)
	require.NoError(t, err)
	assert.Equal(t, child.Revision, retained.Revision)
	assert.Equal(t, child.ShortID, retained.ShortID)
	assert.Len(t, result.Issues, 2)
	assert.Len(t, result.Events, 1)
}

func TestMoveIssueTreePreviewAndIssueOnly(t *testing.T) {
	d := openTestDB(t)
	ctx := t.Context()
	source, err := d.CreateProject(ctx, "source-project")
	require.NoError(t, err)
	target, err := d.CreateProject(ctx, "destination-project")
	require.NoError(t, err)
	root := seedIssueInProject(t, d, source.ID, "Root", "tester")
	child := seedIssueInProject(t, d, source.ID, "Child", "tester")
	seedLink(t, d, child.ID, root.ID, "parent", "tester")
	input := db.MoveIssueTreeIn{MoveIssueProjectIn: db.MoveIssueProjectIn{
		IssueID: root.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: root.Revision, Actor: "tester", DryRun: true,
	}}
	preview, err := d.MoveIssueTreeLocal(ctx, input)
	require.NoError(t, err)
	assert.Len(t, preview.Issues, 2)
	assert.Empty(t, preview.Events)
	for _, original := range []db.Issue{root, child} {
		retained, err := d.IssueByID(ctx, original.ID)
		require.NoError(t, err)
		assert.Equal(t, original.ProjectID, retained.ProjectID)
		assert.Equal(t, original.Revision, retained.Revision)
	}
	input.DryRun, input.IssueOnly = false, true
	result, err := d.MoveIssueTreeLocal(ctx, input)
	require.NoError(t, err)
	assert.Len(t, result.Issues, 1)
	assert.Len(t, result.Events, 1)
	assert.Equal(t, target.ID, result.Root.Issue.ProjectID)
	retained, err := d.IssueByID(ctx, child.ID)
	require.NoError(t, err)
	assert.Equal(t, source.ID, retained.ProjectID)
}

func TestIssueMoveRoutingSchema(t *testing.T) {
	dbtest.RunIssueMoveRoutingSchema(t, openTestDB(t))
}

func TestIssueMoveOwnerSchema(t *testing.T) {
	dbtest.RunIssueMoveOwnerSchema(t, openTestDB(t))
}

func TestIssueMoveAuditPurge(t *testing.T) {
	dbtest.RunIssueMoveAuditPurge(t, openTestDB(t))
}

func TestIssueMoveSourceProjectPurge(t *testing.T) {
	dbtest.RunIssueMoveSourceProjectPurge(t, openTestDB(t))
}

func TestIssueMoveSourceBytesPurge(t *testing.T) {
	dbtest.RunIssueMoveSourceBytesPurge(t, openTestDB(t))
}

func TestIssueMoveProofRetirement(t *testing.T) {
	dbtest.RunIssueMoveProofRetirement(t, openTestDB(t))
}

func TestOrdinaryMappedProjectPurge(t *testing.T) {
	dbtest.RunOrdinaryMappedProjectPurge(t, openTestDB(t))
}

func TestMappedLinkDeletion(t *testing.T) {
	dbtest.RunMappedLinkDeletion(t, openTestDB(t))
}

func TestMappedImportLinkRemoval(t *testing.T) {
	dbtest.RunMappedImportLinkRemoval(t, openTestDB(t))
}

func TestMappedFederationBoundaryCleanup(t *testing.T) {
	dbtest.RunMappedFederationBoundaryCleanup(t, openTestDB(t))
}

func TestMappedFederationReset(t *testing.T) {
	dbtest.RunMappedFederationReset(t, openTestDB(t))
}

func TestMappedProjectionReconcile(t *testing.T) {
	dbtest.RunMappedProjectionReconcile(t, openTestDB(t))
}

func TestIssueMovePurgedRootRetirement(t *testing.T) {
	dbtest.RunIssueMovePurgedRootRetirement(t, openTestDB(t))
}

func TestIssueMoveRetainedNameConflict(t *testing.T) {
	dbtest.RunIssueMoveRetainedNameConflict(t, openTestDB(t))
}

func TestIssueMoveRetainedHistoryRead(t *testing.T) {
	dbtest.RunIssueMoveRetainedHistoryRead(t, openTestDB(t))
}

func TestIssueTreeStalePreview(t *testing.T) { dbtest.RunIssueTreeStalePreview(t, openTestDB(t)) }

func TestMoveOccurrencePreservesSeries(t *testing.T) {
	dbtest.RunMoveOccurrencePreservesSeries(t, openTestDB(t))
}

func TestMoveSeriesIncludesRetainedOccurrences(t *testing.T) {
	dbtest.RunMoveSeriesIncludesRetainedOccurrences(t, openTestDB(t))
}

func TestMoveSeriesEventScope(t *testing.T) { dbtest.RunMoveSeriesEventScope(t, openTestDB(t)) }

func TestMovedSeriesHistory(t *testing.T) { dbtest.RunMovedSeriesHistory(t, openTestDB(t)) }

func TestMovedIssueAdmission(t *testing.T) { dbtest.RunMovedIssueAdmission(t, openTestDB(t)) }

func TestMovedIssueMutations(t *testing.T) { dbtest.RunMovedIssueMutations(t, openTestDB(t)) }

func TestMovedImportSuppression(t *testing.T) { dbtest.RunMovedImportSuppression(t, openTestDB(t)) }

func TestMovedRecurrenceAdmission(t *testing.T) { dbtest.RunMovedRecurrenceAdmission(t, openTestDB(t)) }

func TestMovedStatusAdmission(t *testing.T) { dbtest.RunMovedStatusAdmission(t, openTestDB(t)) }

func TestMoveTreeProviderSourcePolling(t *testing.T) {
	dbtest.RunMoveTreeProviderSourcePolling(t, openTestDB(t))
}
