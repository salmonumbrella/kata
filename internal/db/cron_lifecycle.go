package db

import (
	"context"
	"database/sql"
)

type cronLifecycleQuery interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

// DeleteCronProject removes the project's cron projections during purge.
func DeleteCronProject(ctx context.Context, q cronLifecycleQuery, projectID int64) error {
	for _, table := range []string{"cron_runs", "cron_jobs", "cron_workflows"} {
		if _, err := q.ExecContext(ctx, "DELETE FROM "+table+" WHERE project_id=$1", projectID); err != nil {
			return err
		}
	}
	return nil
}

// MergeCronProjects moves cron projections in the project merge transaction.
func MergeCronProjects(ctx context.Context, tx *sql.Tx, source, target Project) error {
	for _, table := range []string{"cron_jobs", "cron_workflows", "cron_runs"} {
		//nolint:gosec // Table identifier comes from the fixed three-table list; values are bound.
		if _, err := tx.ExecContext(ctx, "UPDATE "+table+" SET project_id=$1 WHERE project_id=$2", target.ID, source.ID); err != nil {
			return err
		}
	}
	return nil
}

// LockCronProject serializes ordinary project mutations before their
// binding and credential fences, using the same ordering on both backends.
func LockCronProject(ctx context.Context, tx Transaction, projectID int64) error {
	result, err := tx.ExecContext(ctx, `UPDATE projects SET name=name WHERE id=$1`, projectID)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrNotFound
	}
	return nil
}
