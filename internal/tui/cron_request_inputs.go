package tui

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strconv"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/metadata"
)

// cronRequestInputs belongs to the pending action. The visual layer can read
// fields, focus, loading, general and the action's inflight/err/requestUID.
type cronRequestInputs struct {
	loading bool
	fields  []cronRequestInputField
	focus   int
	ed      inputState
	edInit  string
	formGen int64
	general string
}
type cronRequestInputField struct {
	input                       cron.ResolvedInput
	text, prefill               string
	set, prefillSet, jobInvalid bool
}

type cronInputsMsg struct {
	action cronPending
	read   cronInputsRead
	err    error
}

func newCronRequestInputs(r cron.InputResolution) *cronRequestInputs {
	d := &cronRequestInputs{}
	slices.SortFunc(r.Inputs, func(a, b cron.ResolvedInput) int { return stringsCompare(a.Name, b.Name) })
	for _, in := range r.Inputs {
		f := cronRequestInputField{input: in, set: in.Value != nil}
		if f.set {
			switch v := in.Value.(type) {
			case string:
				f.text = v
			case bool:
				f.text = strconv.FormatBool(v)
			case jsontext.Value:
				f.text = string(v)
			}
		}
		f.prefill, f.prefillSet = f.text, f.set
		for _, problem := range r.Errors {
			if problem.Code == "invalid_value" && problem.Name == in.Name {
				f.jobInvalid = true
			}
		}
		d.fields = append(d.fields, f)
	}
	if len(d.fields) > 0 {
		d.focusField(0)
	}
	return d
}

func stringsCompare(a, b string) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}
func (d *cronRequestInputs) choice() bool {
	return d.fields[d.focus].input.Type == "bool" || d.fields[d.focus].input.Type == "enum"
}
func (d *cronRequestInputs) externalText() bool {
	f := d.fields[d.focus]
	return f.input.Type == "string" && f.input.Multiline
}
func (d *cronRequestInputs) syncEditor() {
	if len(d.fields) == 0 || d.choice() || d.externalText() || d.ed.activeField() == nil {
		return
	}
	v := d.ed.activeField().value()
	if v != d.edInit {
		f := &d.fields[d.focus]
		f.text, f.set = v, true
		if f.input.Type == "int" && !f.prefillSet && v == "" {
			f.set = false
		}
		d.edInit = v
	}
}
func (d *cronRequestInputs) focusField(i int) {
	d.syncEditor()
	d.focus = i
	f := d.fields[i]
	ed := inputField{kind: fieldSingleLine, input: textinput.New()}
	ed.input.Prompt = ""
	ed.setValue(f.text)
	ed.input.CursorEnd()
	_ = ed.focus()
	d.ed = inputState{fields: []inputField{ed}}
	d.edInit = f.text
}
func (d *cronRequestInputs) step(delta int) {
	f := &d.fields[d.focus]
	choices := f.input.Values
	if f.input.Type == "bool" {
		choices = []string{"true", "false"}
	}
	if len(choices) == 0 {
		return
	}
	if !f.set {
		if delta < 0 {
			f.text = choices[len(choices)-1]
		} else {
			f.text = choices[0]
		}
		f.set = true
		return
	}
	i := slices.Index(choices, f.text)
	f.text = choices[(i+delta+len(choices))%len(choices)]
}
func (d *cronRequestInputs) freeze() (map[string]jsontext.Value, error) {
	return d.values(true)
}
func (d *cronRequestInputs) validate() {
	_, err := d.values(false)
	d.general = ""
	if err != nil { d.general = err.Error() }
}
func (d *cronRequestInputs) values(focusError bool) (map[string]jsontext.Value, error) {
	d.syncEditor()
	values := map[string]jsontext.Value{}
	for i, f := range d.fields {
		if f.input.Type == "int" && !f.prefillSet && f.text == "" {
			f.set = false
		}
		if !f.set {
			if f.input.Required || f.jobInvalid {
				if focusError { d.focusField(i) }
				return nil, fmt.Errorf("%s: required or invalid job value", f.input.Name)
			}
			continue
		}
		var v any
		switch f.input.Type {
		case "int":
			n, err := strconv.ParseInt(f.text, 10, 64)
			if err != nil {
				if focusError { d.focusField(i) }
				return nil, fmt.Errorf("%s: invalid whole number", f.input.Name)
			}
			v = jsontext.Value(strconv.FormatInt(n, 10))
		case "bool":
			if f.text != "true" && f.text != "false" {
				if focusError { d.focusField(i) }
				return nil, fmt.Errorf("%s: invalid boolean", f.input.Name)
			}
			v = f.text == "true"
		default:
			v = f.text
		}
		if !cron.ValidInputValue(f.input, v) {
			if focusError { d.focusField(i) }
			return nil, fmt.Errorf("%s: invalid input value", f.input.Name)
		}
		if f.jobInvalid && f.text == f.prefill && f.set == f.prefillSet {
			if focusError { d.focusField(i) }
			return nil, fmt.Errorf("%s: job value is invalid", f.input.Name)
		}
		if f.set != f.prefillSet || f.text != f.prefill {
			raw, err := json.Marshal(v)
			if err != nil {
				return nil, err
			}
			values[f.input.Name] = raw
		}
	}
	if len(values) == 0 {
		return nil, nil
	}
	raw, err := json.Marshal(values, json.Deterministic(true))
	if err != nil {
		return nil, err
	}
	if len(metadata.NormalizeJSON(raw)) > 4096 {
		return nil, errors.New("inputs exceed 4 KiB")
	}
	return values, nil
}

