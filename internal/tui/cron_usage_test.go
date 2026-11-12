package tui

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
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
// $2.41 usage answer for the fixture run. It counts usage requests and those
// asking for a refresh.
func cronUsageDaemon(t *testing.T, data cronData, source string) (*httptest.Server, *atomic.Int64, *atomic.Int64) {
	t.Helper()
	var usage, refreshes atomic.Int64
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
			if r.URL.Query().Get("refresh") == "true" {
				refreshes.Add(1)
			}
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
	return srv, &usage, &refreshes
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
			srv, usage, refreshes := cronUsageDaemon(t, data, tc.source)
			m.api = NewClient(srv.URL, srv.Client())

			// The existing refresh key rereads the cron data from the daemon.
			m, cmd := updateModel(m, keyRune('r'))
			m = cronUsageSettle(t, m, cmd)
			require.Zero(t, usage.Load(), "lists never read usage")
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
				require.Zero(t, refreshes.Load())
				m, cmd = updateModel(m, keyRune('r'))
				m = cronUsageSettle(t, m, cmd)
				require.Regexp(t, regexp.MustCompile(`Cost:\s+\$2\.41\b`), stripANSI(m.viewBody()))
				require.Equal(t, int64(2), usage.Load())
				require.Equal(t, int64(1), refreshes.Load())
				return
			}
			require.False(t, strings.Contains(body, "Cost:"), body)
			require.Zero(t, usage.Load())
		})
	}
}

