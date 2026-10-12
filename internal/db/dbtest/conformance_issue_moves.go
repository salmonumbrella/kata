package dbtest

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// RunIssueTreeMoves exercises the native atomic-tree contract on real stores.
func RunIssueTreeMoves(t *testing.T, store db.Storage) {
	t.Helper()
	mover, supported := store.(interface {
		MoveIssueTreeLocal(context.Context, db.MoveIssueTreeIn) (db.MoveIssueTreeOut, error)
	})
	require.True(t, supported, "backend must implement native whole-tree moves")
	ctx := t.Context()
	source, err := store.CreateProject(ctx, "source-project")
	require.NoError(t, err)
	target, err := store.CreateProject(ctx, "destination-project")
	require.NoError(t, err)
	var members []db.Issue
	for _, title := range []string{"Root", "Completed child", "Deleted grandchild"} {
		issue, err := createFixtureIssue(ctx, store, source.ID, title, "tester", nil)
		require.NoError(t, err)
		members = append(members, issue)
	}
	for i := 1; i < len(members); i++ {
		_, err := store.CreateLink(ctx, db.CreateLinkParams{
			FromIssueID: members[i].ID, ToIssueID: members[i-1].ID, Type: "parent", Author: "tester",
		})
		require.NoError(t, err)
	}
	_, _, changed, err := store.CloseIssueWithEvents(ctx, members[2].ID, "done", "tester",
		"Completed leaf retains its identity when its parent tree moves between projects.", nil)
	require.NoError(t, err)
	require.True(t, changed)
	_, _, changed, err = store.CloseIssueWithEvents(ctx, members[1].ID, "done", "tester",
		"Completed child is retained by default when its entire parent tree moves.", nil)
	require.NoError(t, err)
	require.True(t, changed)
	_, _, changed, err = store.SoftDeleteIssue(ctx, members[2].ID, "tester")
	require.NoError(t, err)
	require.True(t, changed)
	// Queued status intent must survive relocation with its exact event identity.
	mapping, err := store.UpsertImportMapping(ctx, db.ImportMappingParams{
		ProjectID: source.ID, Source: "example-provider", ExternalID: "task-1",
		ObjectType: "issue", IssueID: &members[1].ID,
	})
	require.NoError(t, err)
	sqlStore := store.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
		QueryRowContext(context.Context, string, ...any) *sql.Row
	})
	var pendingUID string
	require.NoError(t, sqlStore.QueryRowContext(ctx, `SELECT uid FROM events WHERE issue_id=$1 AND type='issue.closed' ORDER BY id DESC LIMIT 1`, members[1].ID).Scan(&pendingUID))
	const observedAt = "2026-10-01T12:00:00Z"
	_, err = sqlStore.ExecContext(ctx, `UPDATE import_mappings SET observed_status=$1,observed_status_at=$2,pending_event_uid=$3,remote_locator=$4 WHERE id=$5`, "open", observedAt, pendingUID, "task-1", mapping.ID)
	require.NoError(t, err)
	result, err := mover.MoveIssueTreeLocal(ctx, db.MoveIssueTreeIn{MoveIssueProjectIn: db.MoveIssueProjectIn{
		IssueID: members[0].ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: members[0].Revision, Actor: "tester",
	}})
	require.NoError(t, err)
	require.Len(t, result.Issues, len(members))
	require.Len(t, result.Events, len(members))
	var mappingProjectID int64
	var observed, observedStamp, pending, locator *string
	require.NoError(t, sqlStore.QueryRowContext(ctx, `SELECT project_id,observed_status,CAST(observed_status_at AS TEXT),pending_event_uid,remote_locator FROM import_mappings WHERE id=$1`, mapping.ID).Scan(&mappingProjectID, &observed, &observedStamp, &pending, &locator))
	assert.Equal(t, target.ID, mappingProjectID)
	assert.Equal(t, new("open"), observed)
	assert.Equal(t, new(observedAt), observedStamp)
	assert.Equal(t, &pendingUID, pending, "moving a tree must not discard a queued external status change")
	assert.Equal(t, new("task-1"), locator)
	committedAccessRevision, err := store.ProjectAccessRevision(ctx)
	require.NoError(t, err)
	assert.Equal(t, committedAccessRevision, result.Root.ProjectAccessRevision,
		"response admission must retain the revision committed by the whole transaction")
	for _, original := range members {
		moved, err := store.IssueByID(ctx, original.ID)
		require.NoError(t, err)
		assert.Equal(t, original.UID, moved.UID)
		assert.Equal(t, target.ID, moved.ProjectID)
		if original.ID == members[1].ID {
			assert.Equal(t, "closed", moved.Status)
		}
		if original.ID == members[2].ID {
			assert.NotNil(t, moved.DeletedAt)
		}
	}
	for _, event := range result.Events {
		assert.Equal(t, "issue.moved", event.Type)
		assert.Equal(t, "tester", event.Actor)
		assert.Equal(t, target.ID, event.ProjectID)
		assert.NotEmpty(t, event.UID)
		assert.NotEmpty(t, event.ContentHash)
	}

	scopeSource, err := store.CreateProject(ctx, "scope-source-project")
	require.NoError(t, err)
	scopeTarget, err := store.CreateProject(ctx, "scope-destination-project")
	require.NoError(t, err)
	hiddenProject, err := store.CreateProject(ctx, "scope-hidden-project")
	require.NoError(t, err)
	scopeRoot, err := createFixtureIssue(ctx, store, scopeSource.ID, "Visible root", "tester", nil)
	require.NoError(t, err)
	hiddenChild, err := createFixtureIssue(ctx, store, hiddenProject.ID, "Hidden child", "tester", nil)
	require.NoError(t, err)
	_, err = store.CreateLink(ctx, db.CreateLinkParams{FromIssueID: hiddenChild.ID, ToIssueID: scopeRoot.ID, Type: "parent", Author: "tester"})
	require.NoError(t, err)
	limited := db.WithAuthorizedProjects(ctx, []string{scopeSource.UID, scopeTarget.UID})
	denied, err := mover.MoveIssueTreeLocal(limited, db.MoveIssueTreeIn{MoveIssueProjectIn: db.MoveIssueProjectIn{
		IssueID: scopeRoot.ID, FromProjectID: scopeSource.ID, ToProjectID: scopeTarget.ID, IfMatchRev: scopeRoot.Revision, Actor: "tester",
	}})
	require.ErrorIs(t, err, db.ErrNotFound)
	assert.Empty(t, denied.Issues)
	assert.Empty(t, denied.Events)
	for _, original := range []db.Issue{scopeRoot, hiddenChild} {
		retained, err := store.IssueByID(ctx, original.ID)
		require.NoError(t, err)
		assert.Equal(t, original.ProjectID, retained.ProjectID)
		assert.Equal(t, original.Revision, retained.Revision)
	}
}

