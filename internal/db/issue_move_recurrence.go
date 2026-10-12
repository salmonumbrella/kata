package db

import (
	"context"
	"database/sql"
	"fmt"
)

// RetainLocalRecurrenceIdentity preserves the actual source series coordinates
// before its occurrence changes project. The series itself stays authoritative.
func RetainLocalRecurrenceIdentity(ctx context.Context, tx *sql.Tx, issueID int64) error {
	_, err := tx.ExecContext(ctx, `UPDATE issues SET
recurrence_uid=COALESCE(recurrence_uid,(SELECT uid FROM recurrences WHERE id=issues.recurrence_id)),
recurrence_project_uid=COALESCE(recurrence_project_uid,(SELECT p.uid FROM recurrences r JOIN projects p ON p.id=r.project_id WHERE r.id=issues.recurrence_id)),
recurrence_authority_uid=COALESCE(recurrence_authority_uid,(SELECT COALESCE(
 (SELECT k.authority_uid FROM federation_root_keys k WHERE k.project_uid=p.uid AND k.active=1),
 (SELECT value FROM meta WHERE key='instance_uid')) FROM recurrences r JOIN projects p ON p.id=r.project_id WHERE r.id=issues.recurrence_id))
WHERE id=$1 AND recurrence_id IS NOT NULL`, issueID)
	if err != nil {
		return fmt.Errorf("retain source recurrence identity: %w", err)
	}
	return nil
}

// LocalMoveSeries identifies existing series selected by retained occurrences.
// These records are ephemeral transaction inputs, not durable move membership.
type LocalMoveSeries struct {
	ID, FromProjectID   int64
	UID, FromProjectUID string
}

func SelectedLocalMoveSeries(ctx context.Context, tx *sql.Tx, memberIDs []int64) ([]LocalMoveSeries, error) {
	seen := make(map[int64]bool)
	var result []LocalMoveSeries
	for _, id := range memberIDs {
		var series LocalMoveSeries
		var moved *string
		err := tx.QueryRowContext(ctx, `SELECT r.id,r.uid,r.project_id,p.uid,r.moved_event_uid FROM recurrences r JOIN projects p ON p.id=r.project_id JOIN issues i ON i.recurrence_id=r.id WHERE i.id=$1`, id).Scan(&series.ID, &series.UID, &series.FromProjectID, &series.FromProjectUID, &moved)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return nil, err
		}
		if moved != nil {
			return nil, ErrIssueProjectChanged
		}
		if !seen[series.ID] {
			seen[series.ID] = true
			result = append(result, series)
		}
	}
	return result, nil
}

// RehomeLocalMoveSeries transfers future materialization and all selected
// occurrences' logical coordinates without changing their stable identities.
func RehomeLocalMoveSeries(ctx context.Context, tx *sql.Tx, series LocalMoveSeries, destinationID int64) error {
	if series.FromProjectID == destinationID {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE recurrences SET project_id=$1,location_generation=location_generation+1,revision=revision+1 WHERE id=$2`, destinationID, series.ID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE issues SET recurrence_uid=$1,
recurrence_project_uid=(SELECT uid FROM projects WHERE id=$2),
recurrence_authority_uid=COALESCE((SELECT k.authority_uid FROM federation_root_keys k JOIN projects p ON p.uid=k.project_uid WHERE p.id=$2 AND k.active=1),(SELECT value FROM meta WHERE key='instance_uid'))
WHERE recurrence_id=$3`, series.UID, destinationID, series.ID)
	return err
}
