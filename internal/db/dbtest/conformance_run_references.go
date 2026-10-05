package dbtest

import (
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

func runReferenceCases(job, otherJob, foreignJob db.CronJob, flow, otherFlow, foreignFlow db.CronFlow, ordinaryEvent string) []struct {
	name string
	edit func(*db.CronRun)
} {
	return []struct {
		name string
		edit func(*db.CronRun)
	}{
		{"foreign job", func(r *db.CronRun) {
			r.JobUID, r.DefinitionEventUID = &foreignJob.UID, &foreignJob.DefinitionEventUID
		}},
		{"foreign flow", func(r *db.CronRun) {
			r.FlowUID, r.FlowDefinitionEventUID = &foreignFlow.UID, &foreignFlow.DefinitionEventUID
		}},
		{"mismatched job event", func(r *db.CronRun) { r.JobUID, r.DefinitionEventUID = &job.UID, &otherJob.DefinitionEventUID }},
		{"mismatched flow event", func(r *db.CronRun) {
			r.FlowUID, r.FlowDefinitionEventUID = &flow.UID, &otherFlow.DefinitionEventUID
		}},
		{"wrong definition kind", func(r *db.CronRun) { r.JobUID, r.DefinitionEventUID = &job.UID, &flow.DefinitionEventUID }},
		{"ordinary issue event", func(r *db.CronRun) { r.DefinitionEventUID = &ordinaryEvent }},
	}
}

func checkRunReferenceRestore(t *testing.T, source db.Storage, backend Backend) error {
	project, err := source.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	foreign, err := source.CreateProject(t.Context(), "other-project")
	require.NoError(t, err)
	job, flow := nativeFederationDefinitions(t, source, project)
	otherJob, otherFlow := nativeFederationDefinitions(t, source, project)
	foreignJob, foreignFlow := nativeFederationDefinitions(t, source, foreign)
	_, ordinaryEvent, err := source.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Review", Author: "worker"})
	require.NoError(t, err)
	runUID, err := uid.New()
	require.NoError(t, err)
	observed, err := source.ObserveCronRun(t.Context(), db.ObserveCronRun{ProjectID: project.ID, UID: runUID, JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, FlowUID: &flow.UID, FlowDefinitionEventUID: &flow.DefinitionEventUID, Actor: "worker", Status: "running", Summary: cron.Summary{Version: 1}})
	require.NoError(t, err)
	records, err := CollectImportRecords(t.Context(), source, db.ExportFilter{IncludeDeleted: true})
	require.NoError(t, err)
	for _, tc := range runReferenceCases(job, otherJob, foreignJob, flow, otherFlow, foreignFlow, ordinaryEvent.UID) {
		t.Run(tc.name, func(t *testing.T) {
			target := backend.Open(t)
			t.Cleanup(func() { require.NoError(t, target.Close()) })
			_, err := target.CreateProject(t.Context(), "retained-project")
			require.NoError(t, err)
			before, err := CollectImportRecords(t.Context(), target, db.ExportFilter{IncludeDeleted: true})
			require.NoError(t, err)
			bad := append([]db.ImportRecord(nil), records...)
			for i, record := range bad {
				if run, ok := record.(*db.CronRunExport); ok {
					value := db.CronRun(*run)
					tc.edit(&value)
					cloned := db.CronRunExport(value)
					bad[i] = &cloned
				}
			}
			require.Error(t, target.ImportReplay(t.Context(), bad, db.ImportOptions{}))
			require.Error(t, db.ValidateImportRecords(bad), "complete portable preflight rejects the known relationship")
			after, err := CollectImportRecords(t.Context(), target, db.ExportFilter{IncludeDeleted: true})
			require.NoError(t, err)
			require.Equal(t, before, after, "rejection precedes target replacement")
		})
	}
	// A compacted E1 remains portable even after the winning definition becomes E2.
	_, _, err = source.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, UID: job.UID, ExpectedEventUID: job.DefinitionEventUID, Name: "Edited review", Actor: "worker", Definition: job.Definition})
	require.NoError(t, err)
	records, err = CollectImportRecords(t.Context(), source, db.ExportFilter{IncludeDeleted: true})
	require.NoError(t, err)
	compacted := []db.ImportRecord{}
	for _, record := range records {
		if event, ok := record.(*db.EventExport); ok && event.UID == job.DefinitionEventUID {
			continue
		}
		compacted = append(compacted, record)
	}
	target := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, target.Close()) })
	require.NoError(t, target.ImportReplay(t.Context(), compacted, db.ImportOptions{}))
	retained, err := target.CronRun(t.Context(), project.ID, observed.Run.UID)
	require.NoError(t, err)
	require.Equal(t, observed.Run, retained)
	return nil
}

