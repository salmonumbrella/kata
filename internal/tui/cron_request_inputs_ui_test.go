package tui

import (
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
)

// inputsFormFixture opens the request form for a job that declares one input
// of each type, through R and the resolution answer.
func inputsFormFixture(t *testing.T, width int) (Model, *cronRuntimeTestAPI) {
	t.Helper()
	m, api := runtimeFixture(t, false)
	m.width, m.height = width, 24
	m.cron.data.jobs[0].Definition.Options = map[string]any{"inputs": map[string]any{"target": map[string]any{"type": "enum", "values": []string{"production", "staging"}}, "batch_size": map[string]any{"type": "int", "required": true}, "dry_run": map[string]any{"type": "bool"}, "note": map[string]any{"type": "string"}, "window": map[string]any{"type": "int"}}}
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
	m, cmd := pressTop(t, m, keyRune('R'))
	return runtimeDone(t, m, cmd), api
}

// Long enum choices that differ only past the value column stay
// distinguishable: the focused field shows the whole selected value.
func TestCronRequestInputsLongEnumShowsFullChoice(t *testing.T) {
	m, api := runtimeFixture(t, false)
	m.width, m.height = 80, 24
	m.cron.data.jobs[0].Definition.Options = map[string]any{"inputs": true}
	choices := []string{
		"copy orders from the primary region into the analytics warehouse and rebuild summary tables",
		"copy orders from the primary region into the analytics warehouse and keep summary tables",
	}
	api.inputsRead = cronInputsRead{DefinitionEventUID: api.shown.DefinitionEventUID, InputResolution: cron.InputResolution{
		Inputs: []cron.ResolvedInput{{Name: "mode", Type: "enum", Title: "Mode", Values: choices, Value: choices[0], Source: "default"}},
	}}
	m, cmd := pressTop(t, m, keyRune('R'))
	m = runtimeDone(t, m, cmd)
	for i := range choices {
		view := strings.Join(strings.Fields(stripANSI(m.View().Content)), " ")
		require.Contains(t, view, choices[i])
		require.NotContains(t, view, choices[1-i])
		m, _ = pressTop(t, m, tea.KeyPressMsg{Code: tea.KeyRight})
	}
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
				m.cron.data.jobs[0].Definition.Options = map[string]any{"inputs": map[string]any{"count": map[string]any{"type": "int"}}}
				m, _ = pressTop(t, m, keyRune('R'))
			} else {
				var api *cronRuntimeTestAPI
				m, api = inputsFormFixture(t, tc.width)
				if tc.state != "form" {
					m, _ = pressTop(t, m, keyRune('5'))
					m, _ = pressTop(t, m, keyRune('0'))
					api.errs = []error{&APIError{Status: http.StatusBadRequest, Code: "validation", Message: "batch_size: invalid input value"}}
					if tc.state == "retry" {
						api.errs = []error{errors.New("connection interrupted")}
					}
					next, cmd := pressTop(t, m, ctrlKey('o'))
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