// A preview cannot silently gain newly created descendants when execution starts.
func RunIssueTreeStalePreview(t *testing.T, store db.Storage) {
	t.Helper()
	mover := store.(interface {
		MoveIssueTreeLocal(context.Context, db.MoveIssueTreeIn) (db.MoveIssueTreeOut, error)
	})
	ctx := t.Context()
	source, err := store.CreateProject(ctx, "preview-source")
	require.NoError(t, err)
	target, err := store.CreateProject(ctx, "preview-destination")
	require.NoError(t, err)
	root, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "Root", Author: "tester"})
	require.NoError(t, err)
	input := db.MoveIssueTreeIn{MoveIssueProjectIn: db.MoveIssueProjectIn{IssueID: root.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: root.Revision, Actor: "tester", DryRun: true}}
	preview, err := mover.MoveIssueTreeLocal(ctx, input)
	require.NoError(t, err)
	require.NotEmpty(t, preview.PreviewDigest, "preview needs a repeatable selection guard")
	unchanged, err := mover.MoveIssueTreeLocal(ctx, input)
	require.NoError(t, err)
	require.Equal(t, preview.PreviewDigest, unchanged.PreviewDigest, "unchanged preview is deterministic")
	child, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "New child", Author: "tester"})
	require.NoError(t, err)
	_, err = store.CreateLink(ctx, db.CreateLinkParams{FromIssueID: child.ID, ToIssueID: root.ID, Type: "parent", Author: "tester"})
	require.NoError(t, err)
	input.DryRun = false
	input.PreviewDigest = preview.PreviewDigest
	result, err := mover.MoveIssueTreeLocal(ctx, input)
	require.Error(t, err, "stale preview cannot move an unseen child")
	require.Empty(t, result.Issues)
	require.Empty(t, result.Events)
	for _, member := range []db.Issue{root, child} {
		current, readErr := store.IssueByID(ctx, member.ID)
		require.NoError(t, readErr)
		require.Equal(t, source.ID, current.ProjectID)
	}
	input.PreviewDigest = ""
	fresh, err := mover.MoveIssueTreeLocal(ctx, input)
	require.NoError(t, err)
	require.Len(t, fresh.Issues, 2)
}

