package tui

import (
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"go.kenn.io/kata/internal/cron"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"
)

// pressTop routes a key through the real top-level dispatcher, which owns the
// open request form, and keeps the command it returns.
func pressTop(t *testing.T, m Model, msg tea.KeyPressMsg) (Model, tea.Cmd) {
	t.Helper()
	next, cmd, handled := m.routeTopLevel(msg)
	require.True(t, handled)
	return next.(Model), cmd
}

func TestCronRequestInputDraftValues(t *testing.T) {
	resolution := cron.InputResolution{Inputs: []cron.ResolvedInput{
		{Name: "count", Type: "int", Source: "unset"},
		{Name: "flag", Type: "bool", Source: "default", Value: true},
		{Name: "note", Type: "string", Source: "unset"},
	}}
	d := newCronRequestInputs(resolution)
	d.fields[0].set, d.fields[0].text = true, "9223372036854775807"
	d.fields[1].set, d.fields[1].text = true, "false"
	d.fields[2].set, d.fields[2].text = true, ""
	values, err := d.freeze()
	require.NoError(t, err)
	require.Equal(t, map[string]jsontext.Value{"count": []byte("9223372036854775807"), "flag": []byte("false"), "note": []byte(`""`)}, values)
}

func TestCronRequestInputsRefusalAndRetry(t *testing.T) {
	m, api := runtimeFixture(t, false)
	m.cron.data.jobs[0].Definition.Options = map[string]any{"inputs": map[string]any{"count": map[string]any{"type": "int"}}}
	api.inputsRead = cronInputsRead{DefinitionEventUID: api.shown.DefinitionEventUID, InputResolution: cron.InputResolution{Inputs: []cron.ResolvedInput{{Name: "count", Type: "int", Source: "unset"}}}}
	m, cmd := pressTop(t, m, keyRune('R'))
	m = runtimeDone(t, m, cmd)
	require.Equal(t, 1, api.inputCalls)
	m, _ = pressTop(t, m, keyRune('7'))
	api.errs = []error{&APIError{Status: http.StatusBadRequest, Message: "refused"}, errors.New("connection lost"), nil}
	m, cmd = pressTop(t, m, ctrlKey('o'))
	m = runtimeDone(t, m, cmd)
	require.NotNil(t, m.cron.action.inputs)
	require.Empty(t, m.cron.action.requestUID)
	require.Contains(t, m.cron.action.inputs.general, "refused")
	m, cmd = pressTop(t, m, ctrlKey('o'))
	m = runtimeDone(t, m, cmd)
	require.NotEmpty(t, m.cron.action.requestUID)
	m, _ = pressTop(t, m, keyRune('8'))
	m, cmd = pressTop(t, m, ctrlKey('o'))
	m = runtimeDone(t, m, cmd)
	require.Len(t, api.calls, 3)
	require.NotEqual(t, api.calls[0].id, api.calls[1].id)
	require.Equal(t, api.calls[1].id, api.calls[2].id)
	require.Equal(t, api.inputValues[1], api.inputValues[2])
	require.Equal(t, jsontext.Value("7"), api.inputValues[2]["count"])
}

func TestCronRequestInputsResolutionOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		problem cron.InputError
		form    bool
	}{
		{"required", cron.InputError{Name: "count", Code: "missing_required", Message: "required"}, true},
		{"invalid field", cron.InputError{Name: "count", Code: "invalid_value", Message: "invalid"}, true},
		{"undeclared", cron.InputError{Name: "other", Code: "undeclared_value", Message: "undeclared"}, false},
		{"declaration", cron.InputError{Code: "declaration_invalid", Message: "invalid"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, api := runtimeFixture(t, false)
			m.cron.data.jobs[0].Definition.Options = map[string]any{"inputs": map[string]any{"count": map[string]any{"type": "int"}}}
			api.inputsRead = cronInputsRead{DefinitionEventUID: api.shown.DefinitionEventUID, InputResolution: cron.InputResolution{Inputs: []cron.ResolvedInput{{Name: "count", Type: "int", Required: tc.name == "required", Source: "unset"}}, Errors: []cron.InputError{tc.problem}}}
			m, cmd := pressTop(t, m, keyRune('R'))
			m = runtimeDone(t, m, cmd)
			if tc.form {
				require.NotNil(t, m.cron.action.inputs)
			} else {
				require.Nil(t, m.cron.action)
				require.Contains(t, m.toast.text, "fix the definition")
			}
		})
	}
}

