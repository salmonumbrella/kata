package dbtest

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// Finalized source mappings cannot be dispatched or acknowledged by a retained
// provider claim. Pending intent remains on the mapping for transfer/audit.
func RunMovedStatusAdmission(t *testing.T, store db.Storage) {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "status-source")
	require.NoError(t, err)
	destination, err := store.CreateProject(ctx, "status-target")
	require.NoError(t, err)
	binding, err := store.UpsertIssueSyncBinding(ctx, db.UpsertIssueSyncBindingParams{ProjectID: project.ID, Provider: "notion", SourceKey: "notion:example-source", RemoteID: "example-source", DisplayName: "Example tasks", Config: jsontext.Value(`{"status_sync":"two-way"}`), IntervalSeconds: 60})
	require.NoError(t, err)
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	_, claimed, err := store.ClaimIssueSyncBinding(ctx, binding.ID, "notion", now, now.Add(-time.Hour))
	require.NoError(t, err)
	require.True(t, claimed)
	guard := db.IssueSyncImportGuard{BindingID: binding.ID, Provider: "notion", StartedAt: now}
	issue, event, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Moved status source", Author: "tester"})
	require.NoError(t, err)
	mapping, err := store.UpsertImportMapping(ctx, db.ImportMappingParams{ProjectID: project.ID, Source: binding.SourceKey, ObjectType: "issue", ExternalID: "source-1", IssueID: &issue.ID})
	require.NoError(t, err)
	active, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Active status source", Author: "tester"})
	require.NoError(t, err)
	current, err := store.UpsertImportMapping(ctx, db.ImportMappingParams{ProjectID: project.ID, Source: binding.SourceKey, ObjectType: "issue", ExternalID: "source-2", IssueID: &active.ID})
	require.NoError(t, err)
	_, closedEvent, _, err := store.CloseIssue(ctx, issue.ID, "done", "tester", "This source issue completed before its ownership moved to the destination.", nil)
	require.NoError(t, err)
	require.NotNil(t, closedEvent)
	event = *closedEvent
	before, err := store.(db.IssueStatusReader).IssueStatusMappingByID(ctx, guard, mapping.ID)
	require.NoError(t, err)
	require.NotNil(t, before.PendingEvent)
	require.Equal(t, event.UID, before.PendingEvent.UID)
	q := store.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
		QueryRowContext(context.Context, string, ...any) *sql.Row
	})
	_, err = q.ExecContext(ctx, `UPDATE import_mappings SET moved_issue_uid=$1,moved_to_authority_uid=$2,moved_to_project_uid=$3,pending_event_uid=$4 WHERE id=$5`, issue.UID, "01AAAAAAAAAAAAAAAAAAAAAAAA", destination.UID, event.UID, mapping.ID)
	require.NoError(t, err)
	reader := store.(db.IssueStatusReader)
	t.Run("by-id", func(t *testing.T) {
		_, err := reader.IssueStatusMappingByID(ctx, guard, mapping.ID)
		require.ErrorIs(t, err, db.ErrNotFound)
	})
	t.Run("page-before-limit", func(t *testing.T) {
		page, err := reader.ListIssueStatusMappings(ctx, db.IssueStatusQuery{Guard: guard, Limit: 1})
		require.NoError(t, err)
		require.Len(t, page.Mappings, 1)
		require.Equal(t, current.ID, page.Mappings[0].Mapping.ID)
	})
	t.Run("pending-page", func(t *testing.T) {
		page, err := reader.ListIssueStatusMappings(ctx, db.IssueStatusQuery{Guard: guard, Limit: 1, PendingOnly: true})
		require.NoError(t, err)
		require.Empty(t, page.Mappings)
	})
	t.Run("dispatch-count", func(t *testing.T) {
		count, err := store.(db.IssueStatusSummaryReader).CountPendingIssueStatuses(ctx, binding.ID)
		require.NoError(t, err)
		require.Zero(t, count, "retained audit intent is no longer dispatchable source work")
	})
	t.Run("observation", func(t *testing.T) {
		changed, events, err := store.(db.IssueStatusWriter).ObserveIssueStatus(ctx, db.IssueStatusObservationParams{Guard: guard, MappingID: mapping.ID, ExternalID: "source-1", IssueUID: issue.UID, Status: "closed", ClosedReason: "done", Observation: db.IssueStatusObservation{Version: time.Now().UTC().Add(time.Minute)}, ServicedEventUID: event.UID})
		require.ErrorIs(t, err, db.ErrNotFound)
		require.False(t, changed)
		require.Empty(t, events)
	})
	var pending string
	require.NoError(t, q.QueryRowContext(ctx, `SELECT pending_event_uid FROM import_mappings WHERE id=$1`, mapping.ID).Scan(&pending))
	require.Equal(t, event.UID, pending)
}
