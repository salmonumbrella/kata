package sqlitestore

import (
	"context"
	"database/sql"
	"fmt"

	"go.kenn.io/kata/internal/db"
)

type contextExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func deleteProjectRelayMetadata(ctx context.Context, execer contextExecer, projectUID string) error {
	resetKey := db.RelayResetMetadataPrefix + projectUID
	resetPrefix := resetKey + "."
	transitionPrefix := db.RootKeyTransitionMetadataPrefix + projectUID + "."
	_, err := execer.ExecContext(ctx, `DELETE FROM meta
		WHERE key = ?
		   OR substr(key, 1, length(?)) = ?
		   OR substr(key, 1, length(?)) = ?`,
		resetKey, resetPrefix, resetPrefix, transitionPrefix, transitionPrefix)
	return err
}

func retireProjectFederationUIDStateTx(ctx context.Context, tx *sql.Tx, projectUID string) error {
	for _, statement := range []struct{ table, query string }{
		{"federation_relay_outbox", `DELETE FROM federation_relay_outbox WHERE project_uid = ?`},
		{"federation_relay_inbox", `DELETE FROM federation_relay_inbox WHERE project_uid = ?`},
		{"federation_relay_cursors", `DELETE FROM federation_relay_cursors WHERE project_uid = ?`},
	} {
		if _, err := tx.ExecContext(ctx, statement.query, projectUID); err != nil {
			return fmt.Errorf("retire project relay %s: %w", statement.table, err)
		}
	}
	// Original signed receipts still need their public key after UID adoption.
	// Retired keys verify history; they cannot act as the new project's root.
	if _, err := tx.ExecContext(ctx, `UPDATE federation_root_keys SET active=0 WHERE project_uid=?`, projectUID); err != nil {
		return fmt.Errorf("retire project proof keys: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM federation_root_keys WHERE project_uid = ? AND NOT EXISTS (SELECT 1 FROM federation_event_provenance p WHERE p.project_uid=federation_root_keys.project_uid AND p.key_id=federation_root_keys.key_id)`, projectUID); err != nil {
		return fmt.Errorf("retire project root provenance: %w", err)
	}
	if err := deleteProjectRelayMetadata(ctx, tx, projectUID); err != nil {
		return fmt.Errorf("retire project relay metadata: %w", err)
	}
	pendingPrefix := db.PendingCreationMetadataPrefix + projectUID + "."
	if _, err := tx.ExecContext(ctx, `DELETE FROM meta
		WHERE key = ? OR substr(key, 1, length(?)) = ?`,
		db.AttributionUIResetMetadataPrefix+projectUID, pendingPrefix, pendingPrefix); err != nil {
		return fmt.Errorf("retire project attribution metadata: %w", err)
	}
	return nil
}