func TestCronRequestInputsCompatibilityAndStaleRead(t *testing.T) {
	m, api := runtimeFixture(t, false)
	m = runtimeDialog(t, m, 'R')
	require.Zero(t, api.inputCalls)
	m = m.dropCronAction()
	m.cron.data.jobs[0].Definition.Options = map[string]any{"inputs": map[string]any{"count": map[string]any{"type": "int"}}}
	api.inputsErr = errCronInputsUnsupported
	m, cmd := pressTop(t, m, keyRune('R'))
	m = runtimeDone(t, m, cmd)
	require.Equal(t, modalCronAction, m.modal)
	m = m.dropCronAction()
	m, cmd = pressTop(t, m, keyRune('R'))
	m, _ = pressTop(t, m, tea.KeyPressMsg{Code: tea.KeyEscape})
	m = runtimeDone(t, m, cmd)
	require.Nil(t, m.cron.action)
}

func TestCronRequestInputsChangedFieldsAndLimit(t *testing.T) {
	invalid := newCronRequestInputs(cron.InputResolution{Inputs: []cron.ResolvedInput{{Name: "count", Type: "int", Required: true, Source: "unset"}}})
	_, err := invalid.freeze()
	require.Error(t, err)
	invalid.fields[0].set, invalid.fields[0].text = true, "9223372036854775808"
	_, err = invalid.freeze()
	require.Error(t, err)
	d := newCronRequestInputs(cron.InputResolution{Inputs: []cron.ResolvedInput{
		{Name: "count", Type: "int", Source: "job", Value: jsontext.Value("42")},
		{Name: "note", Type: "string", Source: "default", Value: "unchanged"},
	}})
	values, err := d.freeze()
	require.NoError(t, err)
	require.Nil(t, values)
	d.fields[0].text = ""
	_, err = d.freeze()
	require.Error(t, err, "a prefilled integer cannot be cleared")

	maxLength := int64(4096)
	inputs := make([]cron.ResolvedInput, 2)
	for i := range inputs {
		inputs[i] = cron.ResolvedInput{Name: fmt.Sprintf("field%d", i), Type: "string", MaxLength: &maxLength, Source: "unset"}
	}
	d = newCronRequestInputs(cron.InputResolution{Inputs: inputs})
	for i := range d.fields {
		d.fields[i].set = true
		d.fields[i].text = strings.Repeat("x", 2200)
	}
	_, err = d.freeze()
	require.ErrorContains(t, err, "4 KiB")
}

func TestCronRequestInputsChangedJob(t *testing.T) {
	for _, tc := range []struct{ name, want string }{
		{"read mismatch", "cron_conflict: The job changed; refresh before retrying"},
		{"local while loading", "The job changed; press R to reload its inputs"},
		{"local before submit", "The job changed; press R to reload its inputs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, api := runtimeFixture(t, false)
			m.cron.data.jobs[0].Definition.Options = map[string]any{"inputs": map[string]any{"note": map[string]any{"type": "string"}}}
			api.inputsRead = cronInputsRead{DefinitionEventUID: api.shown.DefinitionEventUID, InputResolution: cron.InputResolution{Inputs: []cron.ResolvedInput{{Name: "note", Type: "string", Multiline: true}}}}
			if tc.name == "read mismatch" {
				api.inputsRead.DefinitionEventUID = "different"
			}
			m, cmd := pressTop(t, m, keyRune('R'))
			if tc.name == "local while loading" {
				m.cron.data.jobs[0].DefinitionEventUID = "changed"
			}
			m = runtimeDone(t, m, cmd)
			if tc.name == "local before submit" {
				m.cron.data.jobs[0].DefinitionEventUID = "changed"
				m, _ = pressTop(t, m, ctrlKey('o'))
			}
			require.Empty(t, api.calls)
			require.Nil(t, m.cron.action)
			require.Equal(t, tc.want, m.toast.text)
		})
	}
}

