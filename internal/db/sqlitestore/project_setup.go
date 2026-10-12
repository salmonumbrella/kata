package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"go.kenn.io/kata/internal/db"
)

// SetupProject atomically resolves a stable project UID and attaches a real
// workspace alias, returning only the event committed by this invocation.
func (d *Store) SetupProject(ctx context.Context, p db.SetupProjectParams) (db.SetupProjectResult, error) {
	if err := db.ValidateProjectSetup(p); err != nil {
		return db.SetupProjectResult{}, err
	}
	return retryWrite1(ctx, d, func() (db.SetupProjectResult, error) {
		tx, err := d.BeginTx(ctx, nil)
		if err != nil {
			return db.SetupProjectResult{}, err
		}
		defer func() { _ = tx.Rollback() }()
		out, err := d.setupProjectTx(ctx, tx, p)
		if err != nil {
			return db.SetupProjectResult{}, err
		}
		if err := tx.Commit(); err != nil {
			return db.SetupProjectResult{}, err
		}
		return out, nil
	})
}
func (d *Store) setupProjectTx(ctx context.Context, tx *sql.Tx, p db.SetupProjectParams) (db.SetupProjectResult, error) {
	return db.SetupProjectSQL(ctx, tx, p, db.ProjectSetupSQLAdapter{
		LoadProject: func(ctx context.Context, tx *sql.Tx, uid string) (db.Project, error) {
			return hideSystemProject(scanProject(tx.QueryRowContext(ctx, projectSelect+` WHERE uid=$1`, uid)))
		},
		LoadAlias: func(ctx context.Context, tx *sql.Tx, identity string) (db.ProjectAlias, error) {
			return scanAlias(tx.QueryRowContext(ctx, aliasSelect+` WHERE alias_identity=$1 AND alias_kind IN ('git','local')`, identity))
		},
		InsertEvent: func(ctx context.Context, tx *sql.Tx, project db.Project, actor string) (db.Event, error) {
			payload, err := json.Marshal(struct {
				Name string `json:"name"`
			}{Name: project.Name})
			if err != nil {
				return db.Event{}, err
			}
			event, err := d.insertEventTx(ctx, tx, eventInsert{ProjectID: project.ID, ProjectUID: project.UID, ProjectName: project.Name, Type: "project.created", Actor: actor, Payload: string(payload)})
			if err != nil {
				return db.Event{}, err
			}
			if err := bumpProjectAccess(ctx, tx); err != nil {
				return db.Event{}, err
			}
			return event, nil
		},
	})
}
