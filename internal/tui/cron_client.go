package tui

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	kataapi "go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/pkg/client/generated"
)

type cronAPI interface {
	ListCronJobs(context.Context, int64) ([]generated.CronJob, error)
	ShowCronJob(ctx context.Context, projectID int64, uid string) (db.CronJob, error)
	PutCronJob(ctx context.Context, projectID int64, uid string, write kataapi.PutCronJobBody) error
	ListCronWorkflows(context.Context, int64) ([]generated.CronWorkflow, error)
	ShowCronWorkflow(ctx context.Context, projectID int64, uid string) (db.CronWorkflow, error)
	PutCronWorkflow(ctx context.Context, projectID int64, uid string, write kataapi.PutCronWorkflowBody) error
	ListCronRuns(context.Context, int64) ([]generated.CronRun, error)
	readCronParent(ctx context.Context, projectID int64, parentUID string) (cronParent, error)
	CronUsageSource(context.Context, int64) (generated.CronUsageSourceSource, error)
	CronRunUsage(ctx context.Context, projectID int64, runUID string, refresh bool) (generated.RunUsage, error)
}

// cronConflictCurrent returns the server's current record from a refused
// guarded save (a cron_conflict answer), or false for any other error.
func cronConflictCurrent(err error) (jsontext.Value, bool) {
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr == nil || apiErr.Status != http.StatusConflict || apiErr.Code != "cron_conflict" {
		return nil, false
	}
	var data struct {
		Current jsontext.Value `json:"current"`
	}
	if json.Unmarshal(apiErr.Data, &data) != nil || len(data.Current) == 0 {
		return nil, false
	}
	return data.Current, true
}

// ListCronJobs reads the selected project's current job definitions.
func (c *Client) ListCronJobs(ctx context.Context, projectID int64) ([]generated.CronJob, error) {
	client, err := c.generatedClient()
	if err != nil {
		return nil, err
	}
	wire, callErr := client.ListCronJobsWithResponse(ctx, &generated.ListCronJobsRequestOptions{
		PathParams: &generated.ListCronJobsPath{ProjectID: projectID},
	})
	if wire == nil {
		return nil, callErr
	}
	if err := decodeGeneratedResponse(wire.HTTPResponse, wire.Body, callErr, nil); err != nil {
		return nil, err
	}
	return wire.JSON200.Jobs, nil
}

// ListCronWorkflows reads the selected project's current workflow definitions.
func (c *Client) ListCronWorkflows(ctx context.Context, projectID int64) ([]generated.CronWorkflow, error) {
	client, err := c.generatedClient()
	if err != nil {
		return nil, err
	}
	wire, callErr := client.ListCronWorkflowsWithResponse(ctx, &generated.ListCronWorkflowsRequestOptions{
		PathParams: &generated.ListCronWorkflowsPath{ProjectID: projectID},
	})
	if wire == nil {
		return nil, callErr
	}
	if err := decodeGeneratedResponse(wire.HTTPResponse, wire.Body, callErr, nil); err != nil {
		return nil, err
	}
	return wire.JSON200.Workflows, nil
}

// ListCronRuns reads the newest 100 observations.
func (c *Client) ListCronRuns(ctx context.Context, projectID int64) ([]generated.CronRun, error) {
	client, err := c.generatedClient()
	if err != nil {
		return nil, err
	}
	query := &generated.ListCronRunsQuery{Limit: new(int64(100))}
	wire, callErr := client.ListCronRunsWithResponse(ctx, &generated.ListCronRunsRequestOptions{
		PathParams: &generated.ListCronRunsPath{ProjectID: projectID},
		Query:      query,
	})
	if wire == nil {
		return nil, callErr
	}
	if err := decodeGeneratedResponse(wire.HTTPResponse, wire.Body, callErr, nil); err != nil {
		return nil, err
	}
	return wire.JSON200.Runs, nil
}

// CronUsageSource reads which usage source the daemon prices runs with. An
// older daemon that omits it reports none.
func (c *Client) CronUsageSource(ctx context.Context, projectID int64) (generated.CronUsageSourceSource, error) {
	client, err := c.generatedClient()
	if err != nil {
		return "", err
	}
	wire, callErr := client.GetCronCapabilitiesWithResponse(ctx, &generated.GetCronCapabilitiesRequestOptions{
		PathParams: &generated.GetCronCapabilitiesPath{ProjectID: projectID},
	})
	if wire == nil {
		return "", callErr
	}
	if err := decodeGeneratedResponse(wire.HTTPResponse, wire.Body, callErr, nil); err != nil {
		return "", err
	}
	return wire.JSON200.Usage.Source, nil
}

