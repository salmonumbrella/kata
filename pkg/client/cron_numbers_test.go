package client_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

// Exact native values must survive both public response styles and a typed
// read/edit/resubmit. JSONEq alone would round these numbers in its own oracle.
func TestCronTypedSDKPreservesNumbers(t *testing.T) {
	const opaque = `{"large":9007199254740993,"nested":{"items":[-9007199254740993,1.0000000000000000001,{"value":999999999999999999999999999999}]}}`
	id, err := client.NewCronUID()
	require.NoError(t, err)
	jobDefinition := `{"version":1,"kind":"job","enabled":false,"trigger":{"kind":"manual"},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspect"},"overlap":"forbid","catchup":"all","options":` + opaque + `}`
	flowDefinition := `{"version":1,"options":` + opaque + `,"steps":[{"key":"inspect","kind":"command","command":"git status","options":` + opaque + `}]}`
	decode := func(t *testing.T, raw []byte) any {
		t.Helper()
		d := json.NewDecoder(bytes.NewReader(raw))
		d.UseNumber()
		var value any
		require.NoError(t, d.Decode(&value))
		return value
	}
	exact := func(t *testing.T, value any) {
		t.Helper()
		raw, e := json.Marshal(value)
		require.NoError(t, e)
		require.Equal(t, decode(t, []byte(opaque)), decode(t, raw))
	}
	for _, withResponse := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "with response"}[withResponse], func(t *testing.T) {
			var submitted []byte
			var mu sync.Mutex
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method != http.MethodGet {
					raw, e := io.ReadAll(r.Body)
					require.NoError(t, e)
					mu.Lock()
					submitted = raw
					mu.Unlock()
				}
				var response string
				switch {
				case strings.Contains(r.URL.Path, "/jobs/"):
					response = `{"job":{"definition":` + jobDefinition + `}}`
				case strings.Contains(r.URL.Path, "/flows/"):
					response = `{"flow":{"definition":` + flowDefinition + `}}`
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
				}
				_, e := w.Write([]byte(response))
				require.NoError(t, e)
			}))
			defer server.Close()
			c, e := client.NewWithHTTPClient(server.URL, server.Client())
			require.NoError(t, e)
			jobOptions := &generated.ShowCronJobRequestOptions{PathParams: &generated.ShowCronJobPath{ProjectID: 1, CronUID: id}}
			var job generated.CronJobDefinition
			if withResponse {
				r, e := c.ShowCronJobWithResponse(t.Context(), jobOptions)
				require.NoError(t, e)
				copyCronSDKDefinition(t, r.JSON200.Job.Definition, &job)
			} else {
				r, e := c.ShowCronJob(t.Context(), jobOptions)
				require.NoError(t, e)
				copyCronSDKDefinition(t, r.Job.Definition, &job)
			}
			exact(t, job.Options)
			_, e = c.ReplaceCronJob(t.Context(), &generated.ReplaceCronJobRequestOptions{PathParams: &generated.ReplaceCronJobPath{ProjectID: 1, CronUID: id}, Body: &generated.ReplaceCronJobBody{Name: "Inspect", Definition: job}})
			require.NoError(t, e)
			mu.Lock()
			sent := decode(t, submitted).(map[string]any)
			mu.Unlock()
			require.Equal(t, decode(t, []byte(jobDefinition)), sent["definition"])
			flowOptions := &generated.ShowCronFlowRequestOptions{PathParams: &generated.ShowCronFlowPath{ProjectID: 1, CronUID: id}}
			var flow generated.CronFlowDefinition
			if withResponse {
				r, e := c.ShowCronFlowWithResponse(t.Context(), flowOptions)
				require.NoError(t, e)
				copyCronSDKDefinition(t, r.JSON200.Flow.Definition, &flow)
			} else {
				r, e := c.ShowCronFlow(t.Context(), flowOptions)
				require.NoError(t, e)
				copyCronSDKDefinition(t, r.Flow.Definition, &flow)
			}
			exact(t, flow.Options)
			exact(t, flow.Steps[0].Options)
			_, e = c.ReplaceCronFlow(t.Context(), &generated.ReplaceCronFlowRequestOptions{PathParams: &generated.ReplaceCronFlowPath{ProjectID: 1, CronUID: id}, Body: &generated.ReplaceCronFlowBody{Name: "Inspect", Definition: flow}})
			require.NoError(t, e)
			mu.Lock()
			sent = decode(t, submitted).(map[string]any)
			mu.Unlock()
			require.Equal(t, decode(t, []byte(flowDefinition)), sent["definition"])

		})
	}
}

// A typed opaque JSON round trip preserves the full integer domains rather
// than only the binary64-exact subset. The oracle is decimal formatting.
func FuzzCronTypedJSONNumbers(f *testing.F) {
	f.Add(uint64(9007199254740993), int64(-9007199254740993))
	f.Add(^uint64(0), int64(-1<<63))
	f.Fuzz(func(t *testing.T, positive uint64, negative int64) {
		for _, surface := range []struct {
			field string
			value any
		}{
			{"options", new(generated.CronJobDefinition)},
			{"options", new(generated.CronFlowDefinition)},
			{"options", new(generated.CronFlowStep)},
			{"options", new(generated.CronJobDefinitionResponse)},
			{"options", new(generated.CronFlowDefinitionResponse)},
			{"options", new(generated.CronFlowStepResponse)},
		} {
			expected := fmt.Sprintf(`{"nested":[%d,{"value":%d}]}`, positive, negative)
			raw := fmt.Sprintf(`{%q:%s}`, surface.field, expected)
			require.NoError(t, json.Unmarshal([]byte(raw), surface.value))
			output, err := json.Marshal(surface.value)
			require.NoError(t, err)
			var fields map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(output, &fields))
			require.Equal(t, expected, string(fields[surface.field]), "%T", surface.value)
		}
	})
}

func TestCronTypedJSONRejectsInvalidObjects(t *testing.T) {
	for _, surface := range []struct {
		field string
		value any
	}{
		{"options", new(generated.CronJobDefinition)},
		{"options", new(generated.CronFlowDefinition)},
		{"options", new(generated.CronFlowStep)},
		{"options", new(generated.CronJobDefinitionResponse)},
		{"options", new(generated.CronFlowDefinitionResponse)},
		{"options", new(generated.CronFlowStepResponse)},
	} {
		for _, raw := range []string{fmt.Sprintf(`{%q:[]}`, surface.field), fmt.Sprintf(`{%q:{"n":01}}`, surface.field)} {
			require.Error(t, json.Unmarshal([]byte(raw), surface.value), "%T: %s", surface.value, raw)
		}
	}
}

func copyCronSDKDefinition(t *testing.T, source, target any) {
	t.Helper()
	raw, err := json.Marshal(source)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, target))
}
