package pgstore_test

import (
	"database/sql"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/testenv"
)

func TestRejectLegacyCronSchema31(t *testing.T) {
	ctx := t.Context()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	const schema = "task12_legacy_31"
	source, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: schema, SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	require.NoError(t, source.Close())
	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, admin.Close()) })
	dropNativeCronSchema(ctx, t, admin, schema)
	legacy, err := os.ReadFile("testdata/legacy_cron31.sql")
	require.NoError(t, err)
	tx, err := admin.BeginTx(ctx, nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `SET LOCAL search_path TO `+schema+`,public`)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, string(legacy))
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	before := cronPhysicalShape(ctx, t, admin, schema)
	reopened, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: schema, SchemaMode: pgstore.SchemaModeBootstrap})
	if reopened != nil {
		require.NoError(t, reopened.Close())
	}
	require.Error(t, err)
	require.Equal(t, before, cronPhysicalShape(ctx, t, admin, schema), "rejection cannot repair experimental DDL")
	var version string
	require.NoError(t, admin.QueryRowContext(ctx, `SELECT value FROM `+schema+`.meta WHERE key='schema_version'`).Scan(&version))
	require.Equal(t, "31", version)
	_, err = admin.ExecContext(ctx, `DROP SCHEMA `+schema+` CASCADE`)
	require.NoError(t, err)
}