func checkRunReferenceFederation(t *testing.T, store db.Storage) error {
	project, err := store.CreateProject(t.Context(), "hub-project")
	require.NoError(t, err)
	foreign, err := store.CreateProject(t.Context(), "other-project")
	require.NoError(t, err)
	job, flow := nativeFederationDefinitions(t, store, project)
	otherJob, otherFlow := nativeFederationDefinitions(t, store, project)
	foreignJob, foreignFlow := nativeFederationDefinitions(t, store, foreign)
	_, ordinaryEvent, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Review", Author: "worker"})
	require.NoError(t, err)
	_, err = store.EnableProjectFederation(t.Context(), project.ID, "worker")
	require.NoError(t, err)
	origin, err := uid.New()
	require.NoError(t, err)
	grant, err := store.CreateFederationEnrollment(t.Context(), db.CreateFederationEnrollmentParams{SpokeInstanceUID: origin, ProjectID: &project.ID, Actor: "worker", Capabilities: "pull,push"})
	require.NoError(t, err)
	for _, tc := range runReferenceCases(job, otherJob, foreignJob, flow, otherFlow, foreignFlow, ordinaryEvent.UID) {
		for _, path := range []string{"pull", "push"} {
			t.Run(tc.name+"/"+path, func(t *testing.T) {
				id, err := uid.New()
				require.NoError(t, err)
				at := time.Now().UTC().Truncate(time.Millisecond)
				run := db.CronRun{UID: id, JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, FlowUID: &flow.UID, FlowDefinitionEventUID: &flow.DefinitionEventUID, Actor: "worker", Status: "running", Summary: cron.Summary{Version: 1}, Revision: 1, CreatedAt: at, UpdatedAt: at}
				tc.edit(&run)
				payload, err := json.Marshal(db.NewCronRunObservation(run, project.UID))
				require.NoError(t, err)
				eventUID, err := uid.New()
				require.NoError(t, err)
				event := db.RemoteEvent{EventUID: eventUID, OriginInstanceUID: origin, ProjectUID: project.UID, ProjectName: project.Name, Actor: "worker", Type: "cron.run.observed", Payload: payload, CreatedAt: at, HLCPhysicalMS: at.UnixMilli()}
				resignNativeEvent(t, &event)
				before, err := store.MaxEventID(t.Context())
				require.NoError(t, err)
				if path == "pull" {
					_, err = store.InsertRemoteEvent(t.Context(), project.ID, event)
				} else {
					_, err = store.IngestFederationEvents(t.Context(), db.FederationIngestParams{ProjectID: project.ID, FederationEnrollmentID: grant.Enrollment.ID, SpokeInstanceUID: origin, BoundActor: "worker", EventFeatures: db.CronEventFeature, Events: []db.FederationIngestEvent{{SourceEventID: 1, Event: event}}})
				}
				require.ErrorIs(t, err, db.ErrFederationIngestValidation)
				after, err := store.MaxEventID(t.Context())
				require.NoError(t, err)
				require.Equal(t, before, after)
				_, err = store.CronRun(t.Context(), project.ID, id)
				require.ErrorIs(t, err, db.ErrNotFound)
			})
		}
	}
	return nil
}

