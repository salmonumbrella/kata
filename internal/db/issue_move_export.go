package db

import (
	"context"
	"fmt"
	"strings"
)

// EventExportOwnerClause includes retained issue history in its current owner's
// export. Restricted readers need that real issue or purge owner, never merely
// a grant for the event's former source project.
func EventExportOwnerClause(ctx context.Context, filter ExportFilter, args []any, postgres bool) (string, []any) {
	bind := func(value any) string {
		args = append(args, value)
		if postgres {
			return fmt.Sprintf("$%d", len(args))
		}
		return "?"
	}
	allowed, restricted := AuthorizedProjects(ctx)
	if restricted && len(allowed) == 0 {
		return "FALSE", args
	}
	owner := func(tombstone bool, projectFilter bool) string {
		predicate := "subject_issue.id IS NOT NULL"
		id := "subject_issue.project_id"
		uid := "(SELECT uid FROM projects WHERE id=subject_issue.project_id)"
		if tombstone {
			predicate = "owner.issue_uid=events.issue_uid"
			id = "owner.project_id"
			uid = "owner.project_uid"
			if !filter.IncludeDeleted {
				return "FALSE"
			}
		}
		if projectFilter {
			predicate += " AND " + id + "=" + bind(*filter.ProjectID)
		}
		if restricted {
			places := make([]string, len(allowed))
			for i, value := range allowed {
				places[i] = bind(value)
			}
			predicate += " AND " + uid + " IN (" + strings.Join(places, ",") + ")"
		}
		if tombstone {
			return "EXISTS(SELECT 1 FROM purge_log owner WHERE " + predicate + ")"
		}
		return "(" + predicate + ")"
	}
	clauses := []string{}
	if filter.ProjectID != nil {
		// Keep existing owner-requested historical project exports and add the
		// retained lineage needed by a current-project restore.
		clauses = append(clauses, "(events.project_id="+bind(*filter.ProjectID)+" OR (events.source_project_uid IS NOT NULL AND ("+owner(false, true)+" OR "+owner(true, true)+")))")
	}
	if restricted {
		places := make([]string, len(allowed))
		for i, value := range allowed {
			places[i] = bind(value)
		}
		ordinary := "subject_issue.id IS NULL AND events.source_event_bytes IS NULL AND NOT EXISTS(SELECT 1 FROM purge_log owner WHERE owner.issue_uid=events.issue_uid) AND export_project.uid IN (" + strings.Join(places, ",") + ")"
		clauses = append(clauses, "("+owner(false, false)+" OR "+owner(true, false)+" OR ("+ordinary+"))")
	}
	if len(clauses) == 0 {
		return "TRUE", args
	}
	return strings.Join(clauses, " AND "), args
}

// ValidateExportedEventHash forbids export from repairing immutable signed or
// retained source history. Ordinary unsigned exports keep their existing peer
// scrubbing and compatibility hashing behavior.
func ValidateExportedEventHash(event *EventExport, computed string, receiptHash *string) error {
	if receiptHash != nil && (*receiptHash != event.ContentHash || computed != event.ContentHash) {
		return ErrRemoteEventHashMismatch
	}
	if len(event.SourceEventBytes) > 0 {
		original, err := DecodeRelaySourceEvent(event.SourceEventBytes)
		if err != nil {
			return err
		}
		if event.SourceProjectUID == nil || *event.SourceProjectUID != original.ProjectUID || event.UID != original.EventUID || event.ContentHash != original.ContentHash || computed != event.ContentHash {
			return ErrRemoteEventHashMismatch
		}
	}
	if receiptHash == nil && len(event.SourceEventBytes) == 0 {
		event.ContentHash = computed
	}
	return nil
}
