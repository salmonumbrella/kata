package pgstore_test

import (
	"crypto/sha256"
	"database/sql"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/testenv"
)

// The combined dependency schema must validate its real physical catalog.
func TestIntegratedSchemaCatalog(t *testing.T) {
	ctx := t.Context()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	ddl, err := os.ReadFile("schema.sql")
	require.NoError(t, err)
	tx, err := admin.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, "CREATE SCHEMA integrated_catalog;SET LOCAL search_path TO integrated_catalog,pg_catalog,pg_temp;"+string(ddl))
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES('schema_version',$1),('instance_uid','00000000000000000000000009')`, fmt.Sprint(db.CurrentSchemaVersion()))
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	queries := map[string]string{
		"column":     `SELECT 'COLUMN',table_name,column_name,udt_schema||'.'||udt_name,is_nullable FROM information_schema.columns WHERE table_schema='integrated_catalog'`,
		"constraint": `SELECT 'CONSTRAINT',c.relname,con.conname,con.contype::text,pg_get_constraintdef(con.oid,true) FROM pg_constraint con JOIN pg_class c ON c.oid=con.conrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='integrated_catalog' AND con.contype<>'n'`,
		"index":      `SELECT 'INDEX',idx.relname,tbl.relname,i.indisvalid::text,i.indisready::text,pg_get_indexdef(i.indexrelid) FROM pg_index i JOIN pg_class idx ON idx.oid=i.indexrelid JOIN pg_class tbl ON tbl.oid=i.indrelid JOIN pg_namespace n ON n.oid=idx.relnamespace WHERE n.nspname='integrated_catalog'`,
	}
	for kind, query := range queries {
		rows, err := admin.QueryContext(ctx, query)
		require.NoError(t, err)
		columns, err := rows.Columns()
		require.NoError(t, err)
		var records []string
		for rows.Next() {
			values := make([]string, len(columns))
			dest := make([]any, len(columns))
			for i := range values {
				dest[i] = &values[i]
			}
			require.NoError(t, rows.Scan(dest...))
			for i := range values {
				values[i] = strings.ReplaceAll(values[i], "integrated_catalog.", "")
			}
			records = append(records, strings.Join(values, "\x00"))
		}
		require.NoError(t, rows.Err())
		require.NoError(t, rows.Close())
		slices.Sort(records)
		t.Logf("canonical %s fingerprint: %x", kind, sha256.Sum256([]byte(strings.Join(records, "\n"))))
	}
	var owner string
	require.NoError(t, admin.QueryRowContext(ctx, "SELECT current_user").Scan(&owner))
	store, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: "integrated_catalog", SchemaMode: pgstore.SchemaModeValidate, SchemaOwner: owner})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
}
