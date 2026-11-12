package agentsview

import (
	"fmt"
	"strings"

	"go.kenn.io/kata/internal/textsafe"
)

// Amount formats microdollars as dollars rounded half up to the cent. A
// positive amount below half a cent shows as <$0.01, and an estimate starts
// with ~.
func Amount(microdollars int64, estimated bool) string {
	cents := microdollars / 10000
	if microdollars%10000 >= 5000 {
		cents++
	}
	prefix := ""
	if estimated {
		prefix = "~"
	}
	if microdollars > 0 && cents == 0 {
		return prefix + "<$0.01"
	}
	return fmt.Sprintf("%s$%d.%02d", prefix, cents/100, cents%100)
}

// TotalText describes a run's total as the daemon reported it, without
// summing sessions. A partial total ends with how many of the recorded
// sessions are not priced.
func TotalText(state string, microdollars *int64, estimated bool, unpriced, sessions int) string {
	switch state {
	case "unknown":
		return "unknown"
	case "complete", "partial":
		if microdollars == nil {
			return "unknown"
		}
		total := Amount(*microdollars, estimated)
		if state == "partial" {
			total += fmt.Sprintf("+ (%d of %d sessions not priced)", unpriced, sessions)
		}
		return total
	}
	return "no sessions"
}

// SessionText describes one session's usage: its amount when priced,
// otherwise why it is not priced.
func SessionText(state string, microdollars *int64, costSource string, unpricedModels, candidates []string) string {
	switch state {
	case "priced":
		if microdollars != nil {
			return Amount(*microdollars, costSource == "computed" || costSource == "mixed")
		}
	case "unpriced":
		if len(unpricedModels) > 0 {
			return "unpriced (" + textsafe.Line(strings.Join(unpricedModels, ", ")) + ")"
		}
		return "unpriced"
	case "no_usage":
		return "no usage recorded"
	case "not_found":
		return "not in AgentsView yet"
	case "ambiguous":
		if len(candidates) > 0 {
			return "ambiguous session (" + textsafe.Line(strings.Join(candidates, ", ")) + ")"
		}
		return "ambiguous session"
	case "unauthorized":
		return "AgentsView rejected Kata's token"
	}
	return "AgentsView unavailable"
}
