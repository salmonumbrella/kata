package db

import (
	"fmt"
	"strings"

	"go.kenn.io/kata/internal/uid"
)

func cronUID(value string) bool { return uid.Valid(value) && value == strings.ToUpper(value) }

// ValidateCronRecord checks one backup row before either backend restores it.
func ValidateCronRecord(record ImportRecord) error {
	switch value := record.(type) {
	case *CronJobExport:
		if value == nil {
			return fmt.Errorf("nil cron job")
		}
		if err := validateCronDefinition(value.CronDefinition); err != nil {
			return err
		}
		return value.Definition.Validate()
	case *CronWorkflowExport:
		if value == nil {
			return fmt.Errorf("nil cron workflow")
		}
		if err := validateCronDefinition(value.CronDefinition); err != nil {
			return err
		}
		return value.Definition.Validate()
	case *CronRunExport:
		if value == nil {
			return fmt.Errorf("nil cron run")
		}
		if value.ID <= 0 || value.Revision < 1 || value.CreatedAt.IsZero() || value.UpdatedAt.IsZero() {
			return fmt.Errorf("invalid cron observation history")
		}
		return validateCronRun(CronRun(*value))
	default:
		return fmt.Errorf("unknown cron record %T", record)
	}
}
func validateCronDefinition(value CronDefinition) error {
	if value.ID <= 0 || value.ProjectID <= 0 || !cronUID(value.UID) || !cronUID(value.DefinitionEventUID) || value.Revision < 1 || strings.TrimSpace(value.Name) == "" || len(value.Name) > 256 || strings.TrimSpace(value.Author) == "" || value.CreatedAt.IsZero() || value.UpdatedAt.IsZero() {
		return fmt.Errorf("invalid cron definition identity")
	}
	return value.DefinitionHLC.Validate()
}
