package dbtest

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// A finalized series copy is audit state, never an active scheduler or template.
func RunMovedRecurrenceAdmission(t *testing.T, store db.Storage) {
	for _, operation := range []string{"id", "uid", "list", "scheduler", "patch", "delete", "browser"} {
		t.Run(operation, func(t *testing.T) {
			ctx := t.Context()
			source, err := store.CreateProject(ctx, "series-admission-"+operation)
			require.NoError(t, err)
			target, err := store.CreateProject(ctx, "series-target-"+operation)
			require.NoError(t, err)
			create := db.CreateRecurrenceIn{ProjectID: source.ID, Actor: "tester", Rule: "FREQ=DAILY", DTStart: "2026-10-12", Timezone: "UTC", Template: db.RecurrenceTemplate{Title: "Retained series"}}
			ghost, event, err := store.CreateRecurrence(ctx, create)
			require.NoError(t, err)
			create.Template.Title = "Active series"
			active, _, err := store.CreateRecurrence(ctx, create)
			require.NoError(t, err)
			q := store.(interface {
				ExecContext(context.Context, string, ...any) (sql.Result, error)
				QueryRowContext(context.Context, string, ...any) *sql.Row
			})
			_, err = q.ExecContext(ctx, `UPDATE recurrences SET location_generation=2,moved_to_authority_uid=$1,moved_to_project_uid=$2,moved_event_uid=$3 WHERE id=$4`, "01AAAAAAAAAAAAAAAAAAAAAAAA", target.UID, event.UID, ghost.ID)
			require.NoError(t, err)
			switch operation {
			case "id":
				_, err = store.GetRecurrenceByID(ctx, ghost.ID)
				require.ErrorIs(t, err, db.ErrNotFound)
			case "uid":
				_, err = store.GetRecurrenceByUID(ctx, ghost.UID)
				require.ErrorIs(t, err, db.ErrNotFound)
			case "list":
				rows, err := store.ListRecurrencesByProject(ctx, source.ID)
				require.NoError(t, err)
				require.Len(t, rows, 1)
				require.Equal(t, active.UID, rows[0].UID)
			case "scheduler":
				out, err := store.MaterializeNext(ctx, ghost.ID, "2026-10-11", "scheduler")
				require.Error(t, err)
				require.Zero(t, out.NewIssueID)
			case "patch":
				out, err := store.PatchRecurrence(ctx, db.PatchRecurrenceIn{RecurrenceID: ghost.ID, IfMatchRev: ghost.Revision, Actor: "tester", Update: db.RecurrenceUpdate{TemplateTitle: new("Wrong source edit")}})
				require.Error(t, err)
				require.False(t, out.Changed)
			case "delete":
				_, err = store.SoftDeleteRecurrence(ctx, db.SoftDeleteRecurrenceIn{RecurrenceID: ghost.ID, IfMatchRev: ghost.Revision, Actor: "tester"})
				require.Error(t, err)
			case "browser":
				data, err := store.(db.UIStore).ReadUISnapshot(ctx, db.UISnapshotQuery{View: "all-open", ProjectUID: source.UID})
				require.NoError(t, err)
				require.Len(t, data.Recurrences, 1)
				require.Equal(t, active.UID, data.Recurrences[0].UID)
			}
			var title string
			var revision, count int64
			require.NoError(t, q.QueryRowContext(ctx, `SELECT template_title,revision FROM recurrences WHERE id=$1`, ghost.ID).Scan(&title, &revision))
			require.Equal(t, ghost.TemplateTitle, title)
			require.Equal(t, ghost.Revision, revision)
			require.NoError(t, q.QueryRowContext(ctx, `SELECT COUNT(*) FROM issues WHERE recurrence_id=$1`, ghost.ID).Scan(&count))
			require.Zero(t, count)
		})
	}
}
