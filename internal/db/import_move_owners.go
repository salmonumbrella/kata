package db

import (
	"context"
	"database/sql"
	"fmt"
)

// ReplayMoveOwners restores retained identities after their original events
// exist, inside the import transaction. It does not synthesize local owners.
func ReplayMoveOwners(ctx context.Context, tx *sql.Tx, records []ImportRecord) error {
	var sourceAuthority *string
	for _, record := range records {
		if meta, ok := record.(*MetaKV); ok && meta.Key == "instance_uid" && meta.Value != "" {
			sourceAuthority = new(meta.Value)
		}
	}
	for _, record := range records {
		switch r := record.(type) {

		case *IssueExport:
			var seriesUID *string
			if r.RecurrenceAuthorityUID != nil {
				seriesUID = r.RecurrenceUID
			}
			// Missing legacy fields derive only from real local owners and source
			// namespace metadata; explicit remote identities are never inferred away.
			if _, err := tx.ExecContext(ctx, `UPDATE issues SET
recurrence_uid=COALESCE($1,(SELECT uid FROM recurrences WHERE id=issues.recurrence_id)),
recurrence_authority_uid=COALESCE($2,(SELECT COALESCE(
 (SELECT k.authority_uid FROM federation_root_keys k JOIN projects p ON p.uid=k.project_uid WHERE p.id=r.project_id AND k.active=1),
 $6,(SELECT value FROM meta WHERE key='instance_uid')) FROM recurrences r WHERE r.id=issues.recurrence_id)),
recurrence_project_uid=COALESCE($3,(SELECT p.uid FROM recurrences r JOIN projects p ON p.id=r.project_id WHERE r.id=issues.recurrence_id)),
owner_authority_uid=COALESCE($4,(SELECT k.authority_uid FROM federation_root_keys k JOIN projects p ON p.uid=k.project_uid WHERE p.id=issues.project_id AND k.active=1),$6,(SELECT value FROM meta WHERE key='instance_uid'))
WHERE uid=$5`, seriesUID, r.RecurrenceAuthorityUID, r.RecurrenceProjectUID, r.OwnerAuthorityUID, r.UID, sourceAuthority); err != nil {
				return fmt.Errorf("restore issue move owners: %w", err)
			}
		case *RecurrenceExport:
			if r.MovedEventUID == nil {
				continue
			}
			if _, err := tx.ExecContext(ctx, `UPDATE recurrences SET moved_to_authority_uid=$1,moved_to_project_uid=$2,moved_event_uid=$3 WHERE uid=$4`, r.MovedToAuthorityUID, r.MovedToProjectUID, r.MovedEventUID, r.UID); err != nil {
				return fmt.Errorf("restore recurrence routing: %w", err)
			}
		}
	}

	// Resolve graph and history only after all issue namespaces are restored.
	for _, record := range records {
		switch r := record.(type) {
		case *LinkExport:
			if _, err := tx.ExecContext(ctx, `UPDATE links SET
from_authority_uid=COALESCE(from_authority_uid,(SELECT owner_authority_uid FROM issues WHERE id=links.from_issue_id)),
from_project_uid=COALESCE(from_project_uid,(SELECT p.uid FROM issues i JOIN projects p ON p.id=i.project_id WHERE i.id=links.from_issue_id)),
to_authority_uid=COALESCE(to_authority_uid,(SELECT owner_authority_uid FROM issues WHERE id=links.to_issue_id)),
to_project_uid=COALESCE(to_project_uid,(SELECT p.uid FROM issues i JOIN projects p ON p.id=i.project_id WHERE i.id=links.to_issue_id)) WHERE id=$1`, r.ID); err != nil {
				return fmt.Errorf("restore graph owner coordinates: %w", err)
			}
		case *EventExport:
			if _, err := tx.ExecContext(ctx, `UPDATE events SET source_project_uid=COALESCE(source_project_uid,NULLIF($1,''),(SELECT uid FROM projects WHERE id=events.project_id)) WHERE uid=$2`, r.ProjectUID, r.UID); err != nil {
				return fmt.Errorf("restore history source identity: %w", err)
			}
		}
	}
	return nil
}
