package pgstore

import (
	"context"
	"database/sql"
	"fmt"

	"go.kenn.io/kata/internal/db"
)

func deleteProjectRelayMetadataTx(ctx context.Context, tx *sql.Tx, projectUID string) error {
	resetKey := db.RelayResetMetadataPrefix + projectUID
	resetPrefix := resetKey + "."
	transitionPrefix := db.RootKeyTransitionMetadataPrefix + projectUID + "."
	_, err := tx.ExecContext(ctx, `DELETE FROM meta
		WHERE key = $1
		   OR left(key, length($2)) = $2
		   OR left(key, length($3)) = $3`,
		resetKey, resetPrefix, transitionPrefix)
	return mapSQLError(err, nil)
}

func retireProjectFederationUIDStateTx(ctx context.Context, tx *sql.Tx, projectUID string) error {
	for _, statement := range []struct{ table, query string }{
		{"federation_relay_outbox", `DELETE FROM federation_relay_outbox WHERE project_uid = $1`},
		{"federation_relay_inbox", `DELETE FROM federation_relay_inbox WHERE project_uid = $1`},
		{"federation_relay_cursors", `DELETE FROM federation_relay_cursors WHERE project_uid = $1`},
	} {
		if _, err := tx.ExecContext(ctx, statement.query, projectUID); err != nil {
			return fmt.Errorf("retire project relay %s: %w", statement.table, mapSQLError(err, nil))
		}
	}
	// Original signed receipts still need their public key after UID adoption.
	// Retired keys verify history; they cannot act as the new project's root.
	if _, err := tx.ExecContext(ctx, `UPDATE federation_root_keys SET active=0 WHERE project_uid=$1`, projectUID); err != nil {
		return fmt.Errorf("retire project proof keys: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM federation_root_keys WHERE project_uid = $1 AND NOT EXISTS (SELECT 1 FROM federation_event_provenance p WHERE p.project_uid=federation_root_keys.project_uid AND p.key_id=federation_root_keys.key_id)`, projectUID); err != nil {
		return fmt.Errorf("retire project root provenance: %w", mapSQLError(err, nil))
	}
	if err := deleteProjectRelayMetadataTx(ctx, tx, projectUID); err != nil {
		return fmt.Errorf("retire project relay metadata: %w", err)
	}
	pendingPrefix := db.PendingCreationMetadataPrefix + projectUID + "."
	if _, err := tx.ExecContext(ctx, `DELETE FROM meta
		WHERE key = $1 OR left(key, length($2)) = $2`,
		db.AttributionUIResetMetadataPrefix+projectUID, pendingPrefix); err != nil {
		return fmt.Errorf("retire project attribution metadata: %w", mapSQLError(err, nil))
	}
	return nil
}
