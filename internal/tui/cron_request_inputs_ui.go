package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/x/ansi"
	"go.kenn.io/kit/tui/helplayout"
	"go.kenn.io/kit/tui/termtext"
)

// cronRequestInputsOpen reports whether R's input form replaces the cron screen.
func (m Model) cronRequestInputsOpen() bool {
	return m.view == viewCron && m.cron.action != nil && m.cron.action.inputs != nil
}

func cronRequestInputLabel(f cronRequestInputField) string {
	label := f.input.Title
	if label == "" {
		label = f.input.Name
	}
	label = sanitizeForLine(label)
	if f.input.Required {
		label += " *"
	}
	return label
}

// cronRequestInputHint describes the focused field: type and limits, whether
// it is required, and where its current value came from.
func cronRequestInputHint(f cronRequestInputField) string {
	in := f.input
	var kind string
	switch in.Type {
	case "int":
		kind = "whole number"
		switch {
		case in.Min != nil && in.Max != nil:
			kind += fmt.Sprintf(" %d to %d", *in.Min, *in.Max)
		case in.Min != nil:
			kind += fmt.Sprintf(" at least %d", *in.Min)
		case in.Max != nil:
			kind += fmt.Sprintf(" at most %d", *in.Max)
		}
	case "bool":
		kind = "true or false"
	case "enum":
		kind = fmt.Sprintf("one of %d values", len(in.Values))
	default:
		kind = "text"
		if in.Multiline {
			kind = "multiline text"
		}
		if in.MaxLength != nil {
			kind += fmt.Sprintf(" up to %d bytes", *in.MaxLength)
		}
	}
	need := "optional"
	if in.Required {
		need = "required"
	}
	source := map[string]string{"unset": "no value", "default": "declared default", "job": "job value", "request": "request value"}[in.Source]
	switch {
	case f.jobInvalid && f.text == f.prefill && f.set == f.prefillSet:
		source = "job value invalid; enter a value"
	case f.set != f.prefillSet || f.text != f.prefill:
		source = "changed"
	case source == "":
		source = sanitizeForLine(in.Source)
	}
	return kind + " · " + need + " · " + source
}

// cronRequestInputValue renders one value; the focused text field is the editor.
func cronRequestInputValue(d *cronRequestInputs, i int, locked bool, width int) string {
	f := d.fields[i]
	focused := i == d.focus
	if f.input.Type == "bool" || f.input.Type == "enum" {
		text := f.text
		if !f.set {
			text = "(unset)"
		}
		return "‹ " + sanitizeForLine(text) + " ›"
	}
	if focused && !locked && !f.input.Multiline && d.ed.activeField() != nil {
		ed := d.ed.activeField()
		ed.input.Placeholder = "(unset)"
		if f.set {
			ed.input.Placeholder = `""`
		}
		ed.input.SetWidth(max(width-1, 1))
		ed.input.SetCursor(ed.input.Position())
		return ed.input.View()
	}
	var value string
	switch {
	case !f.set:
		value = subtleStyle.Render("(unset)")
	case f.text == "":
		value = `""`
	default:
		value = sanitizeForLine(strings.ReplaceAll(f.text, "\n", " ⏎ "))
	}
	if focused && f.input.Multiline && !locked {
		value = "ctrl+e  " + value
	}
	return value
}