// Moving one occurrence retains the existing series and its future project.
func RunMoveOccurrencePreservesSeries(t *testing.T, store db.Storage) {
	t.Helper()
	ctx := t.Context()
	mover := store.(interface {
		MoveIssueTreeLocal(context.Context, db.MoveIssueTreeIn) (db.MoveIssueTreeOut, error)
	})
	source, err := store.CreateProject(ctx, "recurrence-source")
	require.NoError(t, err)
	destination, err := store.CreateProject(ctx, "recurrence-destination")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "Daily task", Author: "tester"})
	require.NoError(t, err)
	attached, err := store.CreateRecurrenceForIssue(ctx, db.CreateRecurrenceForIssueIn{IssueID: issue.ID, Recurrence: db.CreateRecurrenceIn{ProjectID: source.ID, Actor: "tester", Rule: "FREQ=DAILY", DTStart: "2026-10-12", Timezone: "UTC", Template: db.RecurrenceTemplate{Title: "Daily task"}}})
	require.NoError(t, err)
	current, err := store.IssueByID(ctx, issue.ID)
	require.NoError(t, err)
	result, err := mover.MoveIssueTreeLocal(ctx, db.MoveIssueTreeIn{MoveIssueProjectIn: db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: destination.ID, IfMatchRev: current.Revision, Actor: "tester"}})
	require.NoError(t, err)
	require.Equal(t, destination.ID, result.Root.Issue.ProjectID)
	require.Equal(t, &attached.Recurrence.ID, result.Root.Issue.RecurrenceID)
	series, err := store.GetRecurrenceByID(ctx, attached.Recurrence.ID)
	require.NoError(t, err)
	require.Equal(t, source.ID, series.ProjectID)
	q := store.(interface {
		QueryRowContext(context.Context, string, ...any) *sql.Row
	})
	var seriesUID, seriesAuthority, seriesProject string
	require.NoError(t, q.QueryRowContext(ctx, `SELECT recurrence_uid,recurrence_authority_uid,recurrence_project_uid FROM issues WHERE id=$1`, issue.ID).Scan(&seriesUID, &seriesAuthority, &seriesProject))
	require.Equal(t, series.UID, seriesUID)
	require.Equal(t, store.InstanceUID(), seriesAuthority)
	require.Equal(t, source.UID, seriesProject)
	next, err := store.MaterializeNext(ctx, series.ID, "2026-10-12", "scheduler")
	require.NoError(t, err)
	require.NotEmpty(t, next.NewIssueUID)
	future, err := store.IssueByUID(ctx, next.NewIssueUID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, source.ID, future.ProjectID, "the original series owns future materialization")
}

