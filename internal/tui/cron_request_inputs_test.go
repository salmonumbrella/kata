package tui

import (
	"testing"

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
