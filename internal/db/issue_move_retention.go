package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

type moveRetentionSQL interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type moveHistorySQL interface {
	moveRetentionSQL
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// RetainProjectMoveHistory runs inside the project purge transaction. History
// for relocated subjects keeps its original identity without a fabricated local
// project; ordinary rows still follow the existing purge policy.
func RetainProjectMoveHistory(ctx context.Context, q moveHistorySQL, projectID int64, postgres bool) error {
	// Purge already refuses joined projects. Their retained local reference
	// namespace is this service, independently of historical foreign proof keys.
	var projectUID, name, authorityUID string
	if err := q.QueryRowContext(ctx, `SELECT p.uid,p.name,(SELECT value FROM meta WHERE key='instance_uid') FROM projects p WHERE p.id=$1`, projectID).Scan(&projectUID, &name, &authorityUID); err != nil {
		return fmt.Errorf("load retained project identity: %w", err)
	}
	// Historical public keys remain usable for receipts, never for new signing.
	if _, err := q.ExecContext(ctx, `UPDATE federation_root_keys SET active=0 WHERE project_uid=$1`, projectUID); err != nil {
		return fmt.Errorf("retire purged project root keys: %w", err)
	}
	retained, err := retainRelocatedProjectHistory(ctx, q, projectID, projectUID, postgres)
	if err != nil {
		return err
	}
	return retainProjectMoveNamespace(ctx, q, projectID, projectUID, name, authorityUID, retained)
}

func retainProjectMoveNamespace(ctx context.Context, q moveRetentionSQL, projectID int64, projectUID, name, authorityUID string, retained int64) error {
	if retained == 0 {
		var detached bool
		if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM events WHERE project_id IS NULL AND source_project_uid=$1)`, projectUID).Scan(&detached); err != nil {
			return err
		}
		if !detached {
			return nil
		}
	}
	var reservedUID string
	err := q.QueryRowContext(ctx, `SELECT project_uid FROM project_aliases WHERE alias_kind='name' AND authority_uid=$1 AND alias_identity=$2`, authorityUID, name).Scan(&reservedUID)
	if err == nil && reservedUID != projectUID {
		return &ProjectNameRetainedError{Name: name, AuthorityUID: authorityUID, ReservedProjectUID: reservedUID}
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if errors.Is(err, sql.ErrNoRows) {
		if _, err = q.ExecContext(ctx, `INSERT INTO project_aliases(project_id,alias_identity,alias_kind,authority_uid,project_uid) VALUES(NULL,$1,'name',$2,$3)`, name, authorityUID, projectUID); err != nil {
			return fmt.Errorf("retain project name: %w", err)
		}
	}
	_, err = q.ExecContext(ctx, `UPDATE project_aliases SET project_id=NULL WHERE project_id=$1 AND alias_kind='name'`, projectID)
	return err
}

// RetainMovedIssueCreationHistory preserves the original signed envelopes before
// deleting a relocated issue's body. The real issue tombstone owns the retained
// comment identities; neither source projects nor comment bodies are invented.
func RetainMovedIssueCreationHistory(ctx context.Context, q moveHistorySQL, issueID int64) error {
	return retainMovedCreationHistory(ctx, q, "i.id=$1", issueID)
}

func retainMovedCreationHistory(ctx context.Context, q moveHistorySQL, predicate string, ownerID int64) error {
	rows, err := q.QueryContext(ctx, `SELECT e.uid,e.origin_instance_uid,COALESCE(e.source_project_uid,p.uid),e.project_name,e.issue_uid,e.related_issue_uid,e.type,e.actor,e.payload,e.hlc_physical_ms,e.hlc_counter,e.content_hash,e.created_at
 FROM events e LEFT JOIN projects p ON p.id=e.project_id JOIN issues i ON i.uid=e.issue_uid
 WHERE (`+predicate+`) AND i.location_generation>1 AND e.source_event_bytes IS NULL
 AND EXISTS(SELECT 1 FROM federation_entity_provenance ep JOIN federation_event_provenance r ON r.project_uid=ep.project_uid AND r.event_uid=ep.event_uid
 WHERE ep.event_uid=e.uid AND ep.project_uid=COALESCE(e.source_project_uid,p.uid) AND r.content_hash=e.content_hash)`, ownerID)
	if err != nil {
		return err
	}
	type original struct {
		event RemoteEvent
		raw   []byte
	}
	originals := []original{}
	for rows.Next() {
		var ev RemoteEvent
		var payload, created string
		if err = rows.Scan(&ev.EventUID, &ev.OriginInstanceUID, &ev.ProjectUID, &ev.ProjectName, &ev.IssueUID, &ev.RelatedIssueUID, &ev.Type, &ev.Actor, &payload, &ev.HLCPhysicalMS, &ev.HLCCounter, &ev.ContentHash, &created); err != nil {
			break
		}
		ev.Payload = []byte(payload)
		ev.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
		if err != nil {
			break
		}
		if _, _, err = ValidateRemoteEventContentHash(ev); err != nil {
			break
		}
		var raw []byte
		raw, err = EncodeRelaySourceEvent(ev)
		if err != nil {
			break
		}
		originals = append(originals, original{ev, raw})
	}
	if err == nil {
		err = rows.Err()
	}
	_ = rows.Close()
	if err != nil {
		return fmt.Errorf("retain moved creation source: %w", err)
	}
	for _, original := range originals {
		if _, err = q.ExecContext(ctx, `UPDATE events SET source_project_uid=$1,source_event_bytes=$2 WHERE uid=$3`, original.event.ProjectUID, original.raw, original.event.EventUID); err != nil {
			return err
		}
	}
	return nil
}

// RetainRelocatedProjectHistory protects moved history before a source replica
// reset or UID adoption. It does not retire the live project's signing key.
func RetainRelocatedProjectHistory(ctx context.Context, q moveHistorySQL, projectID int64, postgres bool) error {
	var projectUID, name, authorityUID string
	if err := q.QueryRowContext(ctx, `SELECT p.uid,p.name,(SELECT value FROM meta WHERE key='instance_uid') FROM projects p WHERE p.id=$1`, projectID).Scan(&projectUID, &name, &authorityUID); err != nil {
		return err
	}
	retained, err := retainRelocatedProjectHistory(ctx, q, projectID, projectUID, postgres)
	if err != nil {
		return err
	}
	return retainProjectMoveNamespace(ctx, q, projectID, projectUID, name, authorityUID, retained)
}

func retainRelocatedProjectHistory(ctx context.Context, q moveHistorySQL, projectID int64, projectUID string, postgres bool) (int64, error) {
	if err := retainMovedCreationHistory(ctx, q, "i.project_id=$1 OR e.project_id=$1", projectID); err != nil {
		return 0, err
	}
	retentionQuery := `UPDATE events SET source_project_uid=COALESCE(source_project_uid,$2),project_id=NULL
 WHERE project_id=$1 AND (source_event_bytes IS NOT NULL OR type IN ('issue.moved','issue.move_received') OR EXISTS (
 SELECT 1 FROM issues i WHERE (i.id=events.issue_id OR i.uid=events.issue_uid)
 AND (i.project_id<>$1 OR i.moved_to_authority_uid IS NOT NULL)) OR type='recurrence.moved' OR (type LIKE 'recurrence.%' AND EXISTS(SELECT 1 FROM recurrences r WHERE r.uid=json_extract(events.payload,'$.recurrence_uid') AND (r.project_id<>$1 OR r.moved_to_authority_uid IS NOT NULL))))`
	if postgres {
		retentionQuery = strings.ReplaceAll(retentionQuery, "json_extract(events.payload,'$.recurrence_uid')", "(events.payload::jsonb->>'recurrence_uid')")
	}
	result, err := q.ExecContext(ctx, retentionQuery, projectID, projectUID)
	if err != nil {
		return 0, fmt.Errorf("retain relocated history: %w", err)
	}
	retained, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	return retained, nil
}
