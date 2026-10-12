package jsonl_test

import (
	"database/sql"
	"encoding/json/jsontext"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/jsonl"
)

// SQLite cutover needs the same real-owner backfills as the PostgreSQL forward
// migration; a retained foreign signing authority is not the local instance.
func TestIssueMoveCutoverBackfillsFrozen33(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "source.db")
	source, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	seed, pin, receipt := dbtest.FrozenRelay33Fixture(t)
	seed = strings.Replace(seed, "'instance_uid','00000000000000000000000009'", "'instance_uid','00000000000000000000000010'", 1)
	_, err = source.ExecContext(ctx, relaySchema33+seed+`
 INSERT INTO recurrences(uid,project_id,rrule,dtstart,timezone,template_title,author)
 SELECT '00000000000000000000000006',id,'FREQ=DAILY','2026-10-12','UTC','Daily task','tester' FROM projects WHERE uid='00000000000000000000000001';
 UPDATE issues SET recurrence_id=(SELECT id FROM recurrences WHERE uid='00000000000000000000000006'),occurrence_key='2026-10-12' WHERE uid='00000000000000000000000004';
 INSERT INTO issues(uid,project_id,short_id,title,author) SELECT '00000000000000000000000007',id,'0007','Peer task','tester' FROM projects WHERE uid='00000000000000000000000001';
 INSERT INTO links(from_issue_id,to_issue_id,from_issue_uid,to_issue_uid,type,author)
 SELECT first.id,second.id,first.uid,second.uid,'related','tester' FROM issues first,issues second WHERE first.uid='00000000000000000000000004' AND second.uid='00000000000000000000000007';
 `)
	require.NoError(t, err)

	event := db.EventHashInput{UID: "00000000000000000000000008", OriginInstanceUID: "00000000000000000000000010", ProjectUID: pin.ProjectUID, ProjectName: "shared-project", IssueUID: new("00000000000000000000000004"), Type: "issue.edited", Actor: "assistant", HLCPhysicalMS: 1, CreatedAt: "2026-10-12T00:00:00.000Z", Payload: jsontext.Value(`{}`)}
	hash, err := db.EventContentHash(event)
	require.NoError(t, err)
	_, err = source.ExecContext(ctx, `INSERT INTO events(uid,origin_instance_uid,project_id,project_name,issue_id,issue_uid,type,actor,payload,hlc_physical_ms,hlc_counter,content_hash,created_at) SELECT $1,$2,project_id,$3,id,uid,$4,$5,$6,$7,0,$8,$9 FROM issues WHERE uid='00000000000000000000000004'`, event.UID, event.OriginInstanceUID, event.ProjectName, event.Type, event.Actor, string(event.Payload), event.HLCPhysicalMS, hash, event.CreatedAt)
	require.NoError(t, err)
	require.NoError(t, source.Close())
	require.NoError(t, jsonl.AutoCutover(ctx, path))
	target, err := sqlitestore.Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, target.Close()) })
	dbtest.AssertFrozenRelay33(t, target, pin, receipt)
	var owner, seriesUID, seriesAuthority, seriesProject *string
	require.NoError(t, target.QueryRowContext(ctx, `SELECT owner_authority_uid,recurrence_uid,recurrence_authority_uid,recurrence_project_uid FROM issues WHERE uid='00000000000000000000000004'`).Scan(&owner, &seriesUID, &seriesAuthority, &seriesProject))
	require.Equal(t, new(pin.AuthorityUID), owner)
	require.Equal(t, new("00000000000000000000000006"), seriesUID)
	require.Equal(t, new(pin.AuthorityUID), seriesAuthority)
	require.Equal(t, new(pin.ProjectUID), seriesProject)
	var sourceProject, restoredHash string
	require.NoError(t, target.QueryRowContext(ctx, `SELECT source_project_uid,content_hash FROM events WHERE uid=$1`, event.UID).Scan(&sourceProject, &restoredHash))
	require.Equal(t, pin.ProjectUID, sourceProject)
	require.Equal(t, hash, restoredHash)
	var fromAuthority, fromProject, toAuthority, toProject *string
	require.NoError(t, target.QueryRowContext(ctx, `SELECT from_authority_uid,from_project_uid,to_authority_uid,to_project_uid FROM links WHERE type='related'`).Scan(&fromAuthority, &fromProject, &toAuthority, &toProject))
	require.Equal(t, new(pin.AuthorityUID), fromAuthority)
	require.Equal(t, new(pin.ProjectUID), fromProject)
	require.Equal(t, new(pin.AuthorityUID), toAuthority)
	require.Equal(t, new(pin.ProjectUID), toProject)
}
