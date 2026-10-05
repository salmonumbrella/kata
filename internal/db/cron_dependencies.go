package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"go.kenn.io/kata/internal/cron"
)

// CheckCronDependencies checks the live dependencies of a new job/run.
// Definition writers pass their transaction so referenced documents are checked
// consistently with the write. Frozen resumes use their
// stored snapshot and must not reload an edited flow through this helper.
func CheckCronDependencies(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, projectID int64, definition cron.JobDefinition) error {
	check := func(table, kind, id string) error {
		if id == "" {
			return nil
		}
		var owner int64
		var deleted sql.NullString
		err := q.QueryRowContext(ctx, "SELECT project_id,deleted_at FROM "+table+" WHERE uid=$1", id).Scan(&owner, &deleted)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: missing %s dependency %s", cron.ErrInvalid, kind, id)
		}
		if err != nil {
			return fmt.Errorf("read %s dependency: %w", kind, err)
		}
		if owner != projectID {
			return fmt.Errorf("%w: %s dependency belongs to another project", cron.ErrInvalid, kind)
		}
		if deleted.Valid {
			return fmt.Errorf("%w: tombstoned %s dependency %s", cron.ErrInvalid, kind, id)
		}
		return nil
	}
	if err := check("cron_flows", "flow", definition.Action.FlowUID); err != nil {
		return err
	}
	issueUID := definition.Trigger.IssueUID
	if definition.Issue != nil && definition.Issue.Kind == "existing" {
		if issueUID != "" && issueUID != definition.Issue.UID && definition.Action.Kind != "notify" {
			return fmt.Errorf("%w: issue target differs from date source", cron.ErrInvalid)
		}
		if err := check("issues", "source issue", issueUID); err != nil {
			return err
		}
		issueUID = definition.Issue.UID
	}
	return check("issues", "issue", issueUID)
}