// cronUsageTimeout bounds a usage read. The daemon prices sessions for up to
// five seconds and then answers with the total it has, so this read alone
// waits longer than the TUI's ordinary five-second request timeout.
const cronUsageTimeout = 8 * time.Second

type cronUsageRequestKey struct{}

// usageHTTPClient gives a usage read the selected daemon client with only its
// timeout raised, keeping its transport, credentials and redirect policy.
func usageHTTPClient(ctx context.Context, hc *http.Client) *http.Client {
	if hc == nil || ctx.Value(cronUsageRequestKey{}) == nil {
		return hc
	}
	usage := *hc
	usage.Timeout = cronUsageTimeout
	return &usage
}

// CronRunUsage reads one run's live cost; refresh asks the daemon to reprice it.
func (c *Client) CronRunUsage(ctx context.Context, projectID int64, runUID string, refresh bool) (generated.RunUsage, error) {
	client, err := c.generatedClient()
	if err != nil {
		return generated.RunUsage{}, err
	}
	wire, callErr := client.GetCronRunUsageWithResponse(context.WithValue(ctx, cronUsageRequestKey{}, true), &generated.GetCronRunUsageRequestOptions{
		PathParams: &generated.GetCronRunUsagePath{ProjectID: projectID, RunUID: runUID},
		Query:      &generated.GetCronRunUsageQuery{Refresh: &refresh},
	})
	if wire == nil {
		return generated.RunUsage{}, callErr
	}
	if err := decodeGeneratedResponse(wire.HTTPResponse, wire.Body, callErr, nil); err != nil {
		return generated.RunUsage{}, err
	}
	return *wire.JSON200, nil
}

// cronParent is what a native job's parent issue tells the Jobs tab: whether
// the daemon still delivers (only an open parent does) and each job's progress.
type cronParent struct {
	open     bool
	progress map[string]cron.RecurringProgress
}

// readCronParent reads a parent issue's status and the delivery checkpoints its
// native recurring jobs keep, keyed by job UID, in one show request.
func (c *Client) readCronParent(ctx context.Context, projectID int64, parentUID string) (cronParent, error) {
	client, err := c.generatedClient()
	if err != nil {
		return cronParent{}, err
	}
	wire, callErr := client.ShowIssueWithResponse(ctx, &generated.ShowIssueRequestOptions{
		PathParams: &generated.ShowIssuePath{ProjectID: projectID, Ref: parentUID},
		Query:      &generated.ShowIssueQuery{IncludeDeleted: new(true)},
	})
	if wire == nil {
		return cronParent{}, callErr
	}
	if err := decodeGeneratedResponse(wire.HTTPResponse, wire.Body, callErr, nil); err != nil {
		return cronParent{}, err
	}
	issue := wire.JSON200.Issue
	return cronParent{open: issue.Status == "open" && issue.DeletedAt == nil, progress: cronProgressFromMetadata(issue.Metadata)}, nil
}

// cronProgressFromMetadata decodes each cron.progress.<job-uid> value. An entry
// that does not decode is skipped, so only its own job loses the preview.
func cronProgressFromMetadata(metadata map[string]any) map[string]cron.RecurringProgress {
	out := map[string]cron.RecurringProgress{}
	for key, value := range metadata {
		jobUID, ok := strings.CutPrefix(key, "cron.progress.")
		if !ok {
			continue
		}
		raw, err := json.Marshal(value)
		if err != nil {
			continue
		}
		if progress, err := cron.DecodeRecurringProgress(raw); err == nil {
			out[jobUID] = progress
		}
	}
	return out
}

