package pgstore

import (
	"context"
	"database/sql"

	"go.kenn.io/kata/internal/db"
)

func (s *Store) cronSQL() db.CronSQL {
	return db.CronSQL{Postgres: true, InstanceUID: s.InstanceUID(), Query: s, Transact: func(ctx context.Context, run func(*sql.Tx) error) error { return s.withSerializableTx(ctx, run) }, WriteGate: func(ctx context.Context, tx *sql.Tx, id int64) error { return ensureProjectWritableTx(ctx, tx, id) }, InsertEvent: func(ctx context.Context, tx *sql.Tx, event db.CronEvent) (db.Event, error) {
		return s.insertEventTx(ctx, tx, eventInsert{ProjectID: event.ProjectID, ProjectUID: event.ProjectUID, ProjectName: event.ProjectName, IssueID: event.IssueID, IssueUID: event.IssueUID, Type: event.Type, Actor: event.Actor, Payload: event.Payload})
	}}
}

// PutCronJob validates and writes an attributed job definition.
func (s *Store) PutCronJob(ctx context.Context, in db.PutCronJob) (db.CronJob, []db.Event, error) {
	return s.cronSQL().PutJob(ctx, in)
}

// PutCronFlow validates and writes an attributed flow definition.
func (s *Store) PutCronFlow(ctx context.Context, in db.PutCronFlow) (db.CronFlow, db.Event, error) {
	return s.cronSQL().PutFlow(ctx, in)
}

// CronJob reads one project-scoped job definition.
func (s *Store) CronJob(ctx context.Context, project int64, id string) (db.CronJob, error) {
	return s.cronSQL().Job(ctx, project, id)
}

// CronFlow reads one project-scoped flow definition.
func (s *Store) CronFlow(ctx context.Context, project int64, id string) (db.CronFlow, error) {
	return s.cronSQL().Flow(ctx, project, id)
}

// ListCronJobs lists job definitions under the requested project filter.
func (s *Store) ListCronJobs(ctx context.Context, in db.CronList) ([]db.CronJob, error) {
	return s.cronSQL().Jobs(ctx, in)
}

// ListCronFlows lists flow definitions under the requested project filter.
func (s *Store) ListCronFlows(ctx context.Context, in db.CronList) ([]db.CronFlow, error) {
	return s.cronSQL().Flows(ctx, in)
}