// Explicit series movement includes all retained occurrences and their trees;
// completed/deleted states persist and future work follows the moved series.
func RunMoveSeriesIncludesRetainedOccurrences(t *testing.T, store db.Storage) {
	t.Helper()
	ctx := t.Context()
	mover := store.(interface {
		MoveIssueTreeLocal(context.Context, db.MoveIssueTreeIn) (db.MoveIssueTreeOut, error)
	})
	source, err := store.CreateProject(ctx, "series-source")
	require.NoError(t, err)
	destination, err := store.CreateProject(ctx, "series-destination")
	require.NoError(t, err)
	root, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "First occurrence", Author: "tester"})
	require.NoError(t, err)
	attached, err := store.CreateRecurrenceForIssue(ctx, db.CreateRecurrenceForIssueIn{IssueID: root.ID, Recurrence: db.CreateRecurrenceIn{ProjectID: source.ID, Actor: "tester", Rule: "FREQ=DAILY", DTStart: "2026-10-12", Timezone: "UTC", Template: db.RecurrenceTemplate{Title: "Daily task"}}})
	require.NoError(t, err)
	second, err := store.MaterializeNext(ctx, attached.Recurrence.ID, "2026-10-12", "scheduler")
	require.NoError(t, err)
	third, err := store.MaterializeNext(ctx, attached.Recurrence.ID, "2026-10-13", "scheduler")
	require.NoError(t, err)
	completed, err := store.IssueByUID(ctx, second.NewIssueUID, db.IncludeDeletedNo)
	require.NoError(t, err)
	deleted, err := store.IssueByUID(ctx, third.NewIssueUID, db.IncludeDeletedNo)
	require.NoError(t, err)
	child, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "Occurrence child", Author: "tester"})
	require.NoError(t, err)
	_, err = store.CreateLink(ctx, db.CreateLinkParams{FromIssueID: child.ID, ToIssueID: completed.ID, Type: "parent", Author: "tester"})
	require.NoError(t, err)
	unrelated, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "Unrelated dependency", Author: "tester"})
	require.NoError(t, err)
	_, err = store.CreateLink(ctx, db.CreateLinkParams{FromIssueID: unrelated.ID, ToIssueID: root.ID, Type: "blocks", Author: "tester"})
	require.NoError(t, err)
	q := store.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
		QueryRowContext(context.Context, string, ...any) *sql.Row
	})
	_, err = q.ExecContext(ctx, `UPDATE issues SET status='closed',closed_reason='done',closed_at='2026-10-01T00:00:00.000Z' WHERE id=$1`, completed.ID)
	require.NoError(t, err)
	_, err = q.ExecContext(ctx, `UPDATE issues SET deleted_at='2026-10-01T00:00:00.000Z' WHERE id=$1`, deleted.ID)
	require.NoError(t, err)
	current, err := store.IssueByID(ctx, root.ID)
	require.NoError(t, err)
	result, err := mover.MoveIssueTreeLocal(ctx, db.MoveIssueTreeIn{MoveSeries: true, MoveIssueProjectIn: db.MoveIssueProjectIn{IssueID: root.ID, FromProjectID: source.ID, ToProjectID: destination.ID, IfMatchRev: current.Revision, Actor: "tester"}})
	require.NoError(t, err)
	require.Len(t, result.Issues, 4)
	for _, original := range []db.Issue{root, completed, deleted, child} {
		var projectID int64
		require.NoError(t, q.QueryRowContext(ctx, `SELECT project_id FROM issues WHERE uid=$1`, original.UID).Scan(&projectID))
		require.Equal(t, destination.ID, projectID)
	}
	var status string
	var deletedAt *string
	require.NoError(t, q.QueryRowContext(ctx, `SELECT status FROM issues WHERE id=$1`, completed.ID).Scan(&status))
	require.Equal(t, "closed", status)
	require.NoError(t, q.QueryRowContext(ctx, `SELECT deleted_at FROM issues WHERE id=$1`, deleted.ID).Scan(&deletedAt))
	require.NotNil(t, deletedAt)
	series, err := store.GetRecurrenceByID(ctx, attached.Recurrence.ID)
	require.NoError(t, err)
	require.Equal(t, destination.ID, series.ProjectID)
	var generation int64
	require.NoError(t, q.QueryRowContext(ctx, `SELECT location_generation FROM recurrences WHERE id=$1`, series.ID).Scan(&generation))
	require.Equal(t, int64(2), generation)
	next, err := store.MaterializeNext(ctx, series.ID, "2026-10-14", "scheduler")
	require.NoError(t, err)
	future, err := store.IssueByUID(ctx, next.NewIssueUID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, destination.ID, future.ProjectID)
	retained, err := store.IssueByID(ctx, unrelated.ID)
	require.NoError(t, err)
	require.Equal(t, source.ID, retained.ProjectID)
}

// Both audiences observe a series move; the departure audience gets only a
// reset when the new owner is outside its grant.
func RunMoveSeriesEventScope(t *testing.T, store db.Storage) {
	t.Helper()
	ctx := t.Context()
	mover := store.(interface {
		MoveIssueTreeLocal(context.Context, db.MoveIssueTreeIn) (db.MoveIssueTreeOut, error)
	})
	source, err := store.CreateProject(ctx, "series-event-source")
	require.NoError(t, err)
	destination, err := store.CreateProject(ctx, "series-event-destination")
	require.NoError(t, err)
	root, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "Recurring task", Author: "tester"})
	require.NoError(t, err)
	_, err = store.CreateRecurrenceForIssue(ctx, db.CreateRecurrenceForIssueIn{IssueID: root.ID, Recurrence: db.CreateRecurrenceIn{ProjectID: source.ID, Actor: "tester", Rule: "FREQ=DAILY", DTStart: "2026-10-12", Timezone: "UTC", Template: db.RecurrenceTemplate{Title: "Daily task"}}})
	require.NoError(t, err)
	current, err := store.IssueByID(ctx, root.ID)
	require.NoError(t, err)
	moved, err := mover.MoveIssueTreeLocal(ctx, db.MoveIssueTreeIn{MoveSeries: true, MoveIssueProjectIn: db.MoveIssueProjectIn{IssueID: root.ID, FromProjectID: source.ID, ToProjectID: destination.ID, IfMatchRev: current.Revision, Actor: "tester"}})
	require.NoError(t, err)
	var seriesEvent db.Event
	for _, event := range moved.Events {
		if event.Type == "recurrence.moved" {
			seriesEvent = event
		}
	}
	require.NotZero(t, seriesEvent.ID)
	for _, audience := range []struct {
		name    string
		project db.Project
		typ     string
	}{
		{"departure", source, db.ProjectScopeResetEventType}, {"arrival", destination, db.ProjectScopeResetEventType},
	} {
		t.Run(audience.name, func(t *testing.T) {
			events, err := store.EventsAfter(db.WithAuthorizedProjects(ctx, []string{audience.project.UID}), db.EventsAfterParams{ProjectID: audience.project.ID, Limit: 100})
			require.NoError(t, err)
			found := false
			for _, event := range events {
				if event.ID == seriesEvent.ID {
					found = true
					require.Equal(t, audience.typ, event.Type)
					require.Equal(t, audience.project.UID, event.ProjectUID)
					if audience.typ == db.ProjectScopeResetEventType {
						require.Empty(t, event.UID)
						require.Empty(t, event.Payload)
					}
				}
			}
			require.True(t, found, "the exact transaction cursor must reach both audiences")
		})
	}
}

