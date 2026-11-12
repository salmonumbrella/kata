package tui

import (
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
)

// inputsFormFixture opens the request form for a job that declares one input
// of each type, through R and the resolution answer.
func inputsFormFixture(t *testing.T, width int) (Model, *cronRuntimeTestAPI) {
	t.Helper()
	m, api := runtimeFixture(t, false)
	m.width, m.height = width, 24
	m.cron.data.jobs[0].Definition.Options = map[string]any{"inputs": true}
	lo, hi, maxLength := int64(1), int64(500), int64(200)
	api.inputsRead = cronInputsRead{DefinitionEventUID: api.shown.DefinitionEventUID, InputResolution: cron.InputResolution{
		Inputs: []cron.ResolvedInput{
			{Name: "target", Type: "enum", Title: "Target", Values: []string{"production", "staging"}, Value: "staging", Source: "default"},
			{Name: "batch_size", Type: "int", Title: "Batch size", Required: true, Min: &lo, Max: &hi, Source: "unset", Description: "Rows imported per transaction; larger batches finish sooner but hold locks longer."},
			{Name: "dry_run", Type: "bool", Title: "Dry run", Value: true, Source: "job"},
			{Name: "note", Type: "string", MaxLength: &maxLength, Source: "unset"},
			{Name: "window", Type: "int", Title: "Window", Value: jsontext.Value("30"), Source: "job"},
		},
		Errors: []cron.InputError{{Name: "batch_size", Code: "missing_required", Message: "required input has no value"}},
	}}
	m, cmd := press(m, keyRune('R'))
	return runtimeDone(t, m, cmd), api
}

// The form, not the list, is on screen while the request draft is open.
func TestCronRequestInputsRenderForm(t *testing.T) {
	m, _ := inputsFormFixture(t, 80)
	view := stripANSI(m.View().Content)
	require.Contains(t, view, "Request run · Digest")
	require.Contains(t, view, m.cron.action.target)
	require.Contains(t, view, "▶ Batch size *:")
	require.Contains(t, view, "‹ staging ›")
	require.Contains(t, view, "‹ true ›")
	require.Contains(t, view, "(unset)")
	require.Contains(t, view, "whole number 1 to 500 · required · no value")
	require.Contains(t, view, "Rows imported per transaction")
	require.Contains(t, view, "ctrl+o request run")
	require.NotContains(t, view, "/ search")
}

// One frame per request form state; the no-input confirmation keeps its own goldens.
func TestSnapshot_CronRequestInputs(t *testing.T) {
	defer snapshotInit(t)()
	for _, tc := range []struct {
		state string
		width int
	}{{"loading", 80}, {"form", 80}, {"form", 60}, {"refused", 80}, {"retry", 80}} {
		t.Run(fmt.Sprintf("%s-%d", tc.state, tc.width), func(t *testing.T) {
			var m Model
			if tc.state == "loading" {
				m, _ = runtimeFixture(t, false)
				m.width, m.height = tc.width, 24
				m.cron.data.jobs[0].Definition.Options = map[string]any{"inputs": true}
				m, _ = press(m, keyRune('R'))
			} else {
				var api *cronRuntimeTestAPI
				m, api = inputsFormFixture(t, tc.width)
				if tc.state != "form" {
					m, _ = press(m, keyRune('5'))
					m, _ = press(m, keyRune('0'))
					api.errs = []error{&APIError{Status: http.StatusBadRequest, Code: "validation", Message: "batch_size: invalid input value"}}
					if tc.state == "retry" {
						api.errs = []error{errors.New("connection interrupted")}
					}
					next, cmd := press(m, ctrlKey('o'))
					m = runtimeDone(t, next, cmd)
					if tc.state == "retry" {
						// The minted UID is random; pin the retained one for a stable frame.
						require.NotEmpty(t, m.cron.action.requestUID)
						m.cron.action.requestUID = "01ARZ3NDEKTSV4RRFFQ69G5FB9"
					}
				}
			}
			assertGolden(t, fmt.Sprintf("cron-request-inputs-%s-%d", tc.state, tc.width), m.View().Content)
		})
	}
}
