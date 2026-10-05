package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json/v2"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db/dbtest/faultsql"
)

func TestCronReferenceOperationalErrorChains(t *testing.T) {
	job, version, projectUID := "01M40000000000000000000001", "01M40000000000000000000002", "01M40000000000000000000003"
	at := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	run := CronRun{UID: "01M40000000000000000000004", ProjectID: 1, JobUID: &job, DefinitionEventUID: &version, Actor: "worker", Status: "running", Summary: cron.Summary{Version: 1}, Revision: 1, CreatedAt: at, UpdatedAt: at}
	body, err := json.Marshal(NewCronRunObservation(run, projectUID))
	require.NoError(t, err)
	event := RemoteEvent{Type: "cron.run.observed", EventUID: "01M40000000000000000000005", ProjectUID: projectUID, OriginInstanceUID: "01M40000000000000000000006", HLCPhysicalMS: at.UnixMilli(), Payload: body}
	_, err = parseCronRunObservation(FoldEvent{Type: event.Type, UID: event.EventUID, ProjectUID: event.ProjectUID, OriginInstanceUID: event.OriginInstanceUID, HLCPhysicalMS: event.HLCPhysicalMS, Payload: event.Payload})
	require.NoError(t, err, "faults must exercise a valid observation")
	callers := []struct {
		name    string
		call    func(*sql.Tx) error
		queries int64
	}{
		{"replay", func(tx *sql.Tx) error { return ValidateCronRunReplay(t.Context(), tx, 1, event) }, 7},
		{"materialize", func(tx *sql.Tx) error {
			return materializeCronRuns(t.Context(), tx, 1, projectUID, FoldProjection{CronRuns: map[string]FoldCronRun{run.UID: {CronRun: run, ProjectUID: projectUID}}})
		}, 6},
		{"local", func(tx *sql.Tx) error {
			return validateRunDefinitionReference(t.Context(), tx, Project{ID: 1, UID: projectUID}, "job", job, version)
		}, 5},
	}
	for _, caller := range callers {
		t.Run(caller.name, func(t *testing.T) {
			for _, fault := range []error{&pgconn.PgError{Code: "40001"}, context.Canceled, context.DeadlineExceeded, errors.New("read unavailable")} {
				for query := int64(1); query <= caller.queries; query++ {
					script := &faultsql.Script{Query: func(count int64) (driver.Rows, error) {
						if count == query {
							return nil, fault
						}
						return &faultsql.Rows{}, nil
					}}
					pool := faultsql.Open(t, script)
					tx, err := pool.BeginTx(t.Context(), nil)
					require.NoError(t, err)
					err = caller.call(tx)
					require.ErrorIs(t, err, fault)
					require.False(t, errors.Is(err, ErrFederationIngestValidation))
					require.False(t, errors.Is(err, cron.ErrInvalid))
					require.NoError(t, tx.Rollback())
				}
			}
			for _, scan := range []bool{false, true} {
				script := &faultsql.Script{Query: func(int64) (driver.Rows, error) {
					rows := &faultsql.Rows{Names: []string{"project", "event"}, Err: context.Canceled}
					if scan {
						rows.Err = nil
						rows.Values = [][]driver.Value{{"invalid integer", version}}
					}
					return rows, nil
				}}
				pool := faultsql.Open(t, script)
				tx, err := pool.BeginTx(t.Context(), nil)
				require.NoError(t, err)
				err = caller.call(tx)
				if scan {
					require.ErrorContains(t, err, "invalid syntax")
				} else {
					require.ErrorIs(t, err, context.Canceled)
				}
				require.False(t, errors.Is(err, ErrFederationIngestValidation))
				require.False(t, errors.Is(err, cron.ErrInvalid))
				require.NoError(t, tx.Rollback())
			}
		})
	}
}
