package api

import (
	"encoding/json/jsontext"

	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
)

// CronJobDefinition is the portable job document exposed by the API.
type CronJobDefinition struct {
	Version        int               `json:"version"`
	Kind           string            `json:"kind"`
	Enabled        bool              `json:"enabled"`
	Trigger        cron.Trigger      `json:"trigger"`
	Action         cron.Action       `json:"action"`
	CheckoutKey    string            `json:"checkout_key,omitempty"`
	Issue          *cron.IssuePolicy `json:"issue,omitempty"`
	Overlap        string            `json:"overlap"`
	Catchup        string            `json:"catchup"`
	GraceSeconds   int64             `json:"grace_seconds,omitzero"`
	TimeoutSeconds int64             `json:"timeout_seconds,omitzero"`
	SecretRefs     map[string]string `json:"secret_refs,omitempty"`
	Options        JSONRawObject     `json:"options,omitempty"`
}

// CronFlowStep is a portable shell or prompt step with dependency keys.
type CronFlowStep struct {
	Key     string        `json:"key"`
	Kind    string        `json:"kind"`
	Command string        `json:"command,omitempty"`
	Prompt  string        `json:"prompt,omitempty"`
	After   []string      `json:"after,omitempty"`
	Retries int           `json:"retries,omitzero"`
	Options JSONRawObject `json:"options,omitempty"`
}

// CronFlowDefinition is the portable ordered flow document exposed by the API.
type CronFlowDefinition struct {
	About   string         `json:"about,omitempty"`
	Input   string         `json:"input,omitempty"`
	Options JSONRawObject  `json:"options,omitempty"`
	Version int            `json:"version"`
	Steps   []CronFlowStep `json:"steps"`
}

// Native converts the API document to its domain representation.
func (value CronJobDefinition) Native() cron.JobDefinition {
	return cron.JobDefinition{Version: value.Version, Kind: value.Kind, Enabled: value.Enabled, Trigger: value.Trigger, Action: value.Action, CheckoutKey: value.CheckoutKey, Issue: value.Issue, Overlap: value.Overlap, Catchup: value.Catchup, GraceSeconds: value.GraceSeconds, TimeoutSeconds: value.TimeoutSeconds, SecretRefs: value.SecretRefs, Options: jsontext.Value(value.Options)}
}

// CronJobDefinitionFrom converts a storage value to its API representation.
func CronJobDefinitionFrom(value cron.JobDefinition) CronJobDefinition {
	return CronJobDefinition{Version: value.Version, Kind: value.Kind, Enabled: value.Enabled, Trigger: value.Trigger, Action: value.Action, CheckoutKey: value.CheckoutKey, Issue: value.Issue, Overlap: value.Overlap, Catchup: value.Catchup, GraceSeconds: value.GraceSeconds, TimeoutSeconds: value.TimeoutSeconds, SecretRefs: value.SecretRefs, Options: JSONRawObject(value.Options)}
}

// Native converts the API document to its domain representation.
func (value CronFlowDefinition) Native() cron.FlowDefinition {
	steps := make([]cron.FlowStep, 0, len(value.Steps))
	for _, step := range value.Steps {
		steps = append(steps, cron.FlowStep{Key: step.Key, Kind: step.Kind, Command: step.Command, Prompt: step.Prompt, After: step.After, Retries: step.Retries, Options: jsontext.Value(step.Options)})
	}
	return cron.FlowDefinition{Version: value.Version, About: value.About, Input: value.Input, Options: jsontext.Value(value.Options), Steps: steps}
}

// CronFlowDefinitionFrom converts a storage value to its API representation.
func CronFlowDefinitionFrom(value cron.FlowDefinition) CronFlowDefinition {
	steps := make([]CronFlowStep, 0, len(value.Steps))
	for _, step := range value.Steps {
		steps = append(steps, CronFlowStep{Key: step.Key, Kind: step.Kind, Command: step.Command, Prompt: step.Prompt, After: step.After, Retries: step.Retries, Options: JSONRawObject(step.Options)})
	}
	return CronFlowDefinition{Version: value.Version, About: value.About, Input: value.Input, Options: JSONRawObject(value.Options), Steps: steps}
}

