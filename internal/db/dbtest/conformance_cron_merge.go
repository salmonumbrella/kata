package dbtest

import (
	"encoding/json/jsontext"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

func checkCronProjectMergeRestore(t *testing.T, source db.Storage, backend Backend) error {
	ctx := t.Context()
	sourceProject, err := source.CreateProject(ctx, "merge-source")
	require.NoError(t, err)
	targetProject, err := source.CreateProject(ctx, "merge-target")
	require.NoError(t, err)

	job, workflow := nativeFederationDefinitions(t, source, sourceProject)
	job.Definition.Enabled = true
	job.Definition.Action.WorkflowUID = ""
	job.Definition.Action.Prompt = "Inspect"
	job.Definition.Options = []byte(`{"inputs":{"flag":{"type":"bool"}}}`)
	job, _, err = source.PutCronJob(ctx, db.PutCronJob{ProjectID: sourceProject.ID, UID: job.UID, Name: job.Name, Definition: job.Definition, ExpectedEventUID: job.DefinitionEventUID, Actor: "worker"})
	require.NoError(t, err)
	targetJob, _, err := source.PutCronJob(ctx, db.PutCronJob{ProjectID: targetProject.ID, Name: "Inspect", Definition: job.Definition, Actor: "worker"})
	require.NoError(t, err)
	now := time.Now().UTC()
	sourceRequest := requestUIDAt(t, now)
	targetRequest := requestUIDAt(t, now)
	for _, in := range []db.RequestCronIn{
		{ProjectID: sourceProject.ID, UID: sourceRequest, Kind: "run", TargetUID: job.UID, Actor: "worker", Now: now, Inputs: map[string]jsontext.Value{"flag": jsontext.Value("true")}},
		{ProjectID: targetProject.ID, UID: targetRequest, Kind: "run", TargetUID: targetJob.UID, Actor: "worker", Now: now, Inputs: map[string]jsontext.Value{"flag": jsontext.Value("false")}},
	} {
		_, err = source.RequestCron(ctx, in)
		require.NoError(t, err)
	}
	runUID, err := uid.New()
	require.NoError(t, err)
	_, err = source.ObserveCronRun(ctx, db.ObserveCronRun{
		ProjectID: sourceProject.ID, UID: runUID, JobUID: &job.UID,
		DefinitionEventUID: &job.DefinitionEventUID, WorkflowUID: &workflow.UID,
		WorkflowDefinitionEventUID: &workflow.DefinitionEventUID, Actor: "worker",
		Status: "running", Summary: cron.Summary{Version: 1},
	})
	require.NoError(t, err)

	_, err = source.MergeProjects(ctx, db.MergeProjectsParams{
		SourceProjectID: sourceProject.ID, TargetProjectID: targetProject.ID, Actor: "worker",
	})
	require.NoError(t, err)
	records, err := CollectImportRecords(ctx, source, db.ExportFilter{IncludeDeleted: true})
	require.NoError(t, err)

	var cronEventUIDs []string
	for _, record := range records {
		event, ok := record.(*db.EventExport)
		if ok && strings.HasPrefix(event.Type, "cron.") {
			cronEventUIDs = append(cronEventUIDs, event.UID)
		}
	}
	require.NotEmpty(t, cronEventUIDs)
	mergedEvents, err := source.EventsByUIDs(ctx, targetProject.ID, cronEventUIDs)
	require.NoError(t, err)
	require.Len(t, mergedEvents, len(cronEventUIDs))
	for _, event := range mergedEvents {
		_, _, err := db.ValidateRemoteEventContentHash(portableNativeEvent(event))
		require.NoError(t, err, "merged cron events retain valid portable content hashes")
	}

	restored := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, restored.Close()) })
	_, err = restored.CreateProject(ctx, "retained-project")
	require.NoError(t, err)
	require.NoError(t, restored.ImportReplay(ctx, records, db.ImportOptions{}), "a merged cron history export must restore")

	target, err := restored.ProjectByUID(ctx, targetProject.UID)
	require.NoError(t, err)
	require.Contains(t, string(target.Metadata), "cron.request_inputs."+targetRequest)
	require.NotContains(t, string(target.Metadata), "cron.request_inputs."+sourceRequest)
	retainedSource, err := restored.ProjectByUID(ctx, sourceProject.UID)
	require.NoError(t, err)
	require.Contains(t, string(retainedSource.Metadata), "cron.request_inputs."+sourceRequest)
	restoredJob, err := restored.CronJob(ctx, target.ID, job.UID)
	require.NoError(t, err)
	require.Equal(t, job.UID, restoredJob.UID)
	restoredWorkflow, err := restored.CronWorkflow(ctx, target.ID, workflow.UID)
	require.NoError(t, err)
	require.Equal(t, workflow.UID, restoredWorkflow.UID)
	restoredRun, err := restored.CronRun(ctx, target.ID, runUID)
	require.NoError(t, err)
	require.Equal(t, runUID, restoredRun.UID)

	return nil
}
