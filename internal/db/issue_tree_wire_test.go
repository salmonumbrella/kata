package db_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// The daemon's JSON encoder must honor the required arrays in the public API,
// including empty previews and every outcome without committed members.
func TestIssueTreeWireRequiredArrays(t *testing.T) {
	check := func(t *testing.T, value any, fields ...string) {
		t.Helper()
		raw, err := json.Marshal(value)
		require.NoError(t, err)
		var body map[string]any
		require.NoError(t, json.Unmarshal(raw, &body))
		for _, field := range fields {
			require.Equal(t, []any{}, body[field], "%s must be an array: %s", field, raw)
		}
	}
	t.Run("preview", func(t *testing.T) {
		check(t, db.MoveTreePreview{}, "members", "relations", "provider_actions", "recurrence_actions", "blockers")
	})
	for _, state := range []string{"completed", "partial", "outcome-unknown", "conflict"} {
		t.Run(state, func(t *testing.T) {
			check(t, db.MoveTreeResult{State: state}, "applied", "finalized", "conflicts", "unknown_roots")
		})
	}
}
