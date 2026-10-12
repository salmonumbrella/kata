package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/shortid"
)

// MoveIssueProject moves an issue from one project to another within the same
// database, allocating a fresh short_id in the target project and emitting an
// issue.moved event. It refuses if:
//   - source and target projects are the same
//   - IfMatchRev does not match the current revision (RevisionConflictError)
//   - the issue belongs to a recurrence series (RecurrencePinnedError)
func (d *Store) MoveIssueProject(ctx context.Context, in db.MoveIssueProjectIn) (db.MoveIssueProjectOut, error) {
	return retryWrite1(ctx, d, func() (db.MoveIssueProjectOut, error) {
		return d.moveIssueProject(ctx, in)
	})
}

func (d *Store) moveIssueProject(ctx context.Context, in db.MoveIssueProjectIn) (db.MoveIssueProjectOut, error) {
	var out db.MoveIssueProjectOut
	if in.FromProjectID == in.ToProjectID {
		return out, fmt.Errorf("source and target projects are the same")
	}

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockProjectAccess(ctx, tx); err != nil {
		return out, err
	}

	out, err = d.moveIssueProjectTx(ctx, tx, in, false, false, false)
	if err != nil {
		return out, err
	}
	if in.DryRun {
		return out, tx.Rollback()
	}
	return out, tx.Commit()
}

