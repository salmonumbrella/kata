package mcpserver

import (
	"encoding/json"
	"encoding/json/jsontext"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
)

func TestCronRequestTools(t *testing.T) {
	f := newCronFixture(t)
	definition := cronDefinition()
	definition["enabled"] = true
	job := cronRecord(t, f.session, "cron_job_create", map[string]any{"uid": cronNewUID(t, f.session), "name": "Execute review", "definition": definition}, "job")
	id := cronNewUID(t, f.session)
	request := cronRecord(t, f.session, "cron_job_request_run", map[string]any{"uid": job["uid"], "request_uid": id, "ttl_seconds": 60}, "request")
	require.Equal(t, id, request["uid"])
	require.Equal(t, "example-agent", request["requested_by"])
	require.Equal(t, job["definition_event_uid"], request["definition_event_uid"])
	require.True(t, cronCall(t, f.session, "cron_job_request_run", map[string]any{"uid": job["uid"]}).IsError)
	runID := cronNewUID(t, f.session)
	jobID, definitionID := job["uid"].(string), job["definition_event_uid"].(string)
	_, err := f.store.ObserveCronRun(t.Context(), db.ObserveCronRun{ProjectID: f.project.ID, UID: runID, Actor: "example-agent", JobUID: &jobID, DefinitionEventUID: &definitionID, Status: "running", Summary: cron.Summary{Version: 1}})
	require.NoError(t, err)
	cancel := cronRecord(t, f.session, "cron_run_request_cancel", map[string]any{"uid": runID, "request_uid": cronNewUID(t, f.session)}, "request")
	require.Equal(t, "cancel", cancel["kind"])
	require.Equal(t, runID, cancel["run_uid"])
	require.Len(t, cronOK(t, f.session, "cron_request_list", map[string]any{"state": "pending"})["requests"], 2)
}

// Contract: run request inputs are visible through request-aware resolution.
func TestCronRequestInputsTools(t *testing.T) {
	f := newCronFixture(t)
	definition := cronDefinition()
	definition["enabled"] = true
	definition["options"] = map[string]any{"inputs": map[string]any{"count": map[string]any{"type": "int", "required": true}}}
	job := cronRecord(t, f.session, "cron_job_create", map[string]any{"uid": cronNewUID(t, f.session), "name": "Execute review", "definition": definition}, "job")
	id := cronNewUID(t, f.session)
	cronRecord(t, f.session, "cron_job_request_run", map[string]any{"uid": job["uid"], "request_uid": id, "inputs": map[string]any{"count": int64(9007199254740993)}}, "request")
	requests, err := f.store.ListCronRequests(t.Context(), db.CronRequestList{ProjectID: f.project.ID})
	require.NoError(t, err)
	require.Equal(t, jsontext.Value("9007199254740993"), requests[0].Inputs["count"])
	resolution := cronOK(t, f.session, "cron_job_inputs", map[string]any{"uid": job["uid"], "request_uid": id})
	input := resolution["inputs"].([]any)[0].(map[string]any)
	require.Equal(t, "request", input["source"])
	require.NotNil(t, input["value"])
}

// Contract: published output schemas accept the actual retained scalar values.
func TestCronRequestInputsOutputSchema(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind string
		value any
	}{
		{"string", "string", "example"},
		{"integer", "int", int64(-42)},
		{"boolean", "bool", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCronFixture(t)
			definition := cronDefinition()
			definition["enabled"] = true
			definition["options"] = map[string]any{"inputs": map[string]any{"value": map[string]any{"type": tc.kind, "required": true}}}
			job := cronRecord(t, f.session, "cron_job_create", map[string]any{"uid": cronNewUID(t, f.session), "name": "Fixture job", "definition": definition}, "job")
			listed, err := f.session.ListTools(t.Context(), nil)
			require.NoError(t, err)
			response := cronOK(t, f.session, "cron_job_request_run", map[string]any{"uid": job["uid"], "request_uid": cronNewUID(t, f.session), "inputs": map[string]any{"value": tc.value}})
			checked := 0
			for _, tool := range listed.Tools {
				if tool.Name != "kata.cron_job_request_run" && tool.Name != "kata.cron_request_list" {
					continue
				}
				checked++
				var schema jsonschema.Schema
				require.NoError(t, json.Unmarshal(mustJSON(t, tool.OutputSchema), &schema))
				resolved, err := schema.Resolve(nil)
				require.NoError(t, err)
				var result map[string]any
				if tool.Name == "kata.cron_job_request_run" {
					result = response
				} else {
					result = cronOK(t, f.session, "cron_request_list", map[string]any{})
				}
				var request map[string]any
				if tool.Name == "kata.cron_job_request_run" {
					request = result["request"].(map[string]any)
				} else {
					requests := result["requests"].([]any)
					require.Len(t, requests, 1)
					request = requests[0].(map[string]any)
				}
				assert.JSONEq(t, string(mustJSON(t, tc.value)), string(mustJSON(t, request["inputs"].(map[string]any)["value"])))
				assert.NoError(t, resolved.Validate(result), "%s: %s", tool.Name, mustJSON(t, result))
			}
			require.Equal(t, 2, checked)
		})
	}
}
