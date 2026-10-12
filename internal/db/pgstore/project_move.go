package pgstore

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/kata/internal/db"
)

// errMovePreview rolls back provisional validation updates on a successful preview.
var errMovePreview = errors.New("move preview complete")

// MoveIssueProject rehomes one active, non-recurring issue while preserving
// stable UID relationships and allocating a fresh target-local short ID.
func (s *Store) MoveIssueProject(ctx context.Context, input db.MoveIssueProjectIn) (db.MoveIssueProjectOut, error) {
	var output db.MoveIssueProjectOut
	if input.FromProjectID == input.ToProjectID {
		return output, fmt.Errorf("source and target projects are the same")
	}
	err := s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		if err := lockProjectAccessExclusive(ctx, tx); err != nil {
			return err
		}
		var err error
		output, err = s.moveIssueProjectTx(ctx, tx, input, false, false, false)
		if err != nil {
			return err
		}
		if input.DryRun {
			return errMovePreview
		}

		return nil
	})
	if errors.Is(err, errMovePreview) {
		return output, nil
	}
	if err != nil {
		return db.MoveIssueProjectOut{}, err
	}
	return output, nil
}

// moveIssueProjectTx keeps all selected members in the caller's transaction.
func (s *Store) moveIssueProjectTx(ctx context.Context, tx *sql.Tx, input db.MoveIssueProjectIn, includeDeleted, preserveProviderIntent, allowOccurrence bool) (db.MoveIssueProjectOut, error) {
	var output db.MoveIssueProjectOut
	if err := ensureFederatedMoveAllowedTx(ctx, tx, input.FromProjectID, input.ToProjectID); err != nil {
		return output, err
	}
	current, source, err := lockedIssueTx(ctx, tx, input.IssueID, includeDeleted)
	if err != nil {
		return output, err
	}
	if current.ProjectID != input.FromProjectID {
		return output, db.ErrNotFound
	}
	if err := ensureProjectWritableTx(ctx, tx, input.FromProjectID); err != nil {
		return output, err
	}
	if input.IfMatchRev != current.Revision {
		return output, &db.RevisionConflictError{CurrentRevision: current.Revision}
	}
	if current.RecurrenceID != nil {
		if !allowOccurrence {
			return output, &db.RecurrencePinnedError{}
		}
		if err := db.RetainLocalRecurrenceIdentity(ctx, tx, current.ID); err != nil {
			return output, err
		}
	}
	if err := lockAndRejectFreshExternalRootClaimsForIssueTx(
		ctx, tx, current.ID, time.Now().UTC().Add(-db.ExternalRootClaimStaleAfter),
	); err != nil {
		return output, err
	}
	target, err := scanProject(tx.QueryRowContext(ctx,
		projectSelect+` WHERE id = $1 AND deleted_at IS NULL FOR SHARE`, input.ToProjectID))
	if err != nil {
		return output, err
	}
	if err := ensureProjectWritableTx(ctx, tx, target.ID); err != nil {
		return output, err
	}
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, target.ID); err != nil {
		return output, mapSQLError(err, nil)
	}
	if err := rejectIssueMoveExternalRootIssueSyncTx(
		ctx, tx, current.ID, source.ID, target.ID,
	); err != nil {
		return output, err
	}
	if preserveProviderIntent {
		if err := db.ReclaimLocalMoveMappings(ctx, tx, current.ID, source.ID, target.ID); err != nil {
			return output, err
		}
	}
	mappingCollisions, err := issueMoveImportMappingCollisionsTx(
		ctx, tx, current.ID, source.ID, target.ID,
	)
	if err != nil {
		return output, err
	}
	if len(mappingCollisions) > 0 {
		return output, &db.ProjectMergeImportMappingCollisionError{Mappings: mappingCollisions}
	}
	if input.DryRun {
		output.Issue = current
		output.NewRevision = current.Revision
		return output, nil
	}
	newShortID, err := s.resolveShortIDTx(ctx, tx, target.ID, current.UID, "")
	if err != nil {
		return output, fmt.Errorf("allocate short_id in target: %w", err)
	}
	newRevision := current.Revision + 1
	updatedAt := mutationTimestamp()
	if _, err := tx.ExecContext(ctx, `UPDATE issues
SET project_id = $1, short_id = $2, revision = $3, updated_at = $4, location_generation = location_generation + 1
WHERE id = $5`, target.ID, newShortID, newRevision, updatedAt, current.ID); err != nil {
		return output, mapSQLError(err, nil)
	}
	if err := rebindIssueEmbeddingArtifactsTx(ctx, tx, current.UID, target.UID); err != nil {
		return output, fmt.Errorf("rehome embedding artifacts: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE issue_claims SET project_id = $1 WHERE issue_id = $2`, target.ID, current.ID); err != nil {
		return output, fmt.Errorf("rehome issue claims: %w", mapSQLError(err, nil))
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE pending_claim_requests SET project_id = $1 WHERE issue_id = $2`, target.ID, current.ID); err != nil {
		return output, fmt.Errorf("rehome pending claim requests: %w", mapSQLError(err, nil))
	}

	// A whole-tree move preserves queued provider work and its exact event.
	// Legacy single-issue callers retain their original checkpoint reset.
	if preserveProviderIntent {
		if err := db.RehomeLocalMoveMappings(ctx, tx, current.ID, source.ID, target.ID, input.ProviderDisposition); err != nil {
			return output, err
		}
	} else {
		if _, err := tx.ExecContext(ctx, `UPDATE import_mappings SET project_id=$1,observed_status=NULL,observed_status_at=NULL,pending_event_uid=NULL,remote_locator=NULL WHERE issue_id=$2 AND project_id=$3`, target.ID, current.ID, source.ID); err != nil {
			return output, fmt.Errorf("rehome import mappings: %w", mapSQLError(err, nil))
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE external_root_bindings
SET project_id = $1 WHERE issue_id = $2`,
		target.ID, current.ID); err != nil {
		return output, fmt.Errorf("rehome external root bindings: %w", mapSQLError(err, nil))
	}

	if preserveProviderIntent && input.ProviderDisposition == "detach" {
		if _, err := tx.ExecContext(ctx, `UPDATE external_root_bindings SET enabled=$1,paused_reason=$2 WHERE issue_id=$3`, 0, "detached by issue move", current.ID); err != nil {
			return output, err
		}
	}

	facts := map[string]any{
		"issue_uid": current.UID, "from_project_uid": source.UID,
		"from_short_id": current.ShortID, "to_project_uid": target.UID,
		"to_short_id": newShortID, "updated_at": updatedAt,
	}
	if input.RequestUID != "" {
		facts["request_uid"] = input.RequestUID
		facts["request_digest"] = input.RequestDigest
		facts["actor_authority_uid"] = input.ActorAuthorityUID
		facts["provider_disposition"] = input.ProviderDisposition
		facts["from_authority_uid"] = s.instanceUID
		facts["to_authority_uid"] = s.instanceUID
		facts["revision"] = newRevision
		var generation int64
		if err := tx.QueryRowContext(ctx, `SELECT location_generation FROM issues WHERE id=$1`, current.ID).Scan(&generation); err != nil {
			return output, err
		}
		facts["location_generation"] = generation
	}
	payload, err := json.Marshal(facts)

	if err != nil {
		return output, fmt.Errorf("marshal issue move event: %w", err)
	}
	event, err := s.insertEventTx(ctx, tx, eventInsert{
		ProjectID: target.ID, ProjectUID: target.UID, ProjectName: target.Name,
		IssueID: &current.ID, IssueUID: &current.UID,
		Type: "issue.moved", Actor: input.Actor, Payload: string(payload),
	})
	if err != nil {
		return output, err
	}
	if err := bumpProjectAccess(ctx, tx); err != nil {
		return output, fmt.Errorf("bump project access revision after issue move: %w", err)
	}
	if err := tx.QueryRowContext(ctx,
		`SELECT CAST(value AS BIGINT) FROM meta WHERE key='project_access_revision'`,
	).Scan(&output.ProjectAccessRevision); err != nil {
		return output, mapSQLError(err, nil)
	}
	output.Issue, err = scanIssue(tx.QueryRowContext(ctx, issueSelect+` WHERE i.id = $1`, current.ID))
	output.Event = &event
	output.EventID = event.ID
	output.NewShortID = newShortID
	output.NewRevision = newRevision
	return output, err
}