// moveIssueProjectTx shares one writer transaction with a whole-tree move.
// Legacy one-issue calls keep their existing deleted-row rejection.
func (d *Store) moveIssueProjectTx(ctx context.Context, tx *sql.Tx, in db.MoveIssueProjectIn, allowDeleted, preserveProviderIntent, allowOccurrence bool) (db.MoveIssueProjectOut, error) {
	var out db.MoveIssueProjectOut
	if err := ensureFederatedMoveAllowedTx(ctx, tx, in.FromProjectID, in.ToProjectID); err != nil {
		return out, err
	}

	var (
		curRev         int64
		curShortID     string
		recurrenceID   *int64
		issueUID       string
		fromProjectUID string
	)
	if err := tx.QueryRowContext(ctx, `
		SELECT i.revision, i.short_id, i.recurrence_id, i.uid, p.uid
		  FROM issues i JOIN projects p ON p.id = i.project_id
		 WHERE i.id = ? AND i.project_id = ? AND (? OR i.deleted_at IS NULL) AND p.deleted_at IS NULL`,
		in.IssueID, in.FromProjectID, allowDeleted,
	).Scan(&curRev, &curShortID, &recurrenceID, &issueUID, &fromProjectUID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return out, fmt.Errorf("issue %d not in project %d: %w", in.IssueID, in.FromProjectID, db.ErrNotFound)
		}
		return out, err
	}
	if err := ensureProjectWritableTx(ctx, tx, in.FromProjectID); err != nil {
		return out, err
	}
	if in.IfMatchRev != curRev {
		return out, &db.RevisionConflictError{CurrentRevision: curRev}
	}
	if recurrenceID != nil {
		if !allowOccurrence {
			return out, &db.RecurrencePinnedError{}
		}
		if err := db.RetainLocalRecurrenceIdentity(ctx, tx, in.IssueID); err != nil {
			return out, err
		}
	}
	if err := lockAndRejectFreshExternalRootClaimsForIssueTx(
		ctx, tx, in.IssueID, time.Now().UTC().Add(-db.ExternalRootClaimStaleAfter),
	); err != nil {
		return out, err
	}

	var (
		toProjectUID  string
		toProjectName string
	)
	if err := tx.QueryRowContext(ctx, `
		SELECT uid, name FROM projects WHERE id = ? AND deleted_at IS NULL`,
		in.ToProjectID,
	).Scan(&toProjectUID, &toProjectName); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return out, fmt.Errorf("target project %d not found", in.ToProjectID)
		}
		return out, err
	}
	if err := ensureProjectWritableTx(ctx, tx, in.ToProjectID); err != nil {
		return out, err
	}
	if err := rejectIssueMoveExternalRootIssueSyncTx(
		ctx, tx, in.IssueID, in.FromProjectID, in.ToProjectID,
	); err != nil {
		return out, err
	}
	if preserveProviderIntent {
		if err := db.ReclaimLocalMoveMappings(ctx, tx, in.IssueID, in.FromProjectID, in.ToProjectID); err != nil {
			return out, err
		}
	}
	mappingCollisions, err := issueMoveImportMappingCollisions(
		ctx, tx, in.IssueID, in.FromProjectID, in.ToProjectID,
	)
	if err != nil {
		return out, err
	}
	if len(mappingCollisions) > 0 {
		return out, &db.ProjectMergeImportMappingCollisionError{Mappings: mappingCollisions}
	}

	if in.DryRun {
		issue, err := issueByIDTx(ctx, tx, in.IssueID)
		if err != nil {
			return out, err
		}
		out.Issue = issue
		out.NewRevision = curRev
		// The enclosing transaction rolls back provisional validation changes.
		return out, nil
	}

	newShortID, err := assignShortIDIn(ctx, tx,
		[]int64{in.ToProjectID}, issueUID, shortid.MinLength,
	)
	if err != nil {
		return out, fmt.Errorf("allocate short_id in target: %w", err)
	}

	newRev := curRev + 1
	ts := nowTimestamp()
	if _, err := tx.ExecContext(ctx, `
		UPDATE issues
		   SET project_id = ?,
		       short_id   = ?,
		       location_generation = location_generation + 1,
       revision   = ?,
		       updated_at = ?
		 WHERE id = ?`,
		in.ToProjectID, newShortID, newRev, ts, in.IssueID,
	); err != nil {
		return out, err
	}
	if err := rebindIssueEmbeddingArtifactsTx(ctx, tx, issueUID, toProjectUID); err != nil {
		return out, fmt.Errorf("rehome embedding artifacts: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE issue_claims SET project_id = ? WHERE issue_id = ?`, in.ToProjectID, in.IssueID); err != nil {
		return out, fmt.Errorf("rehome issue claims: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE pending_claim_requests SET project_id = ? WHERE issue_id = ?`, in.ToProjectID, in.IssueID); err != nil {
		return out, fmt.Errorf("rehome pending claim requests: %w", err)
	}

	// Tree moves retain the pending event and provider checkpoint. The legacy
	// single-issue operation keeps its original reset behavior.
	if preserveProviderIntent {
		if err := db.RehomeLocalMoveMappings(ctx, tx, in.IssueID, in.FromProjectID, in.ToProjectID, in.ProviderDisposition); err != nil {
			return out, err
		}
	} else {
		if _, err := tx.ExecContext(ctx, `UPDATE import_mappings SET project_id=?, observed_status=NULL,observed_status_at=NULL,pending_event_uid=NULL,remote_locator=NULL WHERE issue_id=? AND project_id=?`, in.ToProjectID, in.IssueID, in.FromProjectID); err != nil {
			return out, fmt.Errorf("rehome import_mappings: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE external_root_bindings
		   SET project_id = ?
		 WHERE issue_id = ?`,
		in.ToProjectID, in.IssueID,
	); err != nil {
		return out, fmt.Errorf("rehome external root bindings: %w", err)
	}

	if preserveProviderIntent && in.ProviderDisposition == "detach" {
		if _, err := tx.ExecContext(ctx, `UPDATE external_root_bindings SET enabled=$1,paused_reason=$2 WHERE issue_id=$3`, false, "detached by issue move", in.IssueID); err != nil {
			return out, err
		}
	}

	facts := map[string]any{
		"issue_uid":        issueUID,
		"from_project_uid": fromProjectUID,
		"from_short_id":    curShortID,
		"to_project_uid":   toProjectUID,
		"to_short_id":      newShortID,
		"updated_at":       ts,
	}
	if in.RequestUID != "" {
		facts["request_uid"] = in.RequestUID
		facts["request_digest"] = in.RequestDigest
		facts["actor_authority_uid"] = in.ActorAuthorityUID
		facts["provider_disposition"] = in.ProviderDisposition
		facts["from_authority_uid"] = d.instanceUID
		facts["to_authority_uid"] = d.instanceUID
		facts["revision"] = newRev
		var generation int64
		if err := tx.QueryRowContext(ctx, `SELECT location_generation FROM issues WHERE id=$1`, in.IssueID).Scan(&generation); err != nil {
			return out, err
		}
		facts["location_generation"] = generation
	}
	payload, err := json.Marshal(facts)

	ev, err := d.insertEventTx(ctx, tx, eventInsert{
		ProjectID:   in.ToProjectID,
		ProjectName: toProjectName,
		IssueID:     &in.IssueID,
		IssueUID:    &issueUID,
		Type:        "issue.moved",
		Actor:       in.Actor,
		Payload:     string(payload),
	})
	if err != nil {
		return out, err
	}
	if err := bumpProjectAccess(ctx, tx); err != nil {
		return out, fmt.Errorf("bump project access revision after issue move: %w", err)
	}
	if err := tx.QueryRowContext(ctx,
		`SELECT CAST(value AS BIGINT) FROM meta WHERE key='project_access_revision'`,
	).Scan(&out.ProjectAccessRevision); err != nil {
		return out, fmt.Errorf("read project access revision after issue move: %w", err)
	}

	issue, err := issueByIDTx(ctx, tx, in.IssueID)
	if err != nil {
		return out, err
	}
	out.Issue = issue
	out.Event = &ev
	out.EventID = ev.ID
	out.NewShortID = newShortID
	out.NewRevision = newRev
	return out, nil
}

func rejectIssueMoveExternalRootIssueSyncTx(
	ctx context.Context,
	tx *sql.Tx,
	issueID int64,
	sourceProjectID int64,
	targetProjectID int64,
) error {
	if _, err := tx.ExecContext(ctx, `UPDATE projects SET id=id WHERE id=?`, targetProjectID); err != nil {
		return fmt.Errorf("lock issue move target project: %w", err)
	}
	var bindingID int64
	err := tx.QueryRowContext(ctx, `SELECT b.id
		FROM external_root_bindings b
		JOIN import_mappings m ON m.issue_id=b.issue_id
		JOIN issue_sync_bindings s ON s.project_id=? AND s.source_key=m.source
		WHERE b.issue_id=? AND b.active=1
		  AND m.project_id=? AND m.object_type='issue'
		LIMIT 1`, targetProjectID, issueID, sourceProjectID).Scan(&bindingID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("check issue move external root ownership: %w", err)
	}
	return db.ErrExternalRootIssueSyncConflict
}

func issueMoveImportMappingCollisions(
	ctx context.Context,
	tx *sql.Tx,
	issueID int64,
	sourceProjectID int64,
	targetProjectID int64,
) ([]db.ProjectMergeImportMappingCollision, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT source.source, source.external_id, source.object_type
		  FROM import_mappings source
		  JOIN import_mappings target
		    ON target.project_id = ?
		   AND target.source = source.source
		   AND target.external_id = source.external_id
		   AND target.object_type = source.object_type
		 WHERE source.issue_id = ? AND source.project_id = ?
		 ORDER BY source.source, source.object_type, source.external_id
		 LIMIT 20`, targetProjectID, issueID, sourceProjectID)
	if err != nil {
		return nil, fmt.Errorf("check issue move import mapping collisions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var collisions []db.ProjectMergeImportMappingCollision
	for rows.Next() {
		var collision db.ProjectMergeImportMappingCollision
		if err := rows.Scan(&collision.Source, &collision.ExternalID, &collision.ObjectType); err != nil {
			return nil, fmt.Errorf("scan issue move import mapping collision: %w", err)
		}
		collisions = append(collisions, collision)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate issue move import mapping collisions: %w", err)
	}
	return collisions, nil
}
