// Package cron defines portable cron documents. It never runs
// commands, registers executors, or evaluates a schedule against a live clock.
package cron

import "encoding/json/jsontext"

// Portable document limits bound shared definition and observation payloads.
const (
	DefinitionLimit = 256 * 1024
	StateLimit      = 16 * 1024
	SummaryLimit    = 64 * 1024
)

// Trigger describes dormant schedule intent for an independent consumer.
type Trigger struct {
	Kind            string `json:"kind"`
	Timezone        string `json:"timezone,omitempty"`
	Cron            string `json:"cron,omitempty"`
	IntervalSeconds int64  `json:"interval_seconds,omitzero"`
	At              string `json:"at,omitempty"`
	IssueUID        string `json:"issue_uid,omitempty"`
	LeadSeconds     int64  `json:"lead_seconds,omitzero"`
}

// Action selects process execution or ordinary issue notification.
type Action struct {
	Kind      string `json:"kind"`
	Prompt    string `json:"prompt,omitempty"`
	FlowUID   string `json:"flow_uid,omitempty"`
	Recipient string `json:"recipient,omitempty"`
	Message   string `json:"message,omitempty"`
}

// IssuePolicy selects an existing issue or a separately created issue per run.
type IssuePolicy struct {
	Kind                   string `json:"kind"`
	UID                    string `json:"uid,omitempty"`
	Title                  string `json:"title,omitempty"`
	Body                   string `json:"body,omitempty"`
	ScheduledOffsetSeconds *int64 `json:"scheduled_offset_seconds,omitempty"`
	DeadlineOffsetSeconds  *int64 `json:"deadline_offset_seconds,omitempty"`
}

// JobDefinition is a portable configuration document for independent clients.
type JobDefinition struct {
	Version        int               `json:"version"`
	Kind           string            `json:"kind"`
	Enabled        bool              `json:"enabled"`
	Trigger        Trigger           `json:"trigger"`
	Action         Action            `json:"action"`
	CheckoutKey    string            `json:"checkout_key,omitempty"`
	Issue          *IssuePolicy      `json:"issue,omitempty"`
	Overlap        string            `json:"overlap"`
	Catchup        string            `json:"catchup"`
	GraceSeconds   int64             `json:"grace_seconds,omitzero"`
	TimeoutSeconds int64             `json:"timeout_seconds,omitzero"`
	SecretRefs     map[string]string `json:"secret_refs,omitempty"`
	Options        jsontext.Value    `json:"options,omitempty"`
}

// FlowStep is a portable command or prompt with earlier-step dependencies.
type FlowStep struct {
	Key     string         `json:"key"`
	Kind    string         `json:"kind"`
	Command string         `json:"command,omitempty"`
	Prompt  string         `json:"prompt,omitempty"`
	After   []string       `json:"after,omitempty"`
	Retries int            `json:"retries,omitzero"`
	Options jsontext.Value `json:"options,omitempty"`
}

// FlowDefinition retains an ordered portable workflow and consumer options.
type FlowDefinition struct {
	About   string         `json:"about,omitempty"`
	Input   string         `json:"input,omitempty"`
	Options jsontext.Value `json:"options,omitempty"`
	Version int            `json:"version"`
	Steps   []FlowStep     `json:"steps"`
}