func TestCronRequestInputsEditorReturn(t *testing.T) {
	m, api := runtimeFixture(t, false)
	m.cron.data.jobs[0].Definition.Options = map[string]any{"inputs": map[string]any{"note": map[string]any{"type": "string"}}}
	api.inputsRead = cronInputsRead{DefinitionEventUID: api.shown.DefinitionEventUID, InputResolution: cron.InputResolution{Inputs: []cron.ResolvedInput{{Name: "note", Type: "string", Multiline: true}}}}
	m, cmd := pressTop(t, m, keyRune('R'))
	m = runtimeDone(t, m, cmd)
	d := m.cron.action.inputs
	m, _ = m.routeEditorReturn(editorReturnedMsg{formGen: d.formGen, content: "first\nsecond"})
	values, err := d.freeze()
	require.NoError(t, err)
	require.Equal(t, jsontext.Value(`"first\nsecond"`), values["note"])
}

func TestCronRequestInputsOptionalIntegerCleared(t *testing.T) {
	m, api := runtimeFixture(t, false)
	m.cron.data.jobs[0].Definition.Options = map[string]any{"inputs": map[string]any{"count": map[string]any{"type": "int"}}}
	api.inputsRead = cronInputsRead{DefinitionEventUID: api.shown.DefinitionEventUID, InputResolution: cron.InputResolution{Inputs: []cron.ResolvedInput{{Name: "count", Type: "int", Source: "unset"}}}}
	m, cmd := pressTop(t, m, keyRune('R'))
	m = runtimeDone(t, m, cmd)
	m, _ = pressTop(t, m, keyRune('7'))
	m, _ = pressTop(t, m, tea.KeyPressMsg{Code: tea.KeyBackspace})
	values, err := m.cron.action.inputs.freeze()
	require.NoError(t, err)
	require.NotContains(t, values, "count")
}

func TestCronRequestInputsValidationUpdatesWhileEditing(t *testing.T) {
	m, api := runtimeFixture(t, false)
	m.cron.data.jobs[0].Definition.Options = map[string]any{"inputs": map[string]any{"count": map[string]any{"type": "int"}}}
	api.inputsRead = cronInputsRead{DefinitionEventUID: api.shown.DefinitionEventUID, InputResolution: cron.InputResolution{Inputs: []cron.ResolvedInput{{Name: "count", Type: "int", Required: true, Source: "unset"}}, Errors: []cron.InputError{{Name: "count", Code: "missing_required"}}}}
	m, cmd := pressTop(t, m, keyRune('R'))
	m = runtimeDone(t, m, cmd)
	require.Contains(t, m.cron.action.inputs.general, "count")
	m, _ = pressTop(t, m, keyRune('7'))
	require.Empty(t, m.cron.action.inputs.general)
	m, _ = pressTop(t, m, ctrlKey('r'))
	require.Contains(t, m.cron.action.inputs.general, "count")
}

func TestCronRequestInputsChoiceAndReset(t *testing.T) {
	m, api := runtimeFixture(t, false)
	m.cron.data.jobs[0].Definition.Options = map[string]any{"inputs": map[string]any{"flag": map[string]any{"type": "bool"}}}
	api.inputsRead = cronInputsRead{DefinitionEventUID: api.shown.DefinitionEventUID, InputResolution: cron.InputResolution{Inputs: []cron.ResolvedInput{{Name: "flag", Type: "bool", Source: "default", Value: true}}}}
	m, cmd := pressTop(t, m, keyRune('R'))
	m = runtimeDone(t, m, cmd)
	m, _ = pressTop(t, m, tea.KeyPressMsg{Code: tea.KeyRight})
	values, err := m.cron.action.inputs.freeze()
	require.NoError(t, err)
	require.Equal(t, jsontext.Value("false"), values["flag"])
	m, _ = pressTop(t, m, ctrlKey('r'))
	values, err = m.cron.action.inputs.freeze()
	require.NoError(t, err)
	require.Nil(t, values)
}

