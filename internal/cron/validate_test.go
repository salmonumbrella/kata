package cron

import (
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

const executeJSON = `{"version":1,"kind":"job","trigger":{"kind":"manual"},"action":{"kind":"execute","prompt":"Review changes"},"issue":{"kind":"per-run","title":"Review"},"overlap":"forbid","catchup":"skip"}`

// Native definitions are versioned portable documents. Invalid envelopes cannot
// become saved jobs, including notify actions accidentally carrying execution.
func TestParseJob(t *testing.T) {
	for _, input := range []string{executeJSON,
		`{"version":1,"kind":"job","trigger":{"kind":"cron","cron":"0 9 * * 1-5"},"action":{"kind":"notify","recipient":"operator","message":"Review"},"issue":{"kind":"existing","uid":"01ARZ3NDEKTSV4RRFFQ69G5FAV"},"overlap":"forbid","catchup":"latest"}`,
		`{"version":1,"kind":"job","trigger":{"kind":"issue-deadline","issue_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAV","lead_seconds":1800},"action":{"kind":"notify","recipient":"operator/worker","message":"Due soon"},"overlap":"forbid","catchup":"latest"}`,
	} {
		job, err := ParseJob([]byte(input))
		require.NoError(t, err)
		require.False(t, job.Enabled)
	}
	for name, input := range map[string]string{
		"unknown version":    strings.Replace(executeJSON, `"version":1`, `"version":2`, 1),
		"unknown field":      strings.Replace(executeJSON, `"version":1`, `"version":1,"typo":true`, 1),
		"deadline execute":   strings.Replace(executeJSON, `"kind":"manual"`, `"kind":"issue-deadline","issue_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAV"`, 1),
		"notify with prompt": strings.Replace(executeJSON, `"kind":"execute"`, `"kind":"notify","recipient":"operator"`, 1),
		"bad workflow UID":   strings.Replace(executeJSON, `"prompt":"Review changes"`, `"workflow_uid":"workflow.yaml"`, 1),
		"local checkout":     strings.Replace(executeJSON, `"version":1`, `"version":1,"checkout_key":"/home/worker/repo"`, 1),
		"secret value":       strings.Replace(executeJSON, `"version":1`, `"version":1,"options":{"api_token":"secret"}`, 1),
		"oversized":          strings.Replace(executeJSON, `Review changes`, strings.Repeat("a", 256*1024), 1),
		"zero interval":      strings.Replace(executeJSON, `"kind":"manual"`, `"kind":"interval","interval_seconds":0`, 1),
		"bad timezone":       strings.Replace(executeJSON, `"kind":"manual"`, `"kind":"once","at":"2026-10-03T00:00:00Z","timezone":"Not/AZone"`, 1),
		"duplicate key":      strings.Replace(executeJSON, `"version":1`, `"version":1,"version":1`, 1),
	} {
		t.Run(name, func(t *testing.T) { _, err := ParseJob([]byte(input)); require.Error(t, err) })
	}
}

// Job references use the same workflow vocabulary as the CLI and run records.
// The retired spelling must not be accepted as an ignored option.
func TestParseJobWorkflowReference(t *testing.T) {
	input := strings.Replace(executeJSON, `"prompt":"Review changes"`, `"workflow_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAV"`, 1)
	job, err := ParseJob([]byte(input))
	require.NoError(t, err)
	encoded, err := json.Marshal(job)
	require.NoError(t, err)
	var document struct {
		Action map[string]any `json:"action"`
	}
	require.NoError(t, json.Unmarshal(encoded, &document))
	require.Equal(t, "01ARZ3NDEKTSV4RRFFQ69G5FAV", document.Action["workflow_uid"])
	require.NotContains(t, document.Action, "flow_uid")
	_, err = ParseJob([]byte(strings.Replace(input, "workflow_uid", "flow_uid", 1)))
	require.Error(t, err)
}

func TestParseWorkflow(t *testing.T) {
	workflow, err := ParseWorkflow([]byte(`{"version":1,"steps":[{"key":"inspect","kind":"command","command":"git status"},{"key":"review","kind":"prompt","prompt":"Review","after":["inspect"],"retries":2}]}`))
	require.NoError(t, err)
	require.Len(t, workflow.Steps, 2)
	for _, input := range []string{
		`{"version":1,"steps":[]}`,
		`{"version":1,"steps":[{"key":"a","kind":"prompt","prompt":"Hi","after":["a"]}]}`,
		`{"version":1,"steps":[{"key":"a","kind":"command","command":"true","retries":-1}]}`,
		`{"version":1,"steps":[{"key":"a","kind":"command","command":"true"},{"key":"a","kind":"command","command":"false"}]}`,
	} {
		_, err := ParseWorkflow([]byte(input))
		require.Error(t, err)
	}
}

// All representable prompt strings survive the native definition round trip;
// invalid UTF-8 is rejected by JSON before reaching the domain.
func TestJobPromptRoundTripProperty(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		prompt := rapid.String().Draw(t, "prompt")
		job, err := ParseJob([]byte(executeJSON))
		require.NoError(t, err)
		job.Action.Prompt = "Review: " + prompt
		encoded, err := json.Marshal(job)
		require.NoError(t, err)
		restored, err := ParseJob(encoded)
		require.NoError(t, err)
		require.Equal(t, job, restored)
	})
}