// The daemon answers with a partial total once its five-second pricing budget
// ends, so the usage read alone outlasts the TUI's five-second request timeout.
// Its caller can still cancel it.
func TestCronRunUsageWaitsForPartialAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(defaultHTTPTimeout + 500*time.Millisecond):
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"run_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAY","state":"partial","cost_microdollars":2410000,"estimated":false,"sessions_incomplete":false,"fetched_at":"2026-01-01T00:00:00Z","sessions":[{"agent":"claude","session_id":"one","state":"priced","cost_microdollars":2410000},{"agent":"codex","session_id":"two","state":"unavailable"}]}`))
	}))
	t.Cleanup(srv.Close)
	c := NewClient(srv.URL, &http.Client{Timeout: defaultHTTPTimeout})
	ctx, cancel := context.WithTimeout(t.Context(), cronUsageTimeout)
	defer cancel()
	usage, err := c.CronRunUsage(ctx, 7, "01ARZ3NDEKTSV4RRFFQ69G5FAY", false)
	require.NoError(t, err)
	require.Equal(t, generated.Partial, usage.State)

	ctx, cancel = context.WithCancel(t.Context())
	time.AfterFunc(100*time.Millisecond, cancel)
	started := time.Now()
	_, err = c.CronRunUsage(ctx, 7, "01ARZ3NDEKTSV4RRFFQ69G5FAY", false)
	require.ErrorIs(t, err, context.Canceled)
	require.Less(t, time.Since(started), defaultHTTPTimeout)
}

// cronUsageModel opens run A's detail on a daemon that prices runs with
// AgentsView. The loaded runs are A, with two recorded sessions, then B.
func cronUsageModel(t *testing.T, usage generated.RunUsage) (Model, *cronReadTestAPI, tea.Cmd) {
	t.Helper()
	m := cronFixture(t)
	m.sseCh = nil
	data := *m.cron.data
	data.usage = generated.Agentsview
	run := data.runs[0]
	run.Summary.Sessions = []generated.SessionResponse{
		{Agent: "claude", SessionID: "00000000-0000-4000-8000-000000000001", Step: new("main")},
		{Agent: "codex", SessionID: "00000000-0000-4000-8000-000000000002", Step: new("review")},
	}
	other := run
	other.UID, other.ID = "01ARZ3NDEKTSV4RRFFQ69G5FAZ", 2
	data.runs = []generated.CronRun{run, other}
	a := &cronReadTestAPI{data: data, usage: usage}
	m.api = a
	m, _ = updateModel(m, cronLoadedMsg(data))
	m = sendRune(m, '3')
	m, cmd := updateModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	return m, a, cmd
}

// cronUsageRead runs a command and returns the usage reads it made.
func cronUsageRead(cmd tea.Cmd) []tea.Msg {
	var reads []tea.Msg
	for _, msg := range runCmdTree(cmd) {
		if _, ok := msg.(cronUsageMsg); ok {
			reads = append(reads, msg)
		}
	}
	return reads
}

func cronUsageSessions(states ...generated.RunSessionState) []generated.RunSession {
	sessions := []generated.RunSession{}
	for i, state := range states {
		session := generated.RunSession{Agent: []string{"claude", "codex"}[i], SessionID: fmt.Sprintf("00000000-0000-4000-8000-00000000000%d", i+1), State: state}
		if state == generated.Priced {
			session.CostMicrodollars = new(int64(1205000))
		}
		sessions = append(sessions, session)
	}
	return sessions
}

// The spec's stale-result fence: a late answer never replaces a newer one.
func TestCronRunUsageRefreshAndFence(t *testing.T) {
	complete := generated.RunUsage{State: generated.Complete, CostMicrodollars: new(int64(2410000)), Sessions: cronUsageSessions(generated.Priced, generated.Priced)}
	for _, name := range []string{"refresh", "leave and reopen", "A to B to A", "project", "daemon", "row removed", "newer state"} {
		t.Run(name, func(t *testing.T) {
			m, a, cmd := cronUsageModel(t, complete)
			if name == "newer state" {
				a.usage.State = "future_state"
			}
			first := cronUsageRead(cmd)
			require.Len(t, first, 1)
			body := func() string { return stripANSI(m.viewBody()) }
			switch name {
			case "refresh":
				m, _ = updateModel(m, first[0])
				require.Contains(t, body(), "Cost:       $2.41")
				m, cmd = updateModel(m, keyRune('r'))
				require.Contains(t, body(), "Cost:       $2.41 · refreshing…")
				var again tea.Cmd
				m, again = updateModel(m, keyRune('r'))
				require.Empty(t, cronUsageRead(again), "a pending refresh absorbs repeated r")
				a.usageErr = errors.New("daemon busy")
				refreshed := cronUsageRead(cmd)
				require.Len(t, refreshed, 1)
				m, _ = updateModel(m, refreshed[0])
				require.Contains(t, body(), "Cost:       unavailable: daemon busy · r to retry")
				require.NotContains(t, body(), "$2.41")
				require.Equal(t, []string{"01ARZ3NDEKTSV4RRFFQ69G5FAY false", "01ARZ3NDEKTSV4RRFFQ69G5FAY true"}, a.usageReads)
			case "leave and reopen", "A to B to A":
				m = sendRune(m, 'h')
				if name == "A to B to A" {
					m = sendRune(m, 'j')
					m, cmd = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
					require.Len(t, cronUsageRead(cmd), 1)
					m = sendRune(m, 'h')
					m = sendRune(m, 'k')
				}
				m, cmd = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
				m, _ = updateModel(m, first[0])
				require.Contains(t, body(), "Cost:       loading…")
				latest := cronUsageRead(cmd)
				require.Len(t, latest, 1)
				m, _ = updateModel(m, latest[0])
				require.Contains(t, body(), "Cost:       $2.41")
			case "project", "daemon":
				if name == "daemon" {
					m.connGen++
				} else {
					m.scopeGen++
				}
				m, _ = updateModel(m, tea.FocusMsg{})
				m, _ = updateModel(m, first[0])
				require.Equal(t, cronUsage{}, m.cron.usage)
			case "row removed":
				data := *m.cron.data
				data.runs = data.runs[1:]
				m, _ = updateModel(m, cronLoadedMsg(data))
				m, _ = updateModel(m, first[0])
				require.Contains(t, body(), "this run is no longer in the loaded data")
				require.NotContains(t, body(), "Cost:")
			case "newer state":
				m, _ = updateModel(m, first[0])
				require.Contains(t, body(), "Cost:       unavailable: unsupported usage state · r to retry")
			}
		})
	}
}

func TestSnapshot_CronRunCost(t *testing.T) {
	defer snapshotInit(t)()
	cost := new(int64(2410000))
	for _, tc := range []struct {
		name  string
		usage generated.RunUsage
	}{
		{"complete", generated.RunUsage{State: generated.Complete, CostMicrodollars: cost, Sessions: cronUsageSessions(generated.Priced, generated.Priced)}},
		{"estimated", generated.RunUsage{State: generated.Complete, CostMicrodollars: cost, Estimated: true, Sessions: cronUsageSessions(generated.Priced, generated.Priced)}},
		{"partial", generated.RunUsage{State: generated.Partial, CostMicrodollars: cost, Estimated: true, Sessions: cronUsageSessions(generated.Priced, generated.NotFound)}},
		{"unknown", generated.RunUsage{State: generated.RunUsageStateUnknown, Sessions: cronUsageSessions(generated.RunSessionStateUnauthorized, generated.RunSessionStateUnavailable)}},
		{"no-sessions", generated.RunUsage{State: generated.NoSessions, Sessions: []generated.RunSession{}}},
		{"unavailable", generated.RunUsage{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, a, cmd := cronUsageModel(t, tc.usage)
			if tc.name == "unavailable" {
				a.usageErr = &APIError{Method: "GET", Path: "/api/v1/projects/7/cron/runs/01ARZ3NDEKTSV4RRFFQ69G5FAY/usage", Status: 503, Code: "usage_source_disabled", Message: "usage source is disabled"}
			}
			if tc.name == "no-sessions" {
				m.cron.data.runs[0].Summary.Sessions = nil
			}
			reads := cronUsageRead(cmd)
			require.Len(t, reads, 1)
			m, _ = updateModel(m, reads[0])
			assertGolden(t, "cron-run-cost-"+tc.name, m.viewBody())
		})
	}
}
