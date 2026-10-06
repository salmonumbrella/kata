package sqlitestore_test

import (
	"database/sql"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

func TestRejectLegacyCronSchema31(t *testing.T) {
	d, path := openTestDBWithPath(t)
	for _, table := range []string{"cron_issue_holders", "cron_run_claims", "cron_runs", "cron_jobs", "cron_workflows"} {
		_, err := d.ExecContext(t.Context(), "DROP TABLE IF EXISTS "+table)
		require.NoError(t, err)
	}
	legacy, err := os.ReadFile("testdata/legacy_cron31.sql")
	require.NoError(t, err)
	_, err = d.ExecContext(t.Context(), string(legacy))
	require.NoError(t, err)
	require.NoError(t, d.Close())
	reopened, err := sqlitestore.Open(t.Context(), path)
	if reopened != nil {
		require.NoError(t, reopened.Close())
	}
	require.Error(t, err, "equal version does not make the old execution-authority schema compatible")
	// Rejection must not repair or restamp the owned old database. The fixture
	// remains available to a matching old binary rather than losing history.
	raw, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	var version string
	require.NoError(t, raw.QueryRowContext(t.Context(), "SELECT value FROM meta WHERE key='schema_version'").Scan(&version))
	require.Equal(t, "31", version)
	var count int
	require.NoError(t, raw.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name IN ('cron_run_claims','cron_issue_holders')").Scan(&count))
	require.Equal(t, 2, count)
}

func TestRejectOccurrenceExclusiveCronSchema31(t *testing.T) {
	d, path := openTestDBWithPath(t)
	_, err := d.ExecContext(t.Context(), `CREATE UNIQUE INDEX experimental_occurrence_lock ON cron_runs(project_id,job_uid,occurrence_key)`)
	require.NoError(t, err)
	require.NoError(t, d.Close())
	reopened, err := sqlitestore.Open(t.Context(), path)
	if reopened != nil {
		require.NoError(t, reopened.Close())
	}
	require.Error(t, err, "a current-version schema must not retain occurrence exclusivity")
}
func TestRejectLegacyCronReplacementTarget31(t *testing.T) {
	d, path := openTestDBWithPath(t)
	for _, table := range []string{"cron_runs", "cron_jobs", "cron_workflows"} {
		_, err := d.ExecContext(t.Context(), "DROP TABLE "+table)
		require.NoError(t, err)
	}
	raw, err := os.ReadFile("testdata/legacy_cron31.sql")
	require.NoError(t, err)
	_, err = d.ExecContext(t.Context(), string(raw))
	require.NoError(t, err)
	require.NoError(t, d.Close())
	replacement, err := sqlitestore.OpenReplacementTarget(t.Context(), path)
	if replacement != nil {
		require.NoError(t, replacement.Close())
	}
	require.Error(t, err, "replacement must reject incompatible shape before clearing a target")
}
