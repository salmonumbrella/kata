package db

import (
	"context"
	"database/sql"
	"fmt"
)

type ownerMappingCleanupSQL interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

// DeleteOrdinaryProjectOwnerMappings applies the old cascade behavior before
// nullable owner pointers are detached. Moved source identities remain durable.
func DeleteOrdinaryProjectOwnerMappings(ctx context.Context, q ownerMappingCleanupSQL, projectID int64) error {
	_, err := q.ExecContext(ctx, `DELETE FROM import_mappings WHERE moved_issue_uid IS NULL AND (
issue_id IN (SELECT id FROM issues WHERE project_id=$1)
OR comment_id IN (SELECT id FROM comments WHERE issue_id IN (SELECT id FROM issues WHERE project_id=$1))
OR link_id IN (SELECT id FROM links WHERE from_issue_id IN (SELECT id FROM issues WHERE project_id=$1) OR to_issue_id IN (SELECT id FROM issues WHERE project_id=$1)))`, projectID)
	if err != nil {
		return fmt.Errorf("remove ordinary project owner mappings: %w", err)
	}
	return nil
}

// DeleteOrdinaryIssueOwnerMappings removes mappings for an issue and owners
// cascading with it, while retaining portable moved-source suppression facts.
func DeleteOrdinaryIssueOwnerMappings(ctx context.Context, q ownerMappingCleanupSQL, issueID int64) error {
	_, err := q.ExecContext(ctx, `DELETE FROM import_mappings WHERE moved_issue_uid IS NULL AND (
issue_id=$1 OR comment_id IN (SELECT id FROM comments WHERE issue_id=$1)
OR link_id IN (SELECT id FROM links WHERE from_issue_id=$1 OR to_issue_id=$1))`, issueID)
	if err != nil {
		return fmt.Errorf("remove ordinary issue owner mappings: %w", err)
	}
	return nil
}

// DeleteOrdinaryCommentMappings retains moved identities before the local
// comment pointer is detached by deletion in the same transaction.
func DeleteOrdinaryCommentMappings(ctx context.Context, q ownerMappingCleanupSQL, commentID int64) error {
	_, err := q.ExecContext(ctx, `DELETE FROM import_mappings WHERE moved_issue_uid IS NULL AND comment_id=$1`, commentID)
	if err != nil {
		return fmt.Errorf("remove ordinary comment mappings: %w", err)
	}
	return nil
}
