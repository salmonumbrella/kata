package sqlitestore

import (
	"context"
	"database/sql"

	"go.kenn.io/kata/internal/db"
)

func (d *Store) cronSQL() db.CronSQL {
	return db.CronSQL{InstanceUID: d.InstanceUID(), Query: d, Transact: func(ctx context.Context, run func(*sql.Tx) error) error {
		return d.RetryTransient(ctx, func() error {
			tx, err := d.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			if err := run(tx); err != nil {
				return err
			}
			return tx.Commit()
		})
	}, WriteGate: func(ctx context.Context, tx *sql.Tx, id int64) error { return ensureProjectWritableTx(ctx, tx, id) }, InsertEvent: func(ctx context.Context, tx *sql.Tx, event db.CronEvent) (db.Event, error) {
		return d.insertEventTx(ctx, tx, eventInsert{ProjectID: event.ProjectID, ProjectUID: event.ProjectUID, ProjectName: event.ProjectName, IssueID: event.IssueID, IssueUID: event.IssueUID, Type: event.Type, Actor: event.Actor, Payload: event.Payload})
	}}
}

// PutCronJob validates and writes an attributed job definition.
func (d *Store) PutCronJob(ctx context.Context, in db.PutCronJob) (db.CronJob, []db.Event, error) {
	return d.cronSQL().PutJob(ctx, in)
}

// PutCronFlow validates and writes an attributed flow definition.
func (d *Store) PutCronFlow(ctx context.Context, in db.PutCronFlow) (db.CronFlow, db.Event, error) {
	return d.cronSQL().PutFlow(ctx, in)
}

// CronJob reads one project-scoped job definition.
func (d *Store) CronJob(ctx context.Context, project int64, id string) (db.CronJob, error) {
	return d.cronSQL().Job(ctx, project, id)
}

// CronFlow reads one project-scoped flow definition.
func (d *Store) CronFlow(ctx context.Context, project int64, id string) (db.CronFlow, error) {
	return d.cronSQL().Flow(ctx, project, id)
}

// ListCronJobs lists job definitions under the requested project filter.
func (d *Store) ListCronJobs(ctx context.Context, in db.CronList) ([]db.CronJob, error) {
	return d.cronSQL().Jobs(ctx, in)
}

// ListCronFlows lists flow definitions under the requested project filter.
func (d *Store) ListCronFlows(ctx context.Context, in db.CronList) ([]db.CronFlow, error) {
	return d.cronSQL().Flows(ctx, in)
}