// Series history follows its current owner before and after physical source
// project purge, retaining original event identity and never recreating projects.
func RunMovedSeriesHistory(t *testing.T, store db.Storage) {
	t.Helper()
	ctx := t.Context()
	mover := store.(interface {
		MoveIssueTreeLocal(context.Context, db.MoveIssueTreeIn) (db.MoveIssueTreeOut, error)
	})
	source, err := store.CreateProject(ctx, "history-series-source")
	require.NoError(t, err)
	destination, err := store.CreateProject(ctx, "history-series-destination")
	require.NoError(t, err)
	root, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "Recurring history", Author: "tester"})
	require.NoError(t, err)
	attached, err := store.CreateRecurrenceForIssue(ctx, db.CreateRecurrenceForIssueIn{IssueID: root.ID, Recurrence: db.CreateRecurrenceIn{ProjectID: source.ID, Actor: "tester", Rule: "FREQ=DAILY", DTStart: "2026-10-12", Timezone: "UTC", Template: db.RecurrenceTemplate{Title: "Daily task"}}})
	require.NoError(t, err)
	var original db.Event
	for _, event := range attached.Events {
		if event.Type == "recurrence.created" {
			original = event
		}
	}
	require.NotEmpty(t, original.UID)
	current, err := store.IssueByID(ctx, root.ID)
	require.NoError(t, err)
	_, err = mover.MoveIssueTreeLocal(ctx, db.MoveIssueTreeIn{MoveSeries: true, MoveIssueProjectIn: db.MoveIssueProjectIn{IssueID: root.ID, FromProjectID: source.ID, ToProjectID: destination.ID, IfMatchRev: current.Revision, Actor: "tester"}})
	require.NoError(t, err)
	for _, purged := range []bool{false, true} {
		stage := "source-present"
		if purged {
			stage = "source-purged"
			_, _, err = store.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: source.ID, Actor: "tester"})
			require.NoError(t, err)
			_, err = store.PurgeProject(ctx, db.PurgeProjectParams{ProjectID: source.ID, Actor: "tester"})
			require.NoError(t, err)
		}
		t.Run(stage, func(t *testing.T) {
			for _, scope := range []struct {
				name    string
				allowed []string
				visible bool
			}{
				{"owner", nil, true}, {"current-project", []string{destination.UID}, true}, {"old-project", []string{source.UID}, false}, {"no-projects", []string{}, false},
			} {
				t.Run(scope.name, func(t *testing.T) {
					readCtx := ctx
					if scope.allowed != nil {
						readCtx = db.WithAuthorizedProjects(ctx, scope.allowed)
					}
					events, err := store.EventsAfter(readCtx, db.EventsAfterParams{ProjectID: destination.ID, Limit: 100})
					require.NoError(t, err)
					global, globalErr := store.EventsAfter(readCtx, db.EventsAfterParams{Limit: 100})
					require.NoError(t, globalErr)
					window, windowErr := store.EventsInWindow(readCtx, db.EventsInWindowParams{ProjectID: destination.ID, Since: "2000-01-01T00:00:00.000Z", Until: "2099-01-01T00:00:00.000Z"})
					require.NoError(t, windowErr)
					for _, page := range [][]db.Event{events, global, window} {
						found := false
						for _, event := range page {
							if event.UID == original.UID {
								found = true
								require.Equal(t, original.ProjectUID, event.ProjectUID)
								require.Equal(t, original.ContentHash, event.ContentHash)
								require.Equal(t, original.Payload, event.Payload)
								if purged {
									require.Zero(t, event.ProjectID)
								}
							}
						}
						require.Equal(t, scope.visible, found)
					}
				})
			}
		})
	}
}
