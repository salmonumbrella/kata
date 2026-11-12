package tui

import (
	"testing"
	"errors"
	"fmt"
	"strings"
	"net/http"
	"encoding/json/jsontext"

	"go.kenn.io/kata/internal/cron"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"
)

// A declared input must be loaded before asking the operator to request a run.
func TestCronRunRequestOpensInputForm(t *testing.T) {
	m, _ := runtimeFixture(t, false)
	m.cron.data.jobs[0].Definition.Options = map[string]any{"inputs": map[string]any{"count": map[string]any{"type": "int", "required": true}}}
	m, cmd := press(m, keyRune('R'))
	require.Equal(t, modalNone, m.modal)
	require.NotNil(t, cmd)
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
	d.fields[0].text = ""
	values, err = d.freeze()
	require.NoError(t, err)
	require.NotContains(t, values, "count")
}

func TestCronRequestInputDraftRefusesInvalid(t *testing.T) {
	d := newCronRequestInputs(cron.InputResolution{Inputs: []cron.ResolvedInput{{Name: "count", Type: "int", Required: true, Source: "unset"}}})
	_, err := d.freeze()
	require.Error(t, err)
	d.fields[0].set, d.fields[0].text = true, "9223372036854775808"
	_, err = d.freeze()
	require.Error(t, err)
}

func TestCronRequestInputsRefusalAndRetry(t *testing.T) {
	m, api := runtimeFixture(t, false)
	m.cron.data.jobs[0].Definition.Options = map[string]any{"inputs": map[string]any{"count": map[string]any{"type": "int"}}}
	api.inputsRead = cronInputsRead{DefinitionEventUID: api.shown.DefinitionEventUID, InputResolution: cron.InputResolution{Inputs: []cron.ResolvedInput{{Name: "count", Type: "int", Source: "unset"}}}}
	m, cmd := press(m, keyRune('R'))
	m = runtimeDone(t, m, cmd)
	require.Equal(t, 1, api.inputCalls)
	m, _ = press(m, keyRune('7'))
	api.errs = []error{&APIError{Status: http.StatusBadRequest, Message: "refused"}, errors.New("connection lost"), nil}
	m, cmd = press(m, ctrlKey('o'))
	m = runtimeDone(t, m, cmd)
	require.NotNil(t, m.cron.action.inputs)
	require.Empty(t, m.cron.action.requestUID)
	require.Contains(t, m.cron.action.inputs.general, "refused")
	m, cmd = press(m, ctrlKey('o'))
	m = runtimeDone(t, m, cmd)
	require.NotEmpty(t, m.cron.action.requestUID)
	m, _ = press(m, keyRune('8'))
	m, cmd = press(m, ctrlKey('o'))
	m = runtimeDone(t, m, cmd)
	require.Len(t, api.calls, 3)
	require.NotEqual(t, api.calls[0].id, api.calls[1].id)
	require.Equal(t, api.calls[1].id, api.calls[2].id)
	require.Equal(t, api.inputValues[1], api.inputValues[2])
	require.Equal(t, jsontext.Value("7"), api.inputValues[2]["count"])
}

func TestCronRequestInputsResolutionOutcomes(t *testing.T) {
	for _, tc := range []struct { name string; problem cron.InputError; form bool }{
		{"required", cron.InputError{Name: "count", Code: "missing_required", Message: "required"}, true},
		{"invalid field", cron.InputError{Name: "count", Code: "invalid_value", Message: "invalid"}, true},
		{"undeclared", cron.InputError{Name: "other", Code: "undeclared_value", Message: "undeclared"}, false},
		{"declaration", cron.InputError{Code: "declaration_invalid", Message: "invalid"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, api := runtimeFixture(t, false)
			m.cron.data.jobs[0].Definition.Options = map[string]any{"inputs": true}
			api.inputsRead = cronInputsRead{DefinitionEventUID: api.shown.DefinitionEventUID, InputResolution: cron.InputResolution{Inputs: []cron.ResolvedInput{{Name: "count", Type: "int", Source: "unset"}}, Errors: []cron.InputError{tc.problem}}}
			m, cmd := press(m, keyRune('R'))
			m = runtimeDone(t, m, cmd)
			if tc.form { require.NotNil(t, m.cron.action.inputs) } else { require.Nil(t, m.cron.action); require.Contains(t, m.toast.text, "fix the definition") }
		})
	}
}

func TestCronRequestInputsCompatibilityAndStaleRead(t *testing.T) {
	m, api := runtimeFixture(t, false)
	m = runtimeDialog(t, m, 'R')
	require.Zero(t, api.inputCalls)
	m = m.dropCronAction()
	m.cron.data.jobs[0].Definition.Options = map[string]any{"inputs": true}
	api.inputsErr = errCronInputsUnsupported
	m, cmd := press(m, keyRune('R'))
	m = runtimeDone(t, m, cmd)
	require.Equal(t, modalCronAction, m.modal)
	m = m.dropCronAction()
	m, cmd = press(m, keyRune('R'))
	m, _ = press(m, tea.KeyPressMsg{Code: tea.KeyEscape})
	m = runtimeDone(t, m, cmd)
	require.Nil(t, m.cron.action)
}

func TestCronRequestInputsChangedFieldsAndLimit(t *testing.T) {
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
	for i := range inputs { inputs[i] = cron.ResolvedInput{Name: fmt.Sprintf("field%d", i), Type: "string", MaxLength: &maxLength, Source: "unset"} }
	d = newCronRequestInputs(cron.InputResolution{Inputs: inputs})
	for i := range d.fields { d.fields[i].set = true; d.fields[i].text = strings.Repeat("x", 2200) }
	_, err = d.freeze()
	require.ErrorContains(t, err, "4 KiB")
}

func TestCronRequestInputsChangedJobAndEditor(t *testing.T) {
	m, api := runtimeFixture(t, false)
	m.cron.data.jobs[0].Definition.Options = map[string]any{"inputs": true}
	api.inputsRead = cronInputsRead{DefinitionEventUID: "different", InputResolution: cron.InputResolution{Inputs: []cron.ResolvedInput{{Name: "note", Type: "string", Multiline: true}}}}
	m, cmd := press(m, keyRune('R'))
	m = runtimeDone(t, m, cmd)
	require.Nil(t, m.cron.action)
	require.Contains(t, m.toast.text, "changed")
	api.inputsRead.DefinitionEventUID = api.shown.DefinitionEventUID
	m, cmd = press(m, keyRune('R'))
	m = runtimeDone(t, m, cmd)
	d := m.cron.action.inputs
	m, _ = m.routeEditorReturn(editorReturnedMsg{formGen: d.formGen, content: "first\nsecond"})
	values, err := d.freeze()
	require.NoError(t, err)
	require.Equal(t, jsontext.Value(`"first\nsecond"`), values["note"])
	m.cron.data.jobs[0].DefinitionEventUID = "changed"
	m, cmd = press(m, ctrlKey('o'))
	require.Nil(t, cmd)
	require.Nil(t, m.cron.action)
	require.Contains(t, m.toast.text, "changed")
}
