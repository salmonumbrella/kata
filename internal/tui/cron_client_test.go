package tui

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/uid"
	"go.kenn.io/kata/pkg/client/generated"
)

func TestCronClientEnabledTimeoutDoesNotChangeReadTimeout(t *testing.T) {
	var remaining []time.Duration
	hc := &http.Client{Timeout: defaultHTTPTimeout, Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		deadline, ok := req.Context().Deadline()
		require.True(t, ok)
		remaining = append(remaining, time.Until(deadline))
		return nil, errors.New("request stopped after capturing its deadline")
	})}
	c := NewClient("https://daemon.example", hc)
	_, err := c.CronJobEnabled(t.Context(), 42, "example-job", "example-event", "operator", false, false)
	require.Error(t, err)
	_, err = c.ListCronJobs(t.Context(), 42)
	require.Error(t, err)

	require.Len(t, remaining, 2)
	require.Greater(t, remaining[0], 15*time.Second, "enabled actions must outlive the daemon's 15-second federation forward")
	require.Greater(t, remaining[1], 4*time.Second)
	require.LessOrEqual(t, remaining[1], defaultHTTPTimeout, "ordinary reads keep the default timeout")
	require.Equal(t, defaultHTTPTimeout, hc.Timeout, "the shared client keeps its default timeout")
}

func TestCronClientReadsStoredRecords(t *testing.T) {
	env := testenv.New(t)
	c := NewClient(env.URL, env.HTTP)
	ctx := t.Context()
	project, err := c.EnsureProject(ctx, "cron-example", "worker")
	require.NoError(t, err)
	client, err := c.generatedClient()
	require.NoError(t, err)
	workflow, err := client.CreateCronWorkflowWithResponse(ctx, &generated.CreateCronWorkflowRequestOptions{
		PathParams: &generated.CreateCronWorkflowPath{ProjectID: project.ID},
		Body: &generated.CreateCronWorkflowBody{
			Actor: new("worker"), Name: "Summary flow",
			Definition: generated.CronWorkflowDefinition{
				Version: 1,
				Steps: []generated.CronWorkflowStep{{
					Key: "summarize", Kind: "prompt", Prompt: new("Summarize"),
				}},
			},
		},
	})
	require.NoError(t, err)
	require.Equal(t, 201, workflow.HTTPResponse.StatusCode, string(workflow.Body))
	var w struct {
		Workflow api.CronWorkflow `json:"workflow"`
	}
	require.NoError(t, json.Unmarshal(workflow.Body, &w))
	job, err := client.CreateCronJobWithResponse(ctx, &generated.CreateCronJobRequestOptions{
		PathParams: &generated.CreateCronJobPath{ProjectID: project.ID},
		Body: &generated.CreateCronJobBody{
			Actor: new("worker"), Name: "Daily summary",
			Definition: generated.CronJobDefinition{
				Version: 1, Kind: "job", Enabled: true,
				Trigger: generated.Trigger{Kind: "manual"},
				Action:  generated.Action{Kind: "execute", WorkflowUID: &w.Workflow.UID},
				Issue:   &generated.IssuePolicy{Kind: "per-run", Title: new("Summary result")},
				Overlap: "forbid", Catchup: "skip",
			},
		},
	})
	require.NoError(t, err)
	require.Equal(t, 201, job.HTTPResponse.StatusCode, string(job.Body))
	var j struct {
		Job api.CronJob `json:"job"`
	}
	require.NoError(t, json.Unmarshal(job.Body, &j))
	run, err := client.ObserveCronRunWithResponse(ctx, &generated.ObserveCronRunRequestOptions{
		PathParams: &generated.ObserveCronRunPath{ProjectID: project.ID, RunUID: "01ARZ3NDEKTSV4RRFFQ69G5FAV"},
		Body: &generated.ObserveCronRunBody{
			Actor: new("worker"), Status: "succeeded",
			JobUID: &j.Job.UID, DefinitionEventUID: &j.Job.DefinitionEventUID,
			WorkflowUID: &w.Workflow.UID, WorkflowDefinitionEventUID: &w.Workflow.DefinitionEventUID,
			Summary: generated.Summary{Version: 1, Message: new("Report ready")},
		},
	})
	require.NoError(t, err)
	require.Equal(t, 200, run.HTTPResponse.StatusCode, string(run.Body))
	list, err := c.ListCronJobs(ctx, project.ID)
	require.NoError(t, err)
	jobs := list.Jobs
	require.Len(t, jobs, 1)
	require.Equal(t, "Daily summary", jobs[0].Name)
	require.Equal(t, project.ID, jobs[0].ProjectID)
	workflows, err := c.ListCronWorkflows(ctx, project.ID)
	require.NoError(t, err)
	require.Len(t, workflows, 1)
	require.Equal(t, "Summary flow", workflows[0].Name)
	runs, err := c.ListCronRuns(ctx, project.ID)
	require.NoError(t, err)
	require.Len(t, runs, 1)
	require.Equal(t, "Report ready", deref(runs[0].Summary.Message))
	require.Equal(t, project.ID, runs[0].ProjectID)
}

