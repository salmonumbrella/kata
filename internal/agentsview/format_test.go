package agentsview

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

// The CLI and TUI show the same rounded amount and state text for a run.
func TestUsageText(t *testing.T) {
	for _, tc := range []struct {
		value     int64
		estimated bool
		want      string
	}{
		{0, false, "$0.00"}, {0, true, "~$0.00"}, {1, false, "<$0.01"}, {4999, true, "~<$0.01"},
		{5000, false, "$0.01"}, {2410000, false, "$2.41"}, {2410000, true, "~$2.41"},
		{math.MaxInt64, false, "$9223372036854.78"},
	} {
		require.Equal(t, tc.want, Amount(tc.value, tc.estimated))
	}
	cost := int64(2410000)
	// A priced total that arrives without an amount reads unknown, never $0.00.
	for _, tc := range []struct {
		state              string
		cost               *int64
		unpriced, sessions int
		want               string
	}{
		{"complete", &cost, 0, 2, "~$2.41"}, {"partial", &cost, 1, 2, "~$2.41+ (1 of 2 sessions not priced)"},
		{"complete", nil, 0, 2, "unknown"}, {"unknown", nil, 2, 2, "unknown"}, {"no_sessions", nil, 0, 0, "no sessions"},
	} {
		require.Equal(t, tc.want, TotalText(tc.state, tc.cost, true, false, tc.unpriced, tc.sessions))
	}
	require.Equal(t, "~$2.41", SessionText("priced", &cost, "mixed", nil, nil))
	require.Equal(t, "unpriced (model)", SessionText("unpriced", nil, "", []string{"model\x1b[31m"}, nil))
	require.Equal(t, "AgentsView unavailable", SessionText("unavailable", nil, "", nil, nil))
	require.Equal(t, "not in AgentsView yet", SessionText("not_found", nil, "", nil, nil))
	require.Equal(t, "ambiguous session (archive~id)", SessionText("ambiguous", nil, "", nil, []string{"archive~id"}))
	require.Equal(t, "no usage recorded", SessionText("no_usage", nil, "", nil, nil))
	require.Equal(t, "AgentsView rejected Kata's token", SessionText("unauthorized", nil, "", nil, nil))
}