func checkRunCompactedProvenance(t *testing.T, _ db.Storage, backend Backend) error {
	for _, tombstone := range []bool{false, true} {
		t.Run(map[bool]string{false: "edited", true: "tombstoned"}[tombstone], func(t *testing.T) {
			source := backend.Open(t)
			t.Cleanup(func() { require.NoError(t, source.Close()) })
			project, err := source.CreateProject(t.Context(), "hub-project")
			require.NoError(t, err)
			job, flow := nativeFederationDefinitions(t, source, project)
			oldJobEvent, oldFlowEvent := job.DefinitionEventUID, flow.DefinitionEventUID
			id, err := uid.New()
			require.NoError(t, err)
			_, err = source.ObserveCronRun(t.Context(), db.ObserveCronRun{ProjectID: project.ID, UID: id, JobUID: &job.UID, DefinitionEventUID: &oldJobEvent, FlowUID: &flow.UID, FlowDefinitionEventUID: &oldFlowEvent, Actor: "worker", Status: "succeeded", Summary: cron.Summary{Version: 1}})
			require.NoError(t, err)
			job, _, err = source.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, UID: job.UID, ExpectedEventUID: job.DefinitionEventUID, Name: "Edited review", Actor: "worker", Definition: job.Definition})
			require.NoError(t, err)
			flow, _, err = source.PutCronFlow(t.Context(), db.PutCronFlow{ProjectID: project.ID, UID: flow.UID, ExpectedEventUID: flow.DefinitionEventUID, Name: "Edited flow", Actor: "worker", Definition: flow.Definition})
			require.NoError(t, err)
			if tombstone {
				_, _, err = source.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, UID: job.UID, ExpectedEventUID: job.DefinitionEventUID, Name: job.Name, Definition: job.Definition, Actor: "worker", Deleted: true})
				require.NoError(t, err)
				_, _, err = source.PutCronFlow(t.Context(), db.PutCronFlow{ProjectID: project.ID, UID: flow.UID, ExpectedEventUID: flow.DefinitionEventUID, Name: flow.Name, Definition: flow.Definition, Actor: "worker", Deleted: true})
				require.NoError(t, err)
			}
			binding, err := source.EnableProjectFederation(t.Context(), project.ID, "worker")
			require.NoError(t, err)
			baseline, err := source.EventsAfter(t.Context(), db.EventsAfterParams{ProjectID: project.ID, AfterID: binding.ReplayHorizonEventID - 1, Limit: 100})
			require.NoError(t, err)
			peer := backend.Open(t)
			t.Cleanup(func() { require.NoError(t, peer.Close()) })
			local, err := peer.CreateProject(t.Context(), "spoke-project")
			require.NoError(t, err)
			adopted, err := peer.AdoptProjectIntoFederation(t.Context(), db.AdoptProjectIntoFederationParams{ProjectID: local.ID, HubURL: "https://daemon.example", HubProjectID: project.ID, HubProjectUID: project.UID, ReplayHorizonEventID: binding.ReplayHorizonEventID, Actor: "worker", EmptyOnly: true})
			require.NoError(t, err)
			// Replay only baseline snapshots, deliberately run first; neither E1 edit
			// envelope survives this reset peer. Retained run provenance must suffice.
			for replay := range 2 {
				if replay == 1 {
					require.NoError(t, peer.ResetFederatedProject(t.Context(), local.ID, adopted.Binding.ReplayHorizonEventID, adopted.Binding.PullCursorEventID))
				}
				for _, kind := range []string{"cron.run.snapshot", "cron.job.snapshot", "cron.flow.snapshot"} {
					for _, event := range baseline {
						if event.Type != kind {
							continue
						}
						_, err := peer.InsertRemoteEvent(t.Context(), local.ID, portableNativeEvent(event))
						require.NoError(t, err)
						err = peer.MaterializeFederatedProject(t.Context(), local.ID)
						require.NoError(t, err)
					}
				}
			}
			prior, err := peer.CronRun(t.Context(), local.ID, id)
			require.NoError(t, err)
			require.Equal(t, &oldJobEvent, prior.DefinitionEventUID)
			require.Equal(t, &oldFlowEvent, prior.FlowDefinitionEventUID)
			current, err := peer.FederationBindingByProject(t.Context(), local.ID)
			require.NoError(t, err)
			_, err = peer.EnableFederationPush(t.Context(), local.ID, current.PushCursorEventID)
			require.NoError(t, err)
			bufferedUID, err := uid.New()
			require.NoError(t, err)
			buffered, err := peer.ObserveCronRun(t.Context(), db.ObserveCronRun{ProjectID: local.ID, UID: bufferedUID, JobUID: &job.UID, DefinitionEventUID: &oldJobEvent, FlowUID: &flow.UID, FlowDefinitionEventUID: &oldFlowEvent, Actor: "worker", Status: "succeeded", Summary: cron.Summary{Version: 1}})
			require.NoError(t, err, "a distinct buffered execution retains its compacted job and flow versions")
			require.Equal(t, &oldJobEvent, buffered.Run.DefinitionEventUID)
			require.Equal(t, &oldFlowEvent, buffered.Run.FlowDefinitionEventUID)
		})
	}
	return nil
}