func TestCronClientWriteGuardsEdits(t *testing.T) {
	env := testenv.New(t)
	c := NewClient(env.URL, env.HTTP)
	project, err := c.EnsureProject(t.Context(), "cron-write-example", "alice")
	require.NoError(t, err)
	ctx := t.Context()
	const uid = "01ARZ3NDEKTSV4RRFFQ69G5FAV"

	require.NoError(t, c.PutCronJob(ctx, project.ID, uid, api.PutCronJobBody{Name: "Digest", Actor: "alice", Definition: api.CronJobDefinitionFrom(manualExecuteJob(`{}`))}))
	read, err := c.ShowCronJob(ctx, project.ID, uid)
	require.NoError(t, err)
	shown := read.Job

	shown.Definition.Enabled = false
	err = c.PutCronJob(ctx, project.ID, uid, api.PutCronJobBody{Name: "Digest", Actor: "alice", Definition: api.CronJobDefinitionFrom(shown.Definition), ExpectedEventUID: "01ARZ3NDEKTSV4RRFFQ69G5FB9"})
	var apiErr *APIError
	require.ErrorAs(t, err, &apiErr)
	require.Equal(t, "cron_conflict", apiErr.Code)
	require.Equal(t, 409, apiErr.Status)

	require.NoError(t, c.PutCronJob(ctx, project.ID, uid, api.PutCronJobBody{Name: "Digest", Actor: "alice", Definition: api.CronJobDefinitionFrom(shown.Definition), ExpectedEventUID: shown.DefinitionEventUID}))
	updated, err := c.ShowCronJob(ctx, project.ID, uid)
	require.NoError(t, err)
	require.False(t, updated.Job.Definition.Enabled)
}

func TestCronJobRecordRefusesDefinitionsItWouldDrop(t *testing.T) {
	body := `{"job":{"uid":"01ARZ3NDEKTSV4RRFFQ69G5FAV","name":"Digest","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FB2","definition":{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"manual"},"action":{"kind":"execute","prompt":"x"},"issue":{"kind":"per-run","title":"t"},"overlap":"forbid","catchup":"latest","options":{"free":1},"newer_member":{"a":1}}}}`
	_, err := decodeCronJobRecord([]byte(body))
	require.ErrorContains(t, err, "newer_member")
	require.ErrorContains(t, err, "newer kata")
}

// Pause, resume, and discard choose their route and body, and the returned
// job keeps options exact through the envelope that also carries a cutoff.
func TestCronClientEnabledWire(t *testing.T) {
	for _, tc := range []struct {
		name, path       string
		enabled, discard bool
	}{
		{name: "pause", path: "/pause"},
		{name: "resume", path: "/resume", enabled: true},
		{name: "discard", path: "/resume", enabled: true, discard: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			record := pausedNative()
			record.Definition.Options = []byte(`{"large":9007199254740993}`)
			client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, http.MethodPost, r.Method)
				require.True(t, strings.HasSuffix(r.URL.Path, "/cron/jobs/"+record.UID+tc.path), r.URL.Path)
				var body map[string]jsontext.Value
				require.NoError(t, json.UnmarshalRead(r.Body, &body))
				if tc.discard {
					require.Equal(t, "true", string(body["discard_missed"]))
				} else {
					require.NotContains(t, body, "discard_missed")
				}
				require.JSONEq(t, fmt.Sprintf("%q", record.DefinitionEventUID), string(body["expected_event_uid"]))
				respondJSON(t, w, map[string]any{"job": api.CronJobFrom(record), "passover": db.CronPassoverState{Through: time.Now().UTC(), Scope: "job"}})
			})
			got, err := client.CronJobEnabled(t.Context(), 7, record.UID, record.DefinitionEventUID, "operator", tc.enabled, tc.discard)
			require.NoError(t, err)
			require.Equal(t, string(record.Definition.Options), string(got.Definition.Options))
		})
	}
}