func TestCronRequestInputsInvalidJobValueNeedsOverride(t *testing.T) {
	d := newCronRequestInputs(cron.InputResolution{Inputs: []cron.ResolvedInput{{Name: "target", Type: "enum", Values: []string{"staging", "production"}, Source: "job"}}, Errors: []cron.InputError{{Name: "target", Code: "invalid_value"}}})
	_, err := d.freeze()
	require.Error(t, err)
	d.step(1)
	values, err := d.freeze()
	require.NoError(t, err)
	require.Equal(t, jsontext.Value(`"staging"`), values["target"])
}

func TestCronRequestInputsMouseKeepsDraft(t *testing.T) {
	m, api := runtimeFixture(t, false)
	m.opts.Mouse = true
	m.width = 80
	m.cron.data.jobs[0].Definition.Options = map[string]any{"inputs": map[string]any{"count": map[string]any{"type": "int"}}}
	api.inputsRead = cronInputsRead{DefinitionEventUID: api.shown.DefinitionEventUID, InputResolution: cron.InputResolution{Inputs: []cron.ResolvedInput{{Name: "count", Type: "int", Source: "unset"}}}}
	m, cmd := pressTop(t, m, keyRune('R'))
	m = runtimeDone(t, m, cmd)
	m, _ = pressTop(t, m, keyRune('7'))
	next, _, handled := m.routeTopLevel(tea.MouseClickMsg{X: 1, Y: 2, Button: tea.MouseLeft})
	require.True(t, handled)
	m = next.(Model)
	require.False(t, m.cron.filtering)
	m, cmd = pressTop(t, m, ctrlKey('o'))
	m = runtimeDone(t, m, cmd)
	require.Len(t, api.calls, 1)
	require.Equal(t, jsontext.Value("7"), api.inputValues[0]["count"])
}

func TestCronRequestInputsUnreadableLiveCheckKeepsDraft(t *testing.T) {
	for _, readErr := range []error{errors.New("connection lost"), &APIError{Status: http.StatusServiceUnavailable, Message: "unavailable"}} {
		t.Run(readErr.Error(), func(t *testing.T) {
			m, api := runtimeFixture(t, false)
			m.cron.data.jobs[0].Definition.Options = map[string]any{"inputs": map[string]any{"count": map[string]any{"type": "int"}}}
			api.inputsRead = cronInputsRead{DefinitionEventUID: api.shown.DefinitionEventUID, InputResolution: cron.InputResolution{Inputs: []cron.ResolvedInput{{Name: "count", Type: "int", Source: "unset"}}}}
			m, cmd := pressTop(t, m, keyRune('R'))
			m = runtimeDone(t, m, cmd)
			m, _ = pressTop(t, m, keyRune('7'))
			api.showErr = readErr
			m, cmd = pressTop(t, m, ctrlKey('o'))
			m = runtimeDone(t, m, cmd)
			require.NotNil(t, m.cron.action)
			require.Empty(t, m.cron.action.requestUID)
			require.False(t, m.cron.action.inflight)
			require.NotContains(t, m.cron.action.inputs.general, "Request refused")
			require.Empty(t, api.calls)
			m, _ = pressTop(t, m, keyRune('8'))
			api.showErr = nil
			m, cmd = pressTop(t, m, ctrlKey('o'))
			m = runtimeDone(t, m, cmd)
			require.Equal(t, jsontext.Value("78"), api.inputValues[0]["count"])
		})
	}
}

func TestCronRequestInputsUnnamedDiagnostic(t *testing.T) {
	m, api := runtimeFixture(t, false)
	m.cron.data.jobs[0].Definition.Options = map[string]any{"inputs": map[string]any{"count": map[string]any{"type": "int"}}}
	api.inputsRead = cronInputsRead{DefinitionEventUID: api.shown.DefinitionEventUID, InputResolution: cron.InputResolution{Errors: []cron.InputError{{Code: "declaration_invalid", Message: "invalid declaration"}}}}
	m, cmd := pressTop(t, m, keyRune('R'))
	m = runtimeDone(t, m, cmd)
	require.NotContains(t, m.toast.text, ": : ")
	require.Contains(t, m.toast.text, ": invalid declaration; fix the definition")
}