// decodeCronJobRecord reads the envelope without passing options through a generated map.
func decodeCronJobRecord(body []byte) (db.CronJob, error) {
	var wire struct {
		Job jsontext.Value `json:"job"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		return db.CronJob{}, err
	}
	return decodeCronJob(wire.Job)
}

func decodeCronJob(body []byte) (db.CronJob, error) {
	var job kataapi.CronJob
	if err := json.Unmarshal(body, &job, json.RejectUnknownMembers(true)); err != nil {
		return db.CronJob{}, fmt.Errorf("use a newer kata to edit this job: %w", err)
	}
	return db.CronJob{CronDefinition: job.CronDefinition, Definition: job.Definition.Native()}, nil
}

// ShowCronJob reads one job, archived or not.
func (c *Client) ShowCronJob(ctx context.Context, projectID int64, uid string) (db.CronJob, error) {
	client, err := c.generatedClient()
	if err != nil {
		return db.CronJob{}, err
	}
	wire, callErr := client.ShowCronJobWithResponse(ctx, &generated.ShowCronJobRequestOptions{
		PathParams: &generated.ShowCronJobPath{ProjectID: projectID, CronUID: uid},
	})
	if wire == nil {
		return db.CronJob{}, callErr
	}
	if err := decodeGeneratedResponse(wire.HTTPResponse, wire.Body, callErr, nil); err != nil {
		return db.CronJob{}, err
	}
	return decodeCronJobRecord(wire.Body)
}

// PutCronJob creates or replaces a job, the same call `kata cron job create`
// and `update` make. The body reaches the generated type through its own
// decoder, which keeps option numbers exact.
func (c *Client) PutCronJob(ctx context.Context, projectID int64, uid string, write kataapi.PutCronJobBody) error {
	client, err := c.generatedClient()
	if err != nil {
		return err
	}
	raw, err := json.Marshal(write)
	if err != nil {
		return err
	}
	var body generated.PutCronJobBody
	if err := json.Unmarshal(raw, &body); err != nil {
		return err
	}
	wire, callErr := client.ReplaceCronJobWithResponse(ctx, &generated.ReplaceCronJobRequestOptions{
		PathParams: &generated.ReplaceCronJobPath{ProjectID: projectID, CronUID: uid},
		Body:       &body,
	})
	if wire == nil {
		return callErr
	}
	return decodeGeneratedResponse(wire.HTTPResponse, wire.Body, callErr, nil)
}

// decodeCronWorkflow reads a workflow without passing options through a generated map.
func decodeCronWorkflow(body []byte) (db.CronWorkflow, error) {
	var workflow kataapi.CronWorkflow
	if err := json.Unmarshal(body, &workflow, json.RejectUnknownMembers(true)); err != nil {
		return db.CronWorkflow{}, fmt.Errorf("use a newer kata to edit this workflow: %w", err)
	}
	return db.CronWorkflow{CronDefinition: workflow.CronDefinition, Definition: workflow.Definition.Native()}, nil
}

// ShowCronWorkflow reads one workflow, archived or not.
func (c *Client) ShowCronWorkflow(ctx context.Context, projectID int64, uid string) (db.CronWorkflow, error) {
	client, err := c.generatedClient()
	if err != nil {
		return db.CronWorkflow{}, err
	}
	wire, callErr := client.ShowCronWorkflowWithResponse(ctx, &generated.ShowCronWorkflowRequestOptions{
		PathParams: &generated.ShowCronWorkflowPath{ProjectID: projectID, CronUID: uid},
	})
	if wire == nil {
		return db.CronWorkflow{}, callErr
	}
	if err := decodeGeneratedResponse(wire.HTTPResponse, wire.Body, callErr, nil); err != nil {
		return db.CronWorkflow{}, err
	}
	var envelope struct {
		Workflow jsontext.Value `json:"workflow"`
	}
	if err := json.Unmarshal(wire.Body, &envelope); err != nil {
		return db.CronWorkflow{}, err
	}
	return decodeCronWorkflow(envelope.Workflow)
}

// PutCronWorkflow creates or replaces a workflow. The body reaches the
// generated type through its own decoder, which keeps option numbers exact.
func (c *Client) PutCronWorkflow(ctx context.Context, projectID int64, uid string, write kataapi.PutCronWorkflowBody) error {
	client, err := c.generatedClient()
	if err != nil {
		return err
	}
	raw, err := json.Marshal(write)
	if err != nil {
		return err
	}
	var body generated.PutCronWorkflowBody
	if err := json.Unmarshal(raw, &body); err != nil {
		return err
	}
	wire, callErr := client.ReplaceCronWorkflowWithResponse(ctx, &generated.ReplaceCronWorkflowRequestOptions{
		PathParams: &generated.ReplaceCronWorkflowPath{ProjectID: projectID, CronUID: uid},
		Body:       &body,
	})
	if wire == nil {
		return callErr
	}
	return decodeGeneratedResponse(wire.HTTPResponse, wire.Body, callErr, nil)
}