// CronJob combines job identity and revision with its portable definition.
type CronJob struct {
	db.CronDefinition
	Definition CronJobDefinition `json:"definition"`
}

// CronFlow combines flow identity and revision with its portable definition.
type CronFlow struct {
	db.CronDefinition
	Definition CronFlowDefinition `json:"definition"`
}

// CronJobFrom converts a storage value to its API representation.
func CronJobFrom(value db.CronJob) CronJob {
	return CronJob{value.CronDefinition, CronJobDefinitionFrom(value.Definition)}
}

// CronFlowFrom converts a storage value to its API representation.
func CronFlowFrom(value db.CronFlow) CronFlow {
	return CronFlow{value.CronDefinition, CronFlowDefinitionFrom(value.Definition)}
}

// CronDefinitionRequest addresses one project-scoped definition.
type CronDefinitionRequest struct {
	ProjectID int64  `path:"project_id"`
	UID       string `path:"cron_uid"`
}

// ListCronDefinitionsRequest selects a project definition catalog.
type ListCronDefinitionsRequest struct {
	ProjectID      int64 `path:"project_id"`
	IncludeDeleted bool  `query:"include_deleted"`
}

// CronDefinitionActionBody attributes a lifecycle change and supplies its expected revision.
type CronDefinitionActionBody struct {
	Actor            string `json:"actor,omitempty"`
	ExpectedEventUID string `json:"expected_event_uid"`
}

// CronDefinitionActionRequest addresses an attributed definition lifecycle change.
type CronDefinitionActionRequest struct {
	CronDefinitionRequest
	Body CronDefinitionActionBody
}

// PutCronJobBody carries a job definition and optional replacement precondition.
type PutCronJobBody struct {
	Actor            string            `json:"actor,omitempty"`
	Name             string            `json:"name"`
	Definition       CronJobDefinition `json:"definition"`
	ExpectedEventUID string            `json:"expected_event_uid,omitempty"`
}

// PutCronFlowBody carries a flow definition and optional replacement precondition.
type PutCronFlowBody struct {
	Actor            string             `json:"actor,omitempty"`
	Name             string             `json:"name"`
	Definition       CronFlowDefinition `json:"definition"`
	ExpectedEventUID string             `json:"expected_event_uid,omitempty"`
}

// CreateCronJobRequest creates a definition in the selected project.
type CreateCronJobRequest struct {
	ProjectID int64 `path:"project_id"`
	Body      PutCronJobBody
}

// ReplaceCronJobRequest replaces one job at its expected definition event.
type ReplaceCronJobRequest struct {
	CreateCronJobRequest
	UID string `path:"cron_uid"`
}

// CreateCronFlowRequest creates a flow in the selected project.
type CreateCronFlowRequest struct {
	ProjectID int64 `path:"project_id"`
	Body      PutCronFlowBody
}

// ReplaceCronFlowRequest replaces one flow at its expected definition event.
type ReplaceCronFlowRequest struct {
	CreateCronFlowRequest
	UID string `path:"cron_uid"`
}

// CronJobResponse returns the job and events committed by its mutation.
type CronJobResponse struct {
	Body struct {
		Job    CronJob    `json:"job"`
		Events []db.Event `json:"events,omitempty"`
	}
}

// CronFlowResponse returns the flow and events committed by its mutation.
type CronFlowResponse struct {
	Body struct {
		Flow   CronFlow   `json:"flow"`
		Events []db.Event `json:"events,omitempty"`
	}
}

// ListCronJobsResponse returns the selected portable jobs.
type ListCronJobsResponse struct {
	Body struct {
		Jobs []CronJob `json:"jobs"`
	}
}

// ListCronFlowsResponse returns the selected portable flows.
type ListCronFlowsResponse struct {
	Body struct {
		Flows []CronFlow `json:"flows"`
	}
}
