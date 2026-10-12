package db

import (
	"context"
	"database/sql"
	"fmt"
)

// DeleteLinkWithMappings preserves the ordinary cascading cleanup contract
// while retaining moved external identities. Both deletes share the caller's
// transaction so an unlink failure cannot silently discard its mapping.
func DeleteLinkWithMappings(ctx context.Context, tx *sql.Tx, linkID int64) (sql.Result, error) {
	if err := DeleteOrdinaryLinkMappings(ctx, tx, linkID); err != nil {
		return nil, err
	}
	return tx.ExecContext(ctx, `DELETE FROM links WHERE id=$1`, linkID)
}

// DeleteOrdinaryLinkMappings supports conditional edge deletion inside an
// existing transaction without weakening its expected-parent predicate.
func DeleteOrdinaryLinkMappings(ctx context.Context, tx *sql.Tx, linkID int64) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM import_mappings WHERE moved_issue_uid IS NULL AND link_id=$1`, linkID); err != nil {
		return fmt.Errorf("remove ordinary link mappings: %w", err)
	}
	return nil
}