// cronRequestInputLines renders the draft's fields, one row each, with the
// focused field's description beneath it. cursor is the focused row.
func cronRequestInputLines(m Model, d *cronRequestInputs, locked bool) (lines []string, cursor int) {
	if d.loading {
		return []string{"  Reading inputs…"}, 0
	}
	labelWidth := 0
	for _, f := range d.fields {
		labelWidth = max(labelWidth, ansi.StringWidth(cronRequestInputLabel(f)))
	}
	labelWidth = min(labelWidth, max(m.width/3, 8))
	column := labelWidth + 5
	valueWidth := max(m.width-column, 1)
	for i, f := range d.fields {
		prefix := "  "
		if i == d.focus {
			prefix, cursor = "▶ ", len(lines)
		}
		label := ansi.Truncate(cronRequestInputLabel(f), labelWidth, "…")
		line := prefix + label + ":" + strings.Repeat(" ", labelWidth-ansi.StringWidth(label)+2) +
			cronRequestInputValue(d, i, locked, valueWidth)
		lines = append(lines, ansi.Truncate(line, m.width, "…"))
		if i != d.focus {
			continue
		}
		hints := termtext.Wrap(cronRequestInputHint(f), valueWidth)
		if choice := sanitizeForLine(f.text); f.input.Type == "enum" && f.set && ansi.StringWidth(choice)+4 > valueWidth {
			// The row truncates a long choice; show all of it so similar ones differ.
			hints = append(termtext.Wrap(choice, valueWidth), hints...)
		}
		if f.input.Description != "" {
			hints = append(hints, termtext.Wrap(sanitizeForLine(f.input.Description), valueWidth)...)
		}
		for _, hint := range hints {
			lines = append(lines, strings.Repeat(" ", column)+subtleStyle.Render(ansi.Truncate(hint, valueWidth, "…")))
		}
	}
	return lines, cursor
}

func cronRequestInputsHelpRows(a *cronPending) []helplayout.HelpItem {
	switch {
	case a.inputs.loading:
		return []helplayout.HelpItem{{Key: "esc", Description: "cancel"}}
	case a.requestUID != "":
		return []helplayout.HelpItem{{Key: "ctrl+o", Description: "retry"}, {Key: "esc", Description: "abandon"}}
	}
	return []helplayout.HelpItem{
		{Key: "tab/shift+tab", Description: "field"},
		{Key: "←/→", Description: "choose"},
		{Key: "ctrl+r", Description: "reset"},
		{Key: "ctrl+e", Description: "edit text"},
		{Key: "ctrl+o", Description: "request run"},
		{Key: "esc", Description: "cancel"},
	}
}

// cronRequestInputsStatus is the one status line above the help: sending,
// the retry offer, a validation problem or the hub's refusal.
func cronRequestInputsStatus(a *cronPending, width int) []string {
	wrap := func(text string) []string {
		return termtext.Wrap(errorStyle.Render(sanitizeForLine(text)), max(width, 1))
	}
	switch {
	case a.inflight:
		return []string{subtleStyle.Render("sending request…")}
	case a.requestUID != "" && a.err != nil:
		return wrap("Response unavailable. ctrl+o retries request " + a.requestUID + ": " + a.err.Error())
	case a.inputs.general != "":
		return wrap(a.inputs.general)
	}
	return []string{""}
}

func renderCronRequestInputs(m Model) string {
	a := m.cron.action
	d := a.inputs
	locked := a.requestUID != "" || a.inflight
	title := "Request run · " + sanitizeForLine(a.name)
	inner := titleBarInnerWidth(m.width)
	if right := ansi.StringWidth(a.target) + 1; ansi.StringWidth(title)+right > inner {
		title = ansi.Truncate(title, max(inner-right, 1), "…")
	}
	body := []string{
		renderTitleBar(m.width, m.scope, kataVersion, m.chrome().daemon),
		cronTabs(m.cron.tab),
		statsLineStyle.Render(padLeftRightInside(title, a.target, inner)),
		"",
	}
	help := [][]helplayout.HelpItem{cronRequestInputsHelpRows(a)}
	lines, cursor := cronRequestInputLines(m, d, locked)
	status := cronRequestInputsStatus(a, m.width)
	budget := max(m.height-len(body)-len(status)-helpLines(help, m.width)-cronNoticeRows(m), 1)
	start, end := windowBounds(len(lines), cursor, budget)
	body = append(body, lines[start:end]...)
	for range max(budget-(end-start), 0) {
		body = append(body, "")
	}
	body = append(body, status...)
	body = append(body, renderFooterHelpTable(help, m.width))
	return strings.Join(body, "\n")
}
