package jsonl

import (
	"bytes"
	"encoding/json/v2"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

// The cutover exporter must preserve existing move owners instead of restoring
// a finalized source as writable work when the next schema upgrade runs.
func TestCutoverExportPreservesMoveOwners(t *testing.T) {
	ctx := t.Context()
	source, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "source.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, source.Close()) })
	target, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "target.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, target.Close()) })
	project, err := source.CreateProject(ctx, "source-project")
	require.NoError(t, err)
	issue, event, err := source.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Retained task", Author: "tester"})
	require.NoError(t, err)
	originalBytes, err := json.Marshal(db.RemoteEventFromStored(event))
	require.NoError(t, err)
	series, _, err := source.CreateRecurrence(ctx, db.CreateRecurrenceIn{ProjectID: project.ID, Actor: "tester", Rule: "FREQ=DAILY", DTStart: "2026-10-12", Timezone: "UTC", Template: db.RecurrenceTemplate{Title: "Daily task"}})
	require.NoError(t, err)
	_, err = source.ExecContext(ctx, `UPDATE issues SET location_generation=2,moved_to_authority_uid=$1,moved_to_project_uid=$2,moved_to_short_id=$3,moved_event_uid=$4,recurrence_uid=$5,recurrence_authority_uid=$1,recurrence_project_uid=$2,owner_authority_uid=$1,occurrence_key='2026-10-12' WHERE id=$6`, source.InstanceUID(), project.UID, issue.ShortID, event.UID, series.UID, issue.ID)
	require.NoError(t, err)
	_, err = source.ExecContext(ctx, `UPDATE recurrences SET location_generation=3,moved_to_authority_uid=$1,moved_to_project_uid=$2,moved_event_uid=$3 WHERE id=$4`, source.InstanceUID(), project.UID, event.UID, series.ID)
	require.NoError(t, err)
	_, err = source.ExecContext(ctx, `UPDATE events SET project_id=NULL,source_project_uid=$1,source_event_bytes=$2 WHERE uid=$3`, project.UID, originalBytes, event.UID)
	require.NoError(t, err)
	_, err = source.ExecContext(ctx, `INSERT INTO project_aliases(project_id,alias_identity,alias_kind,authority_uid,project_uid) VALUES(NULL,'historical-project','name',$1,$2)`, source.InstanceUID(), project.UID)
	require.NoError(t, err)
	_, err = source.ExecContext(ctx, `INSERT INTO links(from_issue_id,to_issue_id,from_issue_uid,to_issue_uid,type,author,from_authority_uid,from_project_uid,to_authority_uid,to_project_uid) VALUES($1,NULL,$2,$3,'blocks','tester',$4,$5,$6,$7)`, issue.ID, issue.UID, "01AAAAAAAAAAAAAAAAAAAAAAAA", source.InstanceUID(), project.UID, "01BBBBBBBBBBBBBBBBBBBBBBBB", "01CCCCCCCCCCCCCCCCCCCCCCCC")
	require.NoError(t, err)
	mapping, err := source.UpsertImportMapping(ctx, db.ImportMappingParams{ProjectID: project.ID, Source: "example-provider", ExternalID: "task-1", ObjectType: "issue", IssueID: &issue.ID})
	require.NoError(t, err)
	_, err = source.ExecContext(ctx, `UPDATE import_mappings SET moved_issue_uid=$1,moved_to_authority_uid=$2,moved_to_project_uid=$3 WHERE id=$4`, issue.UID, source.InstanceUID(), project.UID, mapping.ID)
	require.NoError(t, err)
	var output bytes.Buffer
	require.NoError(t, exportForCutover(ctx, source, &output, ExportOptions{IncludeDeleted: true}))
	require.NoError(t, Import(ctx, bytes.NewReader(output.Bytes()), target))
	var generation int64
	var route, recurrence, owner string
	require.NoError(t, target.QueryRowContext(ctx, `SELECT location_generation,moved_to_project_uid,recurrence_uid,owner_authority_uid FROM issues WHERE uid=$1`, issue.UID).Scan(&generation, &route, &recurrence, &owner))
	require.Equal(t, int64(2), generation)
	require.Equal(t, project.UID, route)
	require.Equal(t, series.UID, recurrence)
	require.Equal(t, source.InstanceUID(), owner)
	var seriesGeneration int64
	var seriesRoute string
	require.NoError(t, target.QueryRowContext(ctx, `SELECT location_generation,moved_to_project_uid FROM recurrences WHERE uid=$1`, series.UID).Scan(&seriesGeneration, &seriesRoute))
	require.Equal(t, int64(3), seriesGeneration)
	require.Equal(t, project.UID, seriesRoute)
	var nameOwner string
	require.NoError(t, target.QueryRowContext(ctx, `SELECT project_uid FROM project_aliases WHERE alias_kind='name' AND alias_identity='historical-project'`).Scan(&nameOwner))
	require.Equal(t, project.UID, nameOwner)
	var remoteProject string
	require.NoError(t, target.QueryRowContext(ctx, `SELECT to_project_uid FROM links WHERE to_issue_id IS NULL AND from_issue_uid=$1`, issue.UID).Scan(&remoteProject))
	require.Equal(t, "01CCCCCCCCCCCCCCCCCCCCCCCC", remoteProject)
	var movedUID string
	require.NoError(t, target.QueryRowContext(ctx, `SELECT moved_issue_uid FROM import_mappings WHERE id=$1`, mapping.ID).Scan(&movedUID))
	require.Equal(t, issue.UID, movedUID)
	var sourceUID, hash string
	var restoredBytes []byte
	require.NoError(t, target.QueryRowContext(ctx, `SELECT source_project_uid,content_hash,source_event_bytes FROM events WHERE uid=$1`, event.UID).Scan(&sourceUID, &hash, &restoredBytes))
	require.Equal(t, project.UID, sourceUID)
	require.Equal(t, event.ContentHash, hash)
	require.Equal(t, originalBytes, restoredBytes)
}
