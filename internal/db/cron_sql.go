package db

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/uid"
)

// CronSQL shares the portable definition transaction across backends.
// Each backend supplies its own retry/isolation, authorization fence, and event
// writer. SQL uses numbered placeholders supported by SQLite and PostgreSQL.
type CronSQL struct {
	InstanceUID string
	Postgres    bool
	Query       interface {
		QueryContext(context.Context, string, ...any) (*sql.Rows, error)
		QueryRowContext(context.Context, string, ...any) *sql.Row
	}
	Transact    func(context.Context, func(*sql.Tx) error) error
	WriteGate   func(context.Context, *sql.Tx, int64) error
	InsertEvent func(context.Context, *sql.Tx, CronEvent) (Event, error)
}

// CronEvent carries the attributed event returned by a definition transaction.
type CronEvent struct {
	IssueID                                       *int64
	IssueUID                                      *string
	ProjectID                                     int64
	ProjectUID, ProjectName, Type, Actor, Payload string
}

const cronDefinitionColumns = `id,uid,project_id,name,definition_json,definition_event_uid,definition_hlc_json,author,revision,created_at,updated_at,deleted_at`

type cronRow struct {
	CronDefinition
	document []byte
}

func scanCron(row interface{ Scan(...any) error }, _ bool) (cronRow, error) {
	var result cronRow
	var definition, clock, created, updated string
	var deleted sql.NullString
	fields := []any{&result.ID, &result.UID, &result.ProjectID, &result.Name, &definition, &result.DefinitionEventUID, &clock, &result.Author, &result.Revision, &created, &updated, &deleted}
	if err := row.Scan(fields...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return result, ErrNotFound
		}
		return result, err
	}
	if err := json.Unmarshal([]byte(clock), &result.DefinitionHLC); err != nil {
		return result, err
	}
	var err error
	result.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
	if err != nil {
		return result, err
	}
	result.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated)
	if err != nil {
		return result, err
	}
	if deleted.Valid {
		value, err := time.Parse(time.RFC3339Nano, deleted.String)
		if err != nil {
			return result, err
		}
		result.DeletedAt = &value
	}
	result.document = []byte(definition)
	return result, nil
}

func cronTable(job bool) string {
	if job {
		return "cron_jobs"
	}
	return "cron_flows"
}
func cronColumns(_ bool) string { return cronDefinitionColumns }

func jobFromRow(row cronRow) (CronJob, error) {
	result := CronJob{CronDefinition: row.CronDefinition}
	var err error
	result.Definition, err = cron.ParseJob(row.document)
	if err != nil {
		return result, err
	}
	return result, err
}
func flowFromRow(row cronRow) (CronFlow, error) {
	definition, err := cron.ParseFlow(row.document)
	return CronFlow{CronDefinition: row.CronDefinition, Definition: definition}, err
}

// Job reads one project-scoped definition, including a retained tombstone.
func (a CronSQL) Job(ctx context.Context, project int64, id string) (CronJob, error) {
	row, err := scanCron(a.Query.QueryRowContext(ctx, "SELECT "+cronColumns(true)+" FROM cron_jobs WHERE project_id=$1 AND uid=$2", project, strings.ToUpper(id)), true)
	if err != nil {
		return CronJob{}, err
	}
	return jobFromRow(row)
}

// Flow reads one project-scoped definition, including a retained tombstone.
func (a CronSQL) Flow(ctx context.Context, project int64, id string) (CronFlow, error) {
	row, err := scanCron(a.Query.QueryRowContext(ctx, "SELECT "+cronColumns(false)+" FROM cron_flows WHERE project_id=$1 AND uid=$2", project, strings.ToUpper(id)), false)
	if err != nil {
		return CronFlow{}, err
	}
	return flowFromRow(row)
}

