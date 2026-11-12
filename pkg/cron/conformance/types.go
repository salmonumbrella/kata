// Package conformance checks opt-in cron adapter decisions offline. It neither
// launches commands nor certifies an executor's persistence or process control.
package conformance

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"strings"
	"time"
)

// Schema identifies the frozen portable decision format.
const Schema = "kata.cron.adapter-conformance/v1"

// SchemaV2 adds explicit successful and multi-parent workflow coverage.
const SchemaV2 = "kata.cron.adapter-conformance/v2"
// SchemaV3 adds input-delivery decisions.
const SchemaV3 = "kata.cron.adapter-conformance/v3"
const maxScan = 100000

// maxSeconds is the Go duration representation limit, also used for standalone
// declaration and settlement durations that are not job definitions.
const maxSeconds = int64((1<<63 - 1) / int64(time.Second))

// Declaration selects exact policy choices; zero count and age mean unlimited.
type Declaration struct {
	Name                 string `json:"name"`
	ToleranceSeconds     int64  `json:"tolerance_seconds"`
	CatchupLimit         int    `json:"catchup_limit"`
	CatchupWindowSeconds int64  `json:"catchup_window_seconds"`
	FailurePolicy        string `json:"failure_policy"`
	DropRecords          bool   `json:"drop_records,omitzero"`
	ForbidDeferral       string `json:"forbid_deferral,omitempty"`
}

// UnmarshalJSON preserves the omitted deferral default while rejecting an
// explicitly supplied empty, null or unknown mode before presence is lost.
func (d *Declaration) UnmarshalJSON(raw []byte) error {
	type wire Declaration
	var value wire
	if err := json.Unmarshal(raw, &value, json.RejectUnknownMembers(true)); err != nil {
		return err
	}
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	if supplied, ok := fields["forbid_deferral"]; ok {
		var mode string
		if err := json.Unmarshal(supplied, &mode); err != nil {
			return err
		}
		if mode != "queue" && mode != "drop_after_grace" {
			return fmt.Errorf("supplied forbid_deferral must be queue or drop_after_grace")
		}
	}
	*d = Declaration(value)
	return nil
}

// Validate reports the member whose value is outside the profile's bounds.
func (d Declaration) Validate() error {
	switch {
	case strings.TrimSpace(d.Name) == "":
		return fmt.Errorf("name must be nonblank")
	case d.ToleranceSeconds < 0 || d.ToleranceSeconds > 120:
		return fmt.Errorf("tolerance_seconds must be 0..120")
	case d.CatchupLimit != 0 && d.CatchupLimit < 100:
		return fmt.Errorf("catchup_limit must be 0 or at least 100")
	case d.CatchupWindowSeconds != 0 && (d.CatchupWindowSeconds < 86400 || d.CatchupWindowSeconds > maxSeconds):
		return fmt.Errorf("catchup_window_seconds must be 0 or 86400..%d", maxSeconds)
	case d.FailurePolicy != "continue" && d.FailurePolicy != "stop":
		return fmt.Errorf("failure_policy must be continue or stop")
	case d.ForbidDeferral != "" && d.ForbidDeferral != "queue" && d.ForbidDeferral != "drop_after_grace":
		return fmt.Errorf("forbid_deferral must be queue or drop_after_grace")
	}
	return nil
}

// Job carries a portable full definition and replicated identity.
type Job struct {
	CreatedAt          time.Time      `json:"created_at"`
	DefinitionEventUID string         `json:"definition_event_uid"`
	Definition         jsontext.Value `json:"definition"`
	Deleted            bool           `json:"deleted"`
}

// Checkpoint separates the enumeration horizon from work still awaiting launch.
type Checkpoint struct {
	Through            time.Time   `json:"through"`
	DefinitionEventUID string      `json:"definition_event_uid"`
	Pending            []time.Time `json:"pending"`
}

// PlanInput supplies one scheduling decision, without live reads.
type PlanInput struct {
	Adapter     string      `json:"adapter"`
	Job         Job         `json:"job"`
	InstalledAt time.Time   `json:"installed_at"`
	Now         time.Time   `json:"now"`
	Checkpoint  *Checkpoint `json:"checkpoint"`
	ActiveRuns  int         `json:"active_runs"`
}

// PlanOutput carries ascending launches and directly reusable pending state.
type PlanOutput struct {
	Launch     []time.Time `json:"launch"`
	Checkpoint Checkpoint  `json:"checkpoint"`
	Excluded   string      `json:"excluded"`
}

// WorkflowStep names earlier successful prerequisites and a retry budget.
type WorkflowStep struct {
	Key     string   `json:"key"`
	After   []string `json:"after"`
	Retries int      `json:"retries"`
}

