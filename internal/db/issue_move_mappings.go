package db

import (
	"context"
	"database/sql"
	"errors"
)

type localMoveMappingSQL interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

// ReclaimLocalMoveMappings removes only a destination suppression whose exact
// stable issue and route still identify this source. Other identities remain
// collisions. The caller has authorized both projects in this transaction.
func ReclaimLocalMoveMappings(ctx context.Context, q localMoveMappingSQL, issueID, sourceID, targetID int64) error {
	_, err := q.ExecContext(ctx, `DELETE FROM import_mappings AS target
 WHERE target.project_id=$1
 AND target.moved_issue_uid=(SELECT uid FROM issues WHERE id=$2)
 AND target.moved_to_authority_uid=(SELECT value FROM meta WHERE key='instance_uid')
 AND target.moved_to_project_uid=(SELECT uid FROM projects WHERE id=$3)
 AND EXISTS (SELECT 1 FROM import_mappings source WHERE source.issue_id=$2 AND source.project_id=$3
 AND source.source=target.source AND source.external_id=target.external_id AND source.object_type=target.object_type)`, targetID, issueID, sourceID)
	return err
}

// RehomeLocalMoveMappings retains body-free source external identities while
// keeping the current mapping IDs, checkpoints and pending intents at destination.
// Only IDs selected from this source are copied; unrelated destination mappings
// never become source suppressions. All state lives in existing typed owners.
func RehomeLocalMoveMappings(ctx context.Context, q localMoveMappingSQL, issueID, sourceID, targetID int64, disposition string) error {
	if disposition == "detach" {
		_, err := q.ExecContext(ctx, `UPDATE import_mappings SET issue_id=NULL,comment_id=NULL,link_id=NULL,label=NULL,
 observed_status=NULL,observed_status_at=NULL,pending_event_uid=NULL,remote_locator=NULL,
 moved_issue_uid=(SELECT uid FROM issues WHERE id=$1),
 moved_to_authority_uid=(SELECT value FROM meta WHERE key='instance_uid'),
 moved_to_project_uid=(SELECT uid FROM projects WHERE id=$2)
 WHERE issue_id=$1 AND project_id=$3`, issueID, targetID, sourceID)
		return err
	}
	rows, err := q.QueryContext(ctx, `SELECT id FROM import_mappings WHERE issue_id=$1 AND project_id=$2 ORDER BY id`, issueID, sourceID)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	if _, err := q.ExecContext(ctx, `UPDATE import_mappings SET project_id=$1 WHERE issue_id=$2 AND project_id=$3`, targetID, issueID, sourceID); err != nil {
		return err
	}
	for _, id := range ids {
		_, err := q.ExecContext(ctx, `INSERT INTO import_mappings(
 source,external_id,object_type,project_id,source_updated_at,imported_at,
 observed_status,observed_status_at,pending_event_uid,remote_locator,
 moved_issue_uid,moved_to_authority_uid,moved_to_project_uid)
 SELECT source,external_id,object_type,$1,source_updated_at,imported_at,
 observed_status,observed_status_at,pending_event_uid,remote_locator,
 (SELECT uid FROM issues WHERE id=$2),(SELECT value FROM meta WHERE key='instance_uid'),
 (SELECT uid FROM projects WHERE id=$3) FROM import_mappings WHERE id=$4`, sourceID, issueID, targetID, id)
		if err != nil {
			return err
		}
	}
	return nil
}