func TestCronClientPassoverEnvelope(t *testing.T) {
	record := pausedNative()
	record.Definition.Options = []byte(`{"large":9007199254740993}`)
	cutoff := time.Now().UTC().Truncate(time.Second)
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		state := db.CronPassoverState{Through: cutoff, Scope: "job"}
		if strings.HasSuffix(r.URL.Path, "/jobs") {
			respondJSON(t, w, map[string]any{"jobs": []api.CronJob{api.CronJobFrom(record)}, "passovers": map[string]any{record.UID: state}})
			return
		}
		respondJSON(t, w, map[string]any{"job": api.CronJobFrom(record), "passover": state})
	})
	one, err := client.ShowCronJob(t.Context(), 7, record.UID)
	require.NoError(t, err)
	require.Equal(t, cutoff, one.Passover.Through)
	require.Equal(t, string(record.Definition.Options), string(one.Job.Definition.Options))
	list, err := client.ListCronJobs(t.Context(), 7)
	require.NoError(t, err)
	require.Len(t, list.Jobs, 1)
	require.Equal(t, one.Passover, list.Passovers[record.UID])
}

// Run requests target a job and cancellation requests a run; both are listed
// with the caller's UID, and the run read returns the live observation.
func TestCronClientRuntimeRequestsRoundTrip(t *testing.T) {
	env := testenv.New(t)
	c := NewClient(env.URL, env.HTTP)
	ctx := t.Context()
	project, err := env.DB.CreateProject(ctx, "request-project")
	require.NoError(t, err)
	job, _, err := env.DB.PutCronJob(ctx, db.PutCronJob{ProjectID: project.ID, Name: "Summary", Actor: "operator", Definition: executeRecord(`{}`).Definition})
	require.NoError(t, err)
	runID, err := uid.New()
	require.NoError(t, err)
	observed, err := env.DB.ObserveCronRun(ctx, db.ObserveCronRun{ProjectID: project.ID, UID: runID, Actor: "operator", JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, Status: "running", Summary: cron.Summary{Version: 1}})
	require.NoError(t, err)
	shown, err := c.ShowCronRun(ctx, project.ID, runID)
	require.NoError(t, err)
	require.Equal(t, observed.Run, shown)

	runRequest, err := uid.New()
	require.NoError(t, err)
	require.NoError(t, c.RequestCron(ctx, project.ID, runRequest, db.CronRequestKindRun, job.UID, "operator", nil))
	cancelRequest, err := uid.New()
	require.NoError(t, err)
	require.NoError(t, c.RequestCron(ctx, project.ID, cancelRequest, db.CronRequestKindCancel, runID, "operator", nil))
	requests, err := c.ListCronRequests(ctx, project.ID)
	require.NoError(t, err)
	byUID := map[string]db.CronRequest{}
	for _, request := range requests {
		byUID[request.UID] = request
	}
	require.Len(t, byUID, 2)
	require.Equal(t, db.CronRequestKindRun, byUID[runRequest].Kind)
	require.Equal(t, job.UID, byUID[runRequest].JobUID)
	require.Equal(t, "pending", byUID[runRequest].State)
	require.Equal(t, db.CronRequestKindCancel, byUID[cancelRequest].Kind)
	require.Equal(t, runID, *byUID[cancelRequest].RunUID)
}

func TestCronClientRequestInputsExactLiterals(t *testing.T) {
	var body string
	hc := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		response := `{"api_schema_version":"0.49.0"}`
		if strings.Contains(req.URL.Path, "/inputs") {
			response = `{"job_uid":"job","definition_event_uid":"event","declared_by":"job","inputs":[{"name":"count","type":"int","value":9007199254740993,"source":"job"}],"errors":[]}`
		} else if req.Method == http.MethodPost {
			raw, err := io.ReadAll(req.Body)
			require.NoError(t, err)
			body = string(raw)
			response = `{}`
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(response)), Request: req}, nil
	})}
	c := NewClient("https://daemon.example", hc)
	read, err := c.CronJobInputs(t.Context(), 7, "job")
	require.NoError(t, err)
	require.Equal(t, jsontext.Value("9007199254740993"), read.Inputs[0].Value)
	require.NoError(t, c.RequestCron(t.Context(), 7, "01ARZ3NDEKTSV4RRFFQ69G5FAV", db.CronRequestKindRun, "job", "operator", map[string]jsontext.Value{"count": []byte("9007199254740993")}))
	require.Contains(t, body, `"count":9007199254740993`)
}

func TestCronClientRequestInputsVersionAndNotFound(t *testing.T) {
	for _, tc := range []struct{ version string; unsupported bool }{{"0.48.0", true}, {"0.49.0", false}} {
		t.Run(tc.version, func(t *testing.T) {
			calls := 0
			hc := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				status, body := http.StatusOK, `{"api_schema_version":"`+tc.version+`"}`
				if calls > 1 { status, body = http.StatusNotFound, `{}` }
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
			})}
			c := NewClient("https://daemon.example", hc)
			_, err := c.CronJobInputs(t.Context(), 7, "job")
			require.Error(t, err)
			require.Equal(t, tc.unsupported, errors.Is(err, errCronInputsUnsupported))
			if tc.unsupported { require.Equal(t, 1, calls) } else { require.Equal(t, 2, calls) }
		})
	}
}
