package db

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/kata/internal/cron"
)

// CronDefinitionEvent is a complete portable document. Snapshot envelopes
// carry the original winner separately; their new envelope clock is not an edit.
type CronDefinitionEvent struct {
	UID                string             `json:"uid"`
	ProjectUID         string             `json:"project_uid"`
	Name               string             `json:"name"`
	Definition         jsontext.Value     `json:"definition"`
	Author             string             `json:"author"`
	CreatedAt          time.Time          `json:"created_at"`
	UpdatedAt          time.Time          `json:"updated_at"`
	DeletedAt          *time.Time         `json:"deleted_at,omitempty"`
	DefinitionEventUID string             `json:"definition_event_uid,omitempty"`
	DefinitionHLC      *CronDefinitionHLC `json:"definition_hlc,omitempty"`
}

func isCronDefinitionEvent(kind string) bool {
	switch kind {
	case "cron.job.created", "cron.job.updated", "cron.job.deleted", "cron.job.restored", "cron.job.snapshot",
		"cron.workflow.created", "cron.workflow.updated", "cron.workflow.deleted", "cron.workflow.restored", "cron.workflow.snapshot":
		return true
	}
	return false
}

func cronDefinitionClock(value CronDefinition) FoldClock {
	return FoldClock{HLCPhysicalMS: value.DefinitionHLC.PhysicalMS, HLCCounter: value.DefinitionHLC.Counter, OriginInstanceUID: value.DefinitionHLC.OriginInstanceUID, EventUID: value.DefinitionEventUID}
}

func parseCronDefinitionEvent(e FoldEvent) (CronDefinitionEvent, CronDefinition, error) {
	var in CronDefinitionEvent
	if err := json.Unmarshal(e.Payload, &in, json.RejectUnknownMembers(true)); err != nil {
		return in, CronDefinition{}, err
	}
	clock := CronDefinitionHLC{Version: 1, PhysicalMS: e.HLCPhysicalMS, Counter: e.HLCCounter, OriginInstanceUID: e.OriginInstanceUID}
	winner := e.UID
	if strings.HasSuffix(e.Type, ".snapshot") {
		if in.DefinitionHLC == nil || !cronUID(in.DefinitionEventUID) {
			return in, CronDefinition{}, fmt.Errorf("snapshot requires original definition clock")
		}
		clock = *in.DefinitionHLC
		winner = in.DefinitionEventUID
	} else if in.DefinitionHLC != nil || in.DefinitionEventUID != "" {
		return in, CronDefinition{}, fmt.Errorf("definition edit cannot override envelope clock")
	}
	value := CronDefinition{ID: 1, ProjectID: 1, UID: in.UID, Name: in.Name, Author: in.Author, DefinitionEventUID: winner, DefinitionHLC: clock, Revision: 1, CreatedAt: in.CreatedAt, UpdatedAt: in.UpdatedAt, DeletedAt: in.DeletedAt}
	if err := validateCronDefinition(value); err != nil {
		return in, value, err
	}
	if !cronUID(in.ProjectUID) || (e.ProjectUID != "" && in.ProjectUID != e.ProjectUID) {
		return in, value, fmt.Errorf("cron project identity mismatch")
	}
	if strings.HasSuffix(e.Type, ".deleted") && in.DeletedAt == nil {
		return in, value, fmt.Errorf("deletion requires tombstone")
	}
	if (strings.HasSuffix(e.Type, ".created") || strings.HasSuffix(e.Type, ".restored") || strings.HasSuffix(e.Type, ".updated")) && in.DeletedAt != nil {
		return in, value, fmt.Errorf("live definition contains tombstone")
	}
	value.ID = 0
	value.ProjectID = 0
	return in, value, nil
}

func (p *FoldProjection) applyCronDefinition(e FoldEvent) {
	in, value, err := parseCronDefinitionEvent(e)
	if err != nil {
		p.Warnings = append(p.Warnings, fmt.Sprintf("invalid cron event %s: %v", e.UID, err))
		return
	}
	if strings.HasPrefix(e.Type, "cron.job.") {
		definition, err := cron.ParseJob(in.Definition)
		if err != nil {
			p.Warnings = append(p.Warnings, err.Error())
			return
		}
		current, exists := p.CronJobs[in.UID]
		if !exists || compareClock(cronDefinitionClock(value), cronDefinitionClock(current.CronDefinition)) > 0 {
			p.CronJobs[in.UID] = FoldCronJob{CronDefinition: value, Definition: definition, ProjectUID: in.ProjectUID}
		}
	} else {
		definition, err := cron.ParseWorkflow(in.Definition)
		if err != nil {
			p.Warnings = append(p.Warnings, err.Error())
			return
		}
		current, exists := p.CronWorkflows[in.UID]
		if !exists || compareClock(cronDefinitionClock(value), cronDefinitionClock(current.CronDefinition)) > 0 {
			p.CronWorkflows[in.UID] = FoldCronWorkflow{CronDefinition: value, Definition: definition, ProjectUID: in.ProjectUID}
		}
	}
}

// ValidateCronFederationEvent checks the complete portable document;
// referenced definitions may be historical and are materialized separately.
func ValidateCronFederationEvent(event RemoteEvent) error {
	if !cronUID(event.EventUID) || !cronUID(event.OriginInstanceUID) || !cronUID(event.ProjectUID) || strings.TrimSpace(event.Actor) == "" || event.HLCPhysicalMS <= 0 || event.HLCCounter < 0 {
		return fmt.Errorf("%w: invalid cron envelope", ErrFederationIngestValidation)
	}
	if event.Type == "cron.run.observed" || event.Type == "cron.run.snapshot" {
		_, err := parseCronRunObservation(FoldEvent{UID: event.EventUID, ProjectUID: event.ProjectUID, OriginInstanceUID: event.OriginInstanceUID, HLCPhysicalMS: event.HLCPhysicalMS, HLCCounter: event.HLCCounter, Payload: event.Payload, Type: event.Type})
		if err != nil {
			return fmt.Errorf("%w: %v", ErrFederationIngestValidation, err)
		}
		return nil
	}

	if !isCronDefinitionEvent(event.Type) {
		return fmt.Errorf("%w: unsupported cron event %s", ErrFederationIngestValidation, event.Type)
	}
	in, _, err := parseCronDefinitionEvent(FoldEvent{UID: event.EventUID, ProjectUID: event.ProjectUID, OriginInstanceUID: event.OriginInstanceUID, HLCPhysicalMS: event.HLCPhysicalMS, HLCCounter: event.HLCCounter, Payload: event.Payload, Type: event.Type})
	if err == nil {
		if strings.HasPrefix(event.Type, "cron.job.") {
			_, err = cron.ParseJob(in.Definition)
		} else {
			_, err = cron.ParseWorkflow(in.Definition)
		}
	}
	if err != nil {
		return fmt.Errorf("%w: %v", ErrFederationIngestValidation, err)
	}
	return nil
}