// Attempt offers an outcome; the model decides whether it may be consumed.
type Attempt struct {
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at"`
	Status    string    `json:"status"`
}

// SettleInput describes one run's offered attempts and termination knowledge.
type SettleInput struct {
	Steps                []WorkflowStep       `json:"steps"`
	Outcomes             map[string][]Attempt `json:"outcomes"`
	StartedAt            time.Time            `json:"started_at"`
	Now                  time.Time            `json:"now"`
	TimeoutSeconds       int64                `json:"timeout_seconds"`
	CancelledAt          *time.Time           `json:"cancelled_at"`
	StoppedWithCertainty bool                 `json:"stopped_with_certainty"`
}

// SettleOutput reports consumed attempts, skipped work and local occupancy.
type SettleOutput struct {
	Attempts map[string]int `json:"attempts"`
	Skipped  []string       `json:"skipped"`
	Status   string         `json:"status"`
	Occupied bool           `json:"occupied"`
}

// Decision contains exactly one input. Sequence flags use actual prior outputs.
type Decision struct {
	Inputs *InputDelivery `json:"inputs,omitempty"`
	Plan          *PlanInput     `json:"plan,omitempty"`
	Settle        *SettleInput   `json:"settle,omitempty"`
	UseCheckpoint bool           `json:"use_checkpoint,omitempty"`
	UseOccupancy  bool           `json:"use_occupancy,omitempty"`
	Expect        jsontext.Value `json:"expect"`
}

// Scenario groups an ordered decision sequence.
type Scenario struct {
	Name  string     `json:"name"`
	Steps []Decision `json:"steps"`
}

// FixtureSuite contains policy-specific scenarios.
type FixtureSuite struct {
	Schema      string      `json:"schema"`
	Declaration Declaration `json:"declaration"`
	Scenarios   []Scenario  `json:"scenarios"`
}

// Answer supplies one output for every decision of a named scenario.
type Answer struct {
	Name    string           `json:"name"`
	Outputs []jsontext.Value `json:"outputs"`
}

// Failure names a missing, duplicate, extra or mismatched answer.
type Failure struct {
	Name string `json:"name"`
	Diff string `json:"diff"`
}

// Report contains successful counts and named failures.
type Report struct {
	Schema string    `json:"schema"`
	Passed int       `json:"passed"`
	Failed []Failure `json:"failed"`
}

// Adapter exposes a runtime's pure decision bridge.
type Adapter interface {
	Declare() Declaration
	Plan(context.Context, PlanInput) (PlanOutput, error)
	Settle(context.Context, SettleInput) (SettleOutput, error)
}

// Drop groups ascending unique instants under one current disposition reason.
type Drop struct {
	Reason   string      `json:"reason"`
	Instants []time.Time `json:"instants"`
}

// DropRecorder replaces Plan with one combined call when recording is declared.
type DropRecorder interface {
	PlanWithDrops(context.Context, PlanInput) (PlanOutput, []Drop, error)
}

// InputStep describes execution text without changing persisted definitions.
type InputStep struct {
 Kind string `json:"kind"`
 Command string `json:"command"`
 Argv []string `json:"argv"`
 Prompt string `json:"prompt"`
}

// InputObservation describes one daemon resolution response. Resolution is raw
// JSON so signed integers retain their exact representation.
type InputObservation struct {
 DefinitionEventUID string `json:"definition_event_uid"`
 WorkflowDefinitionEventUID string `json:"workflow_definition_event_uid"`
 Resolution jsontext.Value `json:"resolution"`
}

// InputDelivery supplies definitions and observations to the adapter bridge.
type InputDelivery struct {
 JobOptions jsontext.Value `json:"job_options"`
 WorkflowOptions jsontext.Value `json:"workflow_options"`
 HasWorkflow bool `json:"has_workflow"`
 RequestInputs bool `json:"request_inputs"`
 APIVersion string `json:"api_version"`
 DefinitionEventUID string `json:"definition_event_uid"`
 WorkflowDefinitionEventUID string `json:"workflow_definition_event_uid"`
 Observations []InputObservation `json:"observations"`
 Step InputStep `json:"step"`
}

// InputDeliveryOutput is the observable delivery and resolution call result.
type InputDeliveryOutput struct {
 Calls int `json:"calls"`
 Refreshes int `json:"refreshes"`
 Status string `json:"status"`
 Message string `json:"message"`
 Env map[string]string `json:"env"`
 Command string `json:"command"`
 Argv []string `json:"argv"`
 Prompt string `json:"prompt"`
}

// InputDeliverer is an optional adapter decision bridge for v3 fixtures.
type InputDeliverer interface {
 DeliverInputs(context.Context, InputDelivery) (InputDeliveryOutput, error)
}
