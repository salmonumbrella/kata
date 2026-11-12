package tui

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/pkg/client/generated"
)

// cronUsageDaemon is a fake daemon for project 7 serving the fixture's cron
// lists, the capabilities document with the given usage source, and a complete
// $2.41 usage answer for the fixture run. It counts usage requests.
func cronUsageDaemon(t *testing.T, data cronData, source string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var usage atomic.Int64
	base := "/api/v1/projects/7/cron"
	runUID := data.runs[0].UID
	write := func(w http.ResponseWriter, body any) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.MarshalWrite(w, body); err != nil {
			t.Error(err)
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method != http.MethodGet:
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"status":"queued"}`))
		case r.URL.Path == base+"/jobs":
			write(w, map[string]any{"jobs": data.jobs})
		case r.URL.Path == base+"/workflows":
			write(w, map[string]any{"workflows": data.workflows})
		case r.URL.Path == base+"/runs":
			write(w, map[string]any{"runs": data.runs})
		case r.URL.Path == base+"/capabilities":
			write(w, map[string]any{
				"project_uid":    "01ARZ3NDEKTSV4RRFFQ69G5FB5",
				"event_features": []string{"cron_v1", "cron_run_sessions_v1"},
				"usage":          map[string]string{"source": source},
			})
		case r.URL.Path == base+"/runs/"+runUID+"/usage":
			usage.Add(1)
			if source != "agentsview" {
				http.Error(w, `{"code":"usage_disabled"}`, http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Cache-Control", "private, no-store")
			write(w, map[string]any{
				"run_uid": runUID, "state": "complete", "cost_microdollars": int64(2410000), "estimated": false,
				"sessions_incomplete": false, "fetched_at": snapshotFixedNow,
				"sessions": []map[string]any{{
					"agent": "claude", "session_id": "00000000-0000-4000-8000-000000000001", "step": "main",
					"state": "priced", "canonical_session_id": "00000000-0000-4000-8000-000000000001",
					"cost_microdollars": int64(2410000), "cost_source": "reported",
				}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &usage
}

// cronUsageSettle delivers each command's messages back through Update until
// no command remains. A command still running after 15s fails the test.
func cronUsageSettle(t *testing.T, m Model, cmd tea.Cmd) Model {
	t.Helper()
	for range 8 {
		if cmd == nil {
			return m
		}
		done := make(chan []tea.Msg, 1)
		go func(cmd tea.Cmd) { done <- runCmdTree(cmd) }(cmd)
		var msgs []tea.Msg
		select {
		case msgs = <-done:
		case <-time.After(15 * time.Second):
			t.Fatal("a cron command did not finish")
		}
		var next []tea.Cmd
		for _, msg := range msgs {
			if msg == nil {
				continue
			}
			var c tea.Cmd
			m, c = updateModel(m, msg)
			next = append(next, c)
		}
		cmd = tea.Batch(next...)
	}
	t.Fatal("cron commands did not settle")
	return m
}

// TestCronRunUsageOpenDetail covers the spec's done-when for the TUI run
// detail: with usage on, opening a run with sessions shows its cost after one
// usage request; with usage off there is no cost line and no usage request.
func TestCronRunUsageOpenDetail(t *testing.T) {
	for _, tc := range []struct {
		source  string
		enabled bool
	}{
		{"agentsview", true},
		{"none", false},
	} {
		t.Run(tc.source, func(t *testing.T) {
			m := cronFixture(t)
			m.sseCh = nil
			m.height = 40
			data := *m.cron.data
			data.runs = slices.Clone(data.runs)
			data.runs[0].Summary.Sessions = []generated.SessionResponse{{Agent: "claude", SessionID: "00000000-0000-4000-8000-000000000001", Step: new("main")}}
			srv, usage := cronUsageDaemon(t, data, tc.source)
			m.api = NewClient(srv.URL, srv.Client())

			// The existing refresh key rereads the cron data from the daemon.
			m, cmd := updateModel(m, keyRune('r'))
			m = cronUsageSettle(t, m, cmd)
			m = sendRune(m, '3')
			m, cmd = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
			m = cronUsageSettle(t, m, cmd)
			require.Len(t, m.cron.stack, 1)
			require.Equal(t, data.runs[0].UID, m.cron.stack[0].ref.uid)

			body := stripANSI(m.viewBody())
			require.Contains(t, body, "claude 00000000")
			if tc.enabled {
				require.Regexp(t, regexp.MustCompile(`Cost:\s+\$2\.41\b`), body)
				require.Equal(t, int64(1), usage.Load())
				return
			}
			require.False(t, strings.Contains(body, "Cost:"), body)
			require.Zero(t, usage.Load())
		})
	}
}
