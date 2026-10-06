package db

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/teammate"
)

// CronObservationLimit bounds a complete run observation or event in bytes.
const CronObservationLimit = 96 * 1024

func validateCronRun(value CronRun) error {
	if !cronUID(value.UID) || value.ProjectID <= 0 || strings.TrimSpace(value.Actor) == "" || len(value.Actor) > 256 || !utf8.ValidString(value.Actor) {
		return fmt.Errorf("%w: invalid run identity", cron.ErrInvalid)
	}
	for _, pair := range [][2]*string{{value.JobUID, value.DefinitionEventUID}, {value.WorkflowUID, value.WorkflowDefinitionEventUID}} {
		if (pair[0] == nil) != (pair[1] == nil) {
			return fmt.Errorf("%w: definition UID and event UID must be paired", cron.ErrInvalid)
		}
		if pair[0] != nil && (!cronUID(*pair[0]) || !cronUID(*pair[1])) {
			return fmt.Errorf("%w: invalid definition reference", cron.ErrInvalid)
		}
	}
	if value.JobUID == nil && value.WorkflowUID == nil {
		return fmt.Errorf("%w: a job or workflow reference is required", cron.ErrInvalid)
	}
	if value.IssueUID != nil && !cronUID(*value.IssueUID) {
		return fmt.Errorf("%w: invalid issue reference", cron.ErrInvalid)
	}
	if value.OccurrenceKey != nil && (!utf8.ValidString(*value.OccurrenceKey) || utf8.RuneCountInString(*value.OccurrenceKey) < 1 || utf8.RuneCountInString(*value.OccurrenceKey) > 1024) {
		return fmt.Errorf("%w: invalid occurrence key", cron.ErrInvalid)
	}
	for _, label := range []*string{value.Teammate, value.ExecutorLabel} {
		if label != nil && (strings.TrimSpace(*label) == "" || len(*label) > 256 || !utf8.ValidString(*label)) {
			return fmt.Errorf("%w: invalid run label", cron.ErrInvalid)
		}
	}
	if value.Teammate != nil {
		if err := teammate.Validate(*value.Teammate); err != nil {
			return fmt.Errorf("%w: %v", cron.ErrInvalid, err)
		}
	}
	switch value.Status {
	case "running", "succeeded", "failed", "cancelled", "unknown":
	default:
		return fmt.Errorf("%w: unknown run status", cron.ErrInvalid)
	}
	summary, err := json.Marshal(value.Summary)
	if err != nil {
		return err
	}
	if _, err = cron.ParseSummary(summary); err != nil {
		return err
	}
	for _, at := range []*time.Time{value.StartedAt, value.EndedAt} {
		if at != nil && (at.IsZero() || at.Year() < 0 || at.Year() > 9999) {
			return fmt.Errorf("%w: invalid evidence timestamp", cron.ErrInvalid)
		}
	}
	if value.StartedAt != nil && value.EndedAt != nil && value.EndedAt.Before(*value.StartedAt) {
		return fmt.Errorf("%w: end precedes start", cron.ErrInvalid)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(encoded) > CronObservationLimit {
		return fmt.Errorf("%w: run observation exceeds byte limit", cron.ErrInvalid)
	}
	return nil
}

// Run reads an independent observation through its project-scoped UID.
func (a CronSQL) Run(ctx context.Context, projectID int64, id string) (CronRun, error) {
	value, err := scanCronRunRow(a.Query.QueryRowContext(ctx, "SELECT "+cronRunColumns+" FROM cron_runs WHERE project_id=$1 AND uid=$2", projectID, strings.ToUpper(id)))
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return CronRun(value), err
}

// All run writers normalize instants to UTC, but RFC3339Nano fractions have
// variable widths. Pad the comparison key without changing stored precision.
// Both backends implement these text functions without timestamp rounding.
const cronRunCreatedAtKey = `substr(created_at,1,19) || '.' || substr(CASE WHEN substr(created_at,20,1)='.' THEN rtrim(substr(created_at,21),'Z') ELSE '' END || '000000000',1,9)`

// Runs reads a bounded creation-time and UID-ordered project history page.
func (a CronSQL) Runs(ctx context.Context, in CronRunList) ([]CronRun, error) {
	if in.Limit < 0 {
		return nil, fmt.Errorf("%w: negative limit", cron.ErrInvalid)
	}
	if in.Limit == 0 || in.Limit > 100 {
		in.Limit = 100
	}
	query := "SELECT " + cronRunColumns + " FROM cron_runs WHERE project_id=$1"
	args := []any{in.ProjectID}
	if in.JobUID != "" {
		if !cronUID(in.JobUID) {
			return nil, fmt.Errorf("%w: invalid job UID", cron.ErrInvalid)
		}
		args = append(args, in.JobUID)
		query += fmt.Sprintf(" AND job_uid=$%d", len(args))
	}
	if in.BeforeUID != "" {
		if !cronUID(in.BeforeUID) {
			return nil, fmt.Errorf("%w: invalid cursor", cron.ErrInvalid)
		}
		before, err := a.Run(ctx, in.ProjectID, in.BeforeUID)
		if err != nil {
			return nil, err
		}
		args = append(args, before.CreatedAt.UTC().Format("2006-01-02T15:04:05.000000000"), before.UID)
		query += fmt.Sprintf(" AND ((%s)<$%d OR ((%s)=$%d AND uid<$%d))", cronRunCreatedAtKey, len(args)-1, cronRunCreatedAtKey, len(args)-1, len(args))
	}
	args = append(args, in.Limit)
	query += fmt.Sprintf(" ORDER BY (%s) DESC,uid DESC LIMIT $%d", cronRunCreatedAtKey, len(args))
	rows, err := a.Query.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := []CronRun{}
	for rows.Next() {
		value, err := scanCronRunRow(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, CronRun(value))
	}
	return result, rows.Err()
}

// ObserveRun commits ordinary attributed evidence with identity and revision checks.
func (a CronSQL) ObserveRun(ctx context.Context, in ObserveCronRun) (CronRunObservationResult, error) {
	result := CronRunObservationResult{Events: []Event{}}
	value := CronRun{UID: in.UID, ProjectID: in.ProjectID, JobUID: in.JobUID, DefinitionEventUID: in.DefinitionEventUID, WorkflowUID: in.WorkflowUID, WorkflowDefinitionEventUID: in.WorkflowDefinitionEventUID, OccurrenceKey: in.OccurrenceKey, IssueUID: in.IssueUID, Actor: in.Actor, Teammate: in.Teammate, ExecutorLabel: in.ExecutorLabel, Status: in.Status, Summary: in.Summary, StartedAt: in.StartedAt, EndedAt: in.EndedAt}
	if err := validateCronRun(value); err != nil {
		return result, err
	}
	if in.ExpectedRevision < 0 {
		return result, fmt.Errorf("%w: negative expected revision", cron.ErrInvalid)
	}
	err := a.transactProject(ctx, in.ProjectID, func(tx *sql.Tx) error {
		result = CronRunObservationResult{Events: []Event{}}
		if err := LockCronProject(ctx, tx, in.ProjectID); err != nil {
			return err
		}
		var project Project
		project.ID = in.ProjectID
		if err := tx.QueryRowContext(ctx, `SELECT uid,name FROM projects WHERE id=$1 AND deleted_at IS NULL AND name<>$2`, in.ProjectID, SystemProjectName).Scan(&project.UID, &project.Name); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if err := a.WriteGate(ctx, tx, in.ProjectID); err != nil {
			return err
		}
		prior, err := scanCronRunRow(tx.QueryRowContext(ctx, "SELECT "+cronRunColumns+" FROM cron_runs WHERE uid=$1", in.UID))
		fresh := errors.Is(err, sql.ErrNoRows)
		if err != nil && !fresh {
			return err
		}
		now := time.Now().UTC().Truncate(time.Millisecond)
		value.CreatedAt = now
		value.UpdatedAt = now
		value.Revision = 1
		if !fresh {
			current := CronRun(prior)
			value.ID = current.ID
			value.CreatedAt = current.CreatedAt
			if current.ProjectID != in.ProjectID || !sameCronRunIdentity(current, value) {
				return ErrCronConflict
			}
			if sameCronRunEvidence(current, value) {
				result.Run = current
				result.Replayed = true
				return nil
			}
			if in.ExpectedRevision != current.Revision {
				return ErrCronConflict
			}
			value.Revision = current.Revision + 1
		} else {
			if in.ExpectedRevision != 0 {
				return ErrCronConflict
			}
			validator := NewCronReplayValidator(tx, a.Postgres)
			refs, err := cronRunReferences(project.ID, value)
			if err != nil {
				return classifyCronReferenceError(err, cron.ErrInvalid)
			}
			if err := validator.prepareReferences(ctx, refs); err != nil {
				return classifyCronReferenceError(err, cron.ErrInvalid)
			}
			for _, ref := range []struct {
				kind      string
				id, event *string
			}{{"job", in.JobUID, in.DefinitionEventUID}, {"workflow", in.WorkflowUID, in.WorkflowDefinitionEventUID}} {
				if ref.id != nil {
					if err := validator.validateLocalReference(ctx, project, ref.kind, *ref.id, *ref.event); err != nil {
						return err
					}
				}
			}
			if in.IssueUID != nil {
				var owner int64
				err := tx.QueryRowContext(ctx, `SELECT project_id FROM issues WHERE uid=$1`, *in.IssueUID).Scan(&owner)
				if errors.Is(err, sql.ErrNoRows) || (err == nil && owner != in.ProjectID) {
					return fmt.Errorf("%w: issue reference does not belong to project", cron.ErrInvalid)
				}
				if err != nil {
					return err
				}
			}
		}
		payload, err := json.Marshal(NewCronRunObservation(value, project.UID))
		if err != nil {
			return err
		}
		if len(payload) > CronObservationLimit {
			return fmt.Errorf("%w: observation exceeds byte limit", cron.ErrInvalid)
		}
		event, err := a.InsertEvent(ctx, tx, CronEvent{ProjectID: project.ID, ProjectUID: project.UID, ProjectName: project.Name, Type: "cron.run.observed", Actor: in.Actor, Payload: string(payload)})
		if err != nil {
			return err
		}
		if fresh {
			if err := insertCronRun(ctx, tx, value, false, false); err != nil {
				return err
			}
		} else {
			summary, err := json.Marshal(value.Summary)
			if err != nil {
				return err
			}
			changed, err := tx.ExecContext(ctx, `UPDATE cron_runs SET status=$1,summary_json=$2,started_at=$3,ended_at=$4,updated_at=$5,revision=$6 WHERE uid=$7 AND project_id=$8 AND revision=$9`, value.Status, string(summary), cronTimeValue(value.StartedAt), cronTimeValue(value.EndedAt), now.Format(time.RFC3339Nano), value.Revision, value.UID, value.ProjectID, in.ExpectedRevision)
			if err != nil {
				return err
			}
			count, err := changed.RowsAffected()
			if err != nil {
				return err
			}
			if count != 1 {
				return ErrCronConflict
			}
		}
		saved, err := scanCronRunRow(tx.QueryRowContext(ctx, "SELECT "+cronRunColumns+" FROM cron_runs WHERE uid=$1", in.UID))
		if err != nil {
			return err
		}
		result.Run = CronRun(saved)
		result.Events = []Event{event}
		return nil
	})
	if err != nil {
		return CronRunObservationResult{}, err
	}
	return result, nil
}
func sameCronRunEvidence(a, b CronRun) bool {
	return a.Status == b.Status && reflect.DeepEqual(a.Summary, b.Summary) && sameOptionalTime(a.StartedAt, b.StartedAt) && sameOptionalTime(a.EndedAt, b.EndedAt)
}
func sameOptionalTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

// References point to a historical definition event, not its current revision
// or enabled state. Baselines can preserve that original UID without its edit.
func validateRunDefinitionReference(ctx context.Context, tx *sql.Tx, project Project, kind, id, event string) error {
	return NewCronReplayValidator(tx).validateLocalReference(ctx, project, kind, id, event)
}
func (v *CronReplayValidator) validateLocalReference(ctx context.Context, project Project, kind, id, event string) error {
	ref := cronDefinitionReference{projectID: project.ID, kind: kind, uid: id, event: event}
	if err := v.load(ctx, ref); err != nil {
		return classifyCronReferenceError(err, cron.ErrInvalid)
	}
	if actual, known := v.idx.events[event]; known && actual == ref {
		return nil
	}
	return fmt.Errorf("%w: definition event reference does not belong to project", cron.ErrInvalid)
}