func (m Model) readCronRequestInputs(a cronPending) tea.Cmd {
	api, pid := m.api, m.scope.projectID
	return func() tea.Msg {
		// Bubble Tea supplies no request context for this command.
		ctx, cancel := context.WithTimeout(context.Background(), defaultHTTPTimeout)
		defer cancel()
		read, err := api.CronJobInputs(ctx, pid, a.target)
		return cronInputsMsg{action: a, read: read, err: err}
	}
}
func (m Model) cronInputsAnswered(msg cronInputsMsg) (Model, tea.Cmd) {
	if !m.sameCronAction(msg.action) {
		return m, nil
	}
	if next, cmd, ok := m.recheckCronAction(msg.action); !ok {
		return next, cmd
	}
	if errors.Is(msg.err, errCronInputsUnsupported) {
		m.cron.action.inputs = nil
		m.modal = modalCronAction
		return m, nil
	}
	if msg.err != nil {
		return m.dropCronAction().undoNotice("Could not read inputs: "+msg.err.Error(), toastError)
	}
	if msg.read.DefinitionEventUID != msg.action.expected {
		return m.dropCronAction().undoNotice(cronChanged("job").Error(), toastError)
	}
	for _, e := range msg.read.Errors {
		declared := false
		for _, f := range msg.read.Inputs {
			if f.Name == e.Name {
				declared = true
				break
			}
		}
		if !((e.Code == "missing_required" || e.Code == "invalid_value") && e.Name != "" && declared) {
			return m.dropCronAction().undoNotice("Cannot request "+msg.action.name+": "+e.Name+": "+e.Message+"; fix the definition", toastError)
		}
	}
	if len(msg.read.Inputs) == 0 {
		m.cron.action.inputs = nil
		m.modal = modalCronAction
		return m, nil
	}
	d := newCronRequestInputs(msg.read.InputResolution)
	d.validate()
	m.nextFormGen++
	d.formGen = m.nextFormGen
	m.cron.action.inputs = d
	return m, nil
}
func (m Model) routeCronRequestInputsKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	a := m.cron.action
	d := a.inputs
	key := msg.String()
	if key == "ctrl+c" {
		return m, tea.Quit
	}
	if a.inflight {
		return m, nil
	}
	if key == "esc" {
		return m.dropCronAction(), nil
	}
	if d.loading {
		return m, nil
	}
	if key == "ctrl+o" || key == "ctrl+s" {
		if !m.sameCronAction(*a) {
			return m.dropCronAction(), nil
		}
		if next, cmd, ok := m.recheckCronAction(*a); !ok {
			return next, cmd
		}
		sent := *a
		if sent.requestUID == "" {
			values, err := d.freeze()
			if err != nil {
				d.general = err.Error()
				return m, nil
			}
			sent.values = values
		}
		sent.inflight, sent.err, d.general = true, nil, ""
		m.cron.action = &sent
		return m, m.sendCronAction(sent)
	}
	if a.requestUID != "" {
		return m, nil
	}
	if key == "ctrl+r" {
		d.syncEditor()
		f := &d.fields[d.focus]
		f.text, f.set = f.prefill, f.prefillSet
		d.edInit = d.ed.activeField().value()
		d.focusField(d.focus)
		d.validate()
		return m, nil
	}
	if key == "ctrl+e" && d.externalText() {
		return m, editorCmd("edit", d.fields[d.focus].text, d.formGen)
	}
	if key == "tab" || key == "down" || key == "shift+tab" || key == "up" {
		delta := 1
		if key == "shift+tab" || key == "up" {
			delta = -1
		}
		d.focusField((d.focus + delta + len(d.fields)) % len(d.fields))
		return m, nil
	}
	if d.choice() {
		if key == "left" {
			d.step(-1)
		} else if key == "right" || key == "space" {
			d.step(1)
		}
	} else if !d.externalText() {
		d.ed, _ = d.ed.delegateToField(msg)
		d.syncEditor()
	}
	d.validate()
	return m, nil
}

// A missing workflow needs a read; a loaded job without input keys does not.
func (m Model) cronMayHaveInputs(uid string) bool {
	job := m.cron.data.job(uid)
	if job == nil {
		return false
	}
	if _, ok := job.Definition.Options["inputs"]; ok {
		return true
	}
	if _, ok := job.Definition.Options["input_values"]; ok {
		return true
	}
	wid := job.Definition.Action.WorkflowUID
	if wid == nil || *wid == "" {
		return false
	}
	w := m.cron.data.workflow(*wid)
	if w == nil {
		return true
	}
	_, ok := w.Definition.Options["inputs"]
	return ok
}