func (a CronSQL) list(ctx context.Context, in CronList, job bool) ([]cronRow, error) {
	query := "SELECT " + cronColumns(job) + " FROM " + cronTable(job) + " WHERE project_id=$1"
	if !in.IncludeDeleted {
		query += " AND deleted_at IS NULL"
	}
	query += " ORDER BY name,uid"
	rows, err := a.Query.QueryContext(ctx, query, in.ProjectID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := []cronRow{}
	for rows.Next() {
		row, err := scanCron(rows, job)
		if err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

// Jobs lists project-scoped job documents under the requested tombstone policy.
func (a CronSQL) Jobs(ctx context.Context, in CronList) ([]CronJob, error) {
	rows, err := a.list(ctx, in, true)
	if err != nil {
		return nil, err
	}
	result := []CronJob{}
	for _, row := range rows {
		value, err := jobFromRow(row)
		if err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, nil
}

// Flows lists project-scoped flow documents under the requested tombstone policy.
func (a CronSQL) Flows(ctx context.Context, in CronList) ([]CronFlow, error) {
	rows, err := a.list(ctx, in, false)
	if err != nil {
		return nil, err
	}
	result := []CronFlow{}
	for _, row := range rows {
		value, err := flowFromRow(row)
		if err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, nil
}

// PutJob validates and replaces a job using the expected whole-document revision.
func (a CronSQL) PutJob(ctx context.Context, in PutCronJob) (CronJob, []Event, error) {
	if err := in.Definition.Validate(); err != nil {
		return CronJob{}, nil, err
	}
	row, event, err := a.put(ctx, true, in.UID, in.ProjectID, in.Name, in.ExpectedEventUID, in.Actor, in.Deleted, in.Definition)
	if err != nil {
		return CronJob{}, nil, err
	}
	value, err := jobFromRow(row)
	return value, event, err
}

// PutFlow validates and replaces a flow using the expected whole-document revision.
func (a CronSQL) PutFlow(ctx context.Context, in PutCronFlow) (CronFlow, Event, error) {
	if err := in.Definition.Validate(); err != nil {
		return CronFlow{}, Event{}, err
	}
	row, events, err := a.put(ctx, false, in.UID, in.ProjectID, in.Name, in.ExpectedEventUID, in.Actor, in.Deleted, in.Definition)
	if err != nil {
		return CronFlow{}, Event{}, err
	}
	value, err := flowFromRow(row)
	if err != nil {
		return CronFlow{}, Event{}, err
	}
	if len(events) != 1 {
		return CronFlow{}, Event{}, fmt.Errorf("definition write produced %d events", len(events))
	}
	return value, events[0], nil
}

func (a CronSQL) put(ctx context.Context, job bool, id string, projectID int64, name, expected, actor string, deleted bool, definition any) (cronRow, []Event, error) {
	var result cronRow
	var event Event
	var events []Event
	id = strings.ToUpper(id)
	name = strings.TrimSpace(name)
	actor = strings.TrimSpace(actor)
	if name == "" || len(name) > 256 || actor == "" {
		return result, events, fmt.Errorf("%w: name and actor required", cron.ErrInvalid)
	}
	if id != "" && !uid.Valid(id) {
		return result, events, fmt.Errorf("%w: invalid UID", cron.ErrInvalid)
	}
	document, err := json.Marshal(definition)
	if err != nil {
		return result, events, err
	}
	if id == "" {
		id, err = uid.New()
		if err != nil {
			return result, events, err
		}
	}
	err = a.transactProject(ctx, projectID, func(tx *sql.Tx) error {
		events = []Event{}
		if err := LockCronProject(ctx, tx, projectID); err != nil {
			return err
		}
		var projectUID, projectName string
		if err := tx.QueryRowContext(ctx, `SELECT uid,name FROM projects WHERE id=$1 AND deleted_at IS NULL AND name<>$2`, projectID, SystemProjectName).Scan(&projectUID, &projectName); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if err := a.WriteGate(ctx, tx, projectID); err != nil {
			return err
		}
		current, readErr := scanCron(tx.QueryRowContext(ctx, "SELECT "+cronColumns(job)+" FROM "+cronTable(job)+" WHERE uid=$1", id), job)
		fresh := errors.Is(readErr, ErrNotFound)
		if readErr != nil && !fresh {
			return readErr
		}
		if fresh && (expected != "" || deleted) {
			return ErrCronConflict
		}
		if !fresh && (current.ProjectID != projectID || expected == "" || expected != current.DefinitionEventUID) {
			return ErrCronConflict
		}
		if job && !deleted {
			if err := CheckCronDependencies(ctx, tx, projectID, definition.(cron.JobDefinition)); err != nil {
				return err
			}
		}
		kind := "flow"
		if job {
			kind = "job"
		}
		operation := "updated"
		if fresh {
			operation = "created"
		} else if deleted {
			operation = "deleted"
		} else if current.DeletedAt != nil {
			operation = "restored"
		}
		now := time.Now().UTC().Truncate(time.Millisecond)
		created := now
		author := actor
		revision := int64(1)
		if !fresh {
			created = current.CreatedAt
			author = current.Author
			revision = current.Revision + 1
		}
		var tombstone *time.Time
		if deleted {
			tombstone = &now
		}
		payload, err := json.Marshal(struct {
			UID        string     `json:"uid"`
			ProjectUID string     `json:"project_uid"`
			Name       string     `json:"name"`
			Definition any        `json:"definition"`
			Author     string     `json:"author"`
			CreatedAt  time.Time  `json:"created_at"`
			UpdatedAt  time.Time  `json:"updated_at"`
			DeletedAt  *time.Time `json:"deleted_at,omitempty"`
		}{id, projectUID, name, definition, author, created, now, tombstone})
		if err != nil {
			return err
		}
		event, err = a.InsertEvent(ctx, tx, CronEvent{ProjectID: projectID, ProjectUID: projectUID, ProjectName: projectName, Type: "cron." + kind + "." + operation, Actor: actor, Payload: string(payload)})
		if err != nil {
			return err
		}
		events = append(events, event)
		clock, err := json.Marshal(CronDefinitionHLC{Version: 1, PhysicalMS: event.HLCPhysicalMS, Counter: event.HLCCounter, OriginInstanceUID: event.OriginInstanceUID})
		if err != nil {
			return err
		}
		var deletedValue any
		if tombstone != nil {
			deletedValue = now.Format(time.RFC3339Nano)
		}
		if fresh {
			//nolint:gosec // Boolean selector returns one of two fixed table names; values are bound.
			_, err = tx.ExecContext(ctx, "INSERT INTO "+cronTable(job)+`(uid,project_id,name,definition_json,definition_event_uid,definition_hlc_json,author,revision,created_at,updated_at,deleted_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, id, projectID, name, string(document), event.UID, string(clock), author, revision, created.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), deletedValue)
		} else {
			//nolint:gosec // Boolean selector returns one of two fixed tables; values are bound.
			changed, writeErr := tx.ExecContext(ctx, "UPDATE "+cronTable(job)+` SET name=$1,definition_json=$2,definition_event_uid=$3,definition_hlc_json=$4,revision=$5,updated_at=$6,deleted_at=$7 WHERE uid=$8 AND project_id=$9 AND definition_event_uid=$10`, name, string(document), event.UID, string(clock), revision, now.Format(time.RFC3339Nano), deletedValue, id, projectID, expected)
			err = writeErr
			if err == nil {
				count, e := changed.RowsAffected()
				if e != nil {
					return e
				}
				if count != 1 {
					return ErrCronConflict
				}
			}
		}
		if err != nil {
			return err
		}
		result, err = scanCron(tx.QueryRowContext(ctx, "SELECT "+cronColumns(job)+" FROM "+cronTable(job)+" WHERE uid=$1", id), job)
		return err
	})
	if err != nil {
		return cronRow{}, nil, err
	}
	return result, events, nil
}
