package dbtest

import (
	"context"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
)

func checkNativeCronDefinitions(t *testing.T, store db.Storage) error {
	ctx := context.Background()
	project, err := store.CreateProject(ctx, "example-cron")
	require.NoError(t, err)
	other, err := store.CreateProject(ctx, "other-cron")
	require.NoError(t, err)
	flowDef := cron.FlowDefinition{Version: 1, Steps: []cron.FlowStep{{Key: "inspect", Kind: "command", Command: "git status"}}}
	flow, event, err := store.PutCronFlow(ctx, db.PutCronFlow{ProjectID: project.ID, Name: "Review", Definition: flowDef, Actor: "worker"})
	require.NoError(t, err)
	require.Equal(t, event.UID, flow.DefinitionEventUID)
	require.Equal(t, "cron.flow.created", event.Type)
	require.Equal(t, int64(1), flow.Revision)
	flowRead, err := store.CronFlow(ctx, project.ID, flow.UID)
	require.NoError(t, err)
	require.Equal(t, flow, flowRead)
	flows, err := store.ListCronFlows(ctx, db.CronList{ProjectID: project.ID})
	require.NoError(t, err)
	require.Equal(t, []db.CronFlow{flow}, flows)
	_, err = store.CronFlow(ctx, other.ID, flow.UID)
	require.ErrorIs(t, err, db.ErrNotFound)

	jobDef, err := cron.ParseJob([]byte(`{"version":1,"kind":"job","trigger":{"kind":"manual"},"action":{"kind":"execute","prompt":"Review"},"issue":{"kind":"per-run","title":"Review"},"overlap":"forbid","catchup":"skip"}`))
	require.NoError(t, err)
	jobDef.Action.Prompt = ""
	jobDef.Action.FlowUID = flow.UID
	t.Run("dependency diagnostics", func(t *testing.T) {
		missing := jobDef
		missing.Action.FlowUID = "01ARZ3NDEKTSV4RRFFQ69G5FAA"
		_, _, err := store.PutCronJob(ctx, db.PutCronJob{ProjectID: project.ID, Name: "Missing", Definition: missing, Actor: "worker"})
		require.ErrorIs(t, err, cron.ErrInvalid)
		require.ErrorContains(t, err, "missing flow dependency")
		deleted, _, err := store.PutCronFlow(ctx, db.PutCronFlow{UID: flow.UID, ProjectID: project.ID, Name: flow.Name, Definition: flow.Definition, ExpectedEventUID: flow.DefinitionEventUID, Actor: "worker", Deleted: true})
		require.NoError(t, err)
		_, _, err = store.PutCronJob(ctx, db.PutCronJob{ProjectID: project.ID, Name: "Deleted", Definition: jobDef, Actor: "worker"})
		require.ErrorIs(t, err, cron.ErrInvalid)
		require.ErrorContains(t, err, "tombstoned flow dependency")
		flow, _, err = store.PutCronFlow(ctx, db.PutCronFlow{UID: flow.UID, ProjectID: project.ID, Name: flow.Name, Definition: flow.Definition, ExpectedEventUID: deleted.DefinitionEventUID, Actor: "worker"})
		require.NoError(t, err)
	})
	_, _, err = store.PutCronJob(ctx, db.PutCronJob{ProjectID: other.ID, Name: "Wrong project", Definition: jobDef, Actor: "worker"})
	require.Error(t, err)
	job, events, err := store.PutCronJob(ctx, db.PutCronJob{ProjectID: project.ID, Name: "Review", Definition: jobDef, Actor: "worker"})
	require.NoError(t, err)
	require.Equal(t, events[0].UID, job.DefinitionEventUID)
	require.Equal(t, events[0].HLCPhysicalMS, job.DefinitionHLC.PhysicalMS)
	require.Equal(t, "worker", events[0].Actor)
	require.Equal(t, int64(1), job.Revision)
	read, err := store.CronJob(ctx, project.ID, job.UID)
	require.NoError(t, err)
	require.Equal(t, job, read)
	_, err = store.CronJob(ctx, other.ID, job.UID)
	require.ErrorIs(t, err, db.ErrNotFound)
	jobs, err := store.ListCronJobs(ctx, db.CronList{ProjectID: project.ID})
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	before, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	update := db.PutCronJob{UID: job.UID, ProjectID: project.ID, Name: "Edited", Definition: jobDef, ExpectedEventUID: job.DefinitionEventUID, Actor: "editor"}
	updated, events, err := store.PutCronJob(ctx, update)
	require.NoError(t, err)
	require.Greater(t, events[0].ID, before)
	require.Equal(t, int64(2), updated.Revision)
	require.NotEqual(t, job.DefinitionEventUID, updated.DefinitionEventUID)
	require.Equal(t, job.Author, updated.Author)
	_, _, err = store.PutCronJob(ctx, update)
	require.ErrorIs(t, err, db.ErrCronConflict)
	after, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	require.Equal(t, events[0].ID, after, "conflicting write emits no event")
	update.ExpectedEventUID = updated.DefinitionEventUID
	update.Deleted = true
	deleted, deletedEvent, err := store.PutCronJob(ctx, update)
	require.NoError(t, err)
	require.NotNil(t, deleted.DeletedAt)
	require.Equal(t, "cron.job.deleted", deletedEvent[0].Type)
	jobs, err = store.ListCronJobs(ctx, db.CronList{ProjectID: project.ID})
	require.NoError(t, err)
	require.Empty(t, jobs)
	update.ExpectedEventUID = deleted.DefinitionEventUID
	update.Deleted = false
	restored, events, err := store.PutCronJob(ctx, update)
	require.NoError(t, err)
	require.Nil(t, restored.DeletedAt)
	require.Equal(t, "cron.job.restored", events[0].Type)
	var payload map[string]any
	require.NoError(t, json.Unmarshal([]byte(events[0].Payload), &payload))
	require.Equal(t, job.UID, payload["uid"])
	_, _, err = store.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: project.ID, Actor: "worker"})
	require.NoError(t, err)
	update.ExpectedEventUID = restored.DefinitionEventUID
	_, _, err = store.PutCronJob(ctx, update)
	require.Error(t, err)
	_, _, err = store.PutCronFlow(ctx, db.PutCronFlow{ProjectID: project.ID, Name: "Archived", Definition: flowDef, Actor: "worker"})
	require.Error(t, err)
	return nil
}
