package sqlitestore

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// Bulk moves must not make folding each retained event scan every move. Page
// fetches count the work even when SQLite serves the entire history from cache.
func TestFederationFoldBulkMovePageBudget(t *testing.T) {
	d, project := seedFederationFoldMoves(t, 20_000, 2_000)
	raw, err := sql.Open("sqlite", d.Path())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	raw.SetMaxOpenConns(1)
	conn, err := raw.Conn(t.Context())
	require.NoError(t, err)
	defer func() { require.NoError(t, conn.Close()) }()
	tx, err := conn.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { require.NoError(t, tx.Rollback()) }()
	cronPullPageFetches(t, conn, true)
	events, err := federationFoldEventsScoped(t.Context(), tx, project.ID, nil, nil)
	require.NoError(t, err)
	pages := cronPullPageFetches(t, conn, false)
	var retained int
	require.NoError(t, tx.QueryRowContext(t.Context(), "SELECT count(*) FROM events WHERE project_id=?", project.ID).Scan(&retained))
	require.Len(t, events, retained+20_000, "destination fold must retain the moved issues' source history")
	t.Logf("20k history / 2k moves: %d page fetches", pages)
	// A linear history read plus indexed lookups fits comfortably. The old
	// correlated move scan uses millions of page fetches for the same result.
	require.LessOrEqual(t, pages, 500_000, "bulk moves must not multiply history reads by move count")
}

func TestFederationFoldQueryUsesHistoryIndexes(t *testing.T) {
	d, project := seedFederationFoldMoves(t, 20_000, 2_000)
	for _, analyzed := range []bool{false, true} {
		if analyzed {
			_, err := d.ExecContext(t.Context(), "ANALYZE")
			require.NoError(t, err)
		}
		for _, types := range [][]string{nil, db.FederationLinkAffectingEventTypes()} {
			query, args := federationFoldEventsQuery(project.ID, types, nil)
			rows, err := d.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+query, args...)
			require.NoError(t, err)
			var plan []string
			for rows.Next() {
				var id, parent, unused int
				var detail string
				require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
				plan = append(plan, detail)
			}
			require.NoError(t, rows.Err())
			require.NoError(t, rows.Close())
			joined := strings.Join(plan, "\n")
			require.NotRegexp(t, `SCAN (e|x|m)(\s|$)`, joined, "analyzed=%t: %s", analyzed, joined)
			require.Contains(t, joined, "idx_events_resolved_issue_uid", "analyzed=%t", analyzed)
		}
	}
}

// Historical issue events may omit their envelope UID and identify the moved
// issue through either legacy payload spelling, including an empty envelope.
func TestFederationFoldMovedPayloadIdentity(t *testing.T) {
	for _, key := range []string{"issue_uid", "uid"} {
		for _, empty := range []bool{false, true} {
			t.Run(fmt.Sprintf("key=%s/empty=%t", key, empty), func(t *testing.T) {
				d, project := seedFederationFoldMoves(t, 20, 2)
				var envelope any
				if empty {
					envelope = ""
				}
				_, err := d.ExecContext(t.Context(), `UPDATE events SET issue_uid=$1,
     payload=json_set(json_remove(payload,'$.issue_uid'),'$.' || $2,json_extract(payload,'$.issue_uid'))
     WHERE type IN ('issue.updated','issue.moved')`, envelope, key)
				require.NoError(t, err)
				tx, err := d.BeginTx(t.Context(), nil)
				require.NoError(t, err)
				defer func() { require.NoError(t, tx.Rollback()) }()
				events, err := federationFoldEventsScoped(t.Context(), tx, project.ID, []string{"issue.updated"}, nil)
				require.NoError(t, err)
				require.Len(t, events, 20, "moved source history must resolve legacy payload identity")
			})
		}
	}
}

func BenchmarkFederationFoldBulkMoves(b *testing.B) {
	for _, moves := range []int{0, 3_400} {
		for _, group := range []bool{false, true} {
			b.Run(fmt.Sprintf("history=221000/moves=%d/group=%t", moves, group), func(b *testing.B) {
				d, project := seedFederationFoldMoves(b, 221_000, moves)
				projectIDs := []int64{project.ID}
				if group {
					var sourceID int64
					require.NoError(b, d.QueryRowContext(b.Context(), "SELECT id FROM projects WHERE name='source-project'").Scan(&sourceID))
					projectIDs = append(projectIDs, sourceID)
					for i := range 23 {
						p, err := d.CreateProject(b.Context(), fmt.Sprintf("group-project-%d", i))
						require.NoError(b, err)
						projectIDs = append(projectIDs, p.ID)
					}
				}
				tx, err := d.BeginTx(b.Context(), nil)
				require.NoError(b, err)
				defer func() { require.NoError(b, tx.Rollback()) }()
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					if group {
						_, err = federationGroupFoldProjection(b.Context(), tx, projectIDs)
					} else {
						_, err = federationFoldEventsScoped(b.Context(), tx, project.ID, nil, nil)
					}
					require.NoError(b, err)
				}
			})
		}
	}
}

// Seed already-materialized destination identities and retained source events,
// avoiding the unrelated cost of creating and replaying thousands of issues.
func seedFederationFoldMoves(t testing.TB, history, moves int) (*Store, db.Project) {
	t.Helper()
	d, destination, _ := incrementalTestStore(t)
	source, err := d.CreateProject(t.Context(), "source-project")
	require.NoError(t, err)
	_, err = d.ExecContext(t.Context(), `WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<$1)
 INSERT INTO issues(uid,short_id,project_id,title,author)
 SELECT printf('%026d',x),printf('%08d',x),$2,'Moved issue','tester' FROM n`, max(moves, 1), destination.ID)
	require.NoError(t, err)
	_, err = d.ExecContext(t.Context(), `WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<$1)
 INSERT INTO events(uid,origin_instance_uid,project_id,project_name,issue_uid,type,actor,payload,hlc_physical_ms,hlc_counter,content_hash)
 SELECT printf('%026d',x),$2,$3,'source-project',printf('%026d',1+(x-1)%$4),'issue.updated','tester',
 json_object('issue_uid',printf('%026d',1+(x-1)%$4),'title','Retained work'),x,0,$5 FROM n`, history, d.InstanceUID(), source.ID, max(moves, 1), strings.Repeat("a", 64))
	require.NoError(t, err)
	if moves == 0 {
		return d, destination
	}
	_, err = d.ExecContext(t.Context(), `WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<$1)
 INSERT INTO events(uid,origin_instance_uid,project_id,project_name,issue_uid,type,actor,payload,hlc_physical_ms,hlc_counter,content_hash)
 SELECT printf('%026d',x+$2),$3,$4,'hub-project',printf('%026d',x),'issue.moved','tester',
 json_object('issue_uid',printf('%026d',x),'from_project_uid',$5,'from_short_id',printf('%08d',x),'to_project_uid',$6,'to_short_id',printf('%08d',x),'updated_at','2026-05-23T12:00:00.000Z'),x+$2,0,$7 FROM n`, moves, history, d.InstanceUID(), destination.ID, source.UID, destination.UID, strings.Repeat("b", 64))
	require.NoError(t, err)
	return d, destination
}