func rejectIssueMoveExternalRootIssueSyncTx(
	ctx context.Context,
	tx *sql.Tx,
	issueID int64,
	sourceProjectID int64,
	targetProjectID int64,
) error {
	var bindingID int64
	err := tx.QueryRowContext(ctx, `SELECT b.id
		FROM external_root_bindings b
		JOIN import_mappings m ON m.issue_id=b.issue_id
		JOIN issue_sync_bindings s ON s.project_id=$1 AND s.source_key=m.source
		WHERE b.issue_id=$2 AND b.active=1
		  AND m.project_id=$3 AND m.object_type='issue'
		LIMIT 1`, targetProjectID, issueID, sourceProjectID).Scan(&bindingID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return mapSQLError(err, nil)
	}
	return db.ErrExternalRootIssueSyncConflict
}

func issueMoveImportMappingCollisionsTx(
	ctx context.Context,
	tx *sql.Tx,
	issueID int64,
	sourceProjectID int64,
	targetProjectID int64,
) ([]db.ProjectMergeImportMappingCollision, error) {
	rows, err := tx.QueryContext(ctx, `SELECT source.source, source.external_id, source.object_type
FROM import_mappings source
JOIN import_mappings target
  ON target.project_id = $1
 AND target.source = source.source
 AND target.external_id = source.external_id
 AND target.object_type = source.object_type
WHERE source.issue_id = $2 AND source.project_id = $3
ORDER BY source.source, source.object_type, source.external_id
LIMIT 20`, targetProjectID, issueID, sourceProjectID)
	if err != nil {
		return nil, mapSQLError(err, nil)
	}
	defer func() { _ = rows.Close() }()
	var collisions []db.ProjectMergeImportMappingCollision
	for rows.Next() {
		var collision db.ProjectMergeImportMappingCollision
		if err := rows.Scan(&collision.Source, &collision.ExternalID, &collision.ObjectType); err != nil {
			return nil, mapSQLError(err, nil)
		}
		collisions = append(collisions, collision)
	}
	return collisions, mapSQLError(rows.Err(), nil)
}