func TestUnsupportedVersionsRejectedProperty(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		version := rapid.Int().Draw(t, "version")
		if version == 1 {
			return
		}
		job, err := ParseJob([]byte(executeJSON))
		require.NoError(t, err)
		job.Version = version
		encoded, err := json.Marshal(job)
		require.NoError(t, err)
		_, err = ParseJob(encoded)
		require.Error(t, err)
	})
}

// Command and prompt variants obey the same exact document round-trip law,
// including non-default retries and dependency lists.
func TestWorkflowRoundTripProperty(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		texts := rapid.SliceOf(rapid.String()).Draw(t, "step text")
		if len(texts) == 0 {
			texts = []string{""}
		}
		workflow := WorkflowDefinition{Version: 1}
		for i, text := range texts {
			step := WorkflowStep{Key: fmt.Sprintf("step-%d", i), Retries: rapid.IntRange(0, 100).Draw(t, "retries")}
			if rapid.Bool().Draw(t, "command") {
				step.Kind = "command"
				step.Command = "echo " + text
			} else {
				step.Kind = "prompt"
				step.Prompt = "Review: " + text
			}
			if i > 0 && rapid.Bool().Draw(t, "dependency") {
				step.After = []string{workflow.Steps[i-1].Key}
			}
			workflow.Steps = append(workflow.Steps, step)
		}
		raw, err := json.Marshal(workflow)
		require.NoError(t, err)
		restored, err := ParseWorkflow(raw)
		require.NoError(t, err)
		require.Equal(t, workflow, restored)
	})
}

func TestCronCompatibility(t *testing.T) {
	for _, expression := range []string{"0 9 * JAN MON-FRI", "@hourly", "@daily", "@weekly", "@monthly", "@yearly", "@every 90m"} {
		t.Run(expression, func(t *testing.T) {
			job, err := ParseJob([]byte(executeJSON))
			require.NoError(t, err)
			job.Trigger = Trigger{Kind: "cron", Cron: expression, Timezone: "UTC"}
			require.NoError(t, job.Validate())
		})
	}
	for _, expression := range []string{"@invented", "@every nonsense", "61 * * * *", "* * * * * *"} {
		require.Error(t, (Trigger{Kind: "cron", Cron: expression}).Validate())
	}
}

func TestWorkflowPortableTopLevelConfiguration(t *testing.T) {
	input := `{"version":1,"about":"Review a change","input":"Change reference","options":{"skip_permissions":false,"overwatch":{"agent":"reviewer","model":"example-model"}},"steps":[{"key":"review","kind":"prompt","prompt":"Review"}]}`
	workflow, err := ParseWorkflow([]byte(input))
	require.NoError(t, err)
	encoded, err := json.Marshal(workflow)
	require.NoError(t, err)
	require.JSONEq(t, input, string(encoded))
	for _, bad := range []string{
		strings.Replace(input, `"agent":"reviewer"`, `"api_token":"credential"`, 1),
		strings.Replace(input, `"agent":"reviewer"`, `"path":"/local/checkout"`, 1),
		strings.Replace(input, `"example-model"`, `"`+strings.Repeat("x", DefinitionLimit)+`"`, 1),
	} {
		_, err := ParseWorkflow([]byte(bad))
		require.Error(t, err)
	}
}

func TestRelativeDateOffsetRejectsDurationOverflow(t *testing.T) {
	job, err := ParseJob([]byte(executeJSON))
	require.NoError(t, err)
	for _, offset := range []int64{9223372037, -9223372037} {
		job.Issue = &IssuePolicy{Kind: "per-run", Title: "Result", ScheduledOffsetSeconds: &offset}
		require.Error(t, job.Validate())
		job.Issue = &IssuePolicy{Kind: "per-run", Title: "Result", DeadlineOffsetSeconds: &offset}
		require.Error(t, job.Validate())
	}
}

func TestDeadlineLeadDurationAdmission(t *testing.T) {
	for _, tc := range []struct {
		lead  int64
		valid bool
	}{
		{-1, false}, {0, true}, {9223372035, true}, {9223372036, true}, {9223372037, false}, {9223372036854775807, false},
	} {
		t.Run(fmt.Sprint(tc.lead), func(t *testing.T) {
			trigger := Trigger{Kind: "issue-deadline", IssueUID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", LeadSeconds: tc.lead}
			require.Equal(t, tc.valid, trigger.Validate() == nil)
			raw := fmt.Sprintf(`{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"issue-deadline","issue_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAV","lead_seconds":%d},"action":{"kind":"notify","recipient":"operator","message":"Attention"},"overlap":"forbid","catchup":"all"}`, tc.lead)
			_, err := ParseJob([]byte(raw))
			require.Equal(t, tc.valid, err == nil)
		})
	}
}
