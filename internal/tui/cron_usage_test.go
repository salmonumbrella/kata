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
// lists, the capabilities document with the usage source currently in source
// ("error" fails that read), and a complete $2.41 usage answer for the fixture
// run. It counts usage requests and those asking for a refresh.
func cronUsageDaemon(t *testing.T, data cronData, source *atomic.Value) (*httptest.Server, *atomic.Int64, *atomic.Int64) {
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
		source, _ := source.Load().(string)
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
		case r.URL.Path == base+"/capabilities" && source == "error":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"status":500,"error":{"code":"internal_error","message":"daemon busy"}}`))
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
// no command remains.
func cronUsageSettle(t *testing.T, m Model, cmd tea.Cmd) Model {
	t.Helper()
	for range 8 {
		if cmd == nil {
			return m
		}
		var next []tea.Cmd
		for _, msg := range runCmdTree(cmd) {
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
// When the usage source could not be read, the cost says so and no usage
// request is sent.
func TestCronRunUsageOpenDetail(t *testing.T) {
	for _, tc := range []struct {
		source  string
		enabled bool
	}{
		{"agentsview", true},
		{"none", false},
		{"error", false},
	} {
		t.Run(tc.source, func(t *testing.T) {
			m := cronFixture(t)
			m.sseCh = nil
			m.height = 40
			data := *m.cron.data
			data.runs = slices.Clone(data.runs)
			data.runs[0].Summary.Sessions = []generated.SessionResponse{{Agent: "claude", SessionID: "00000000-0000-4000-8000-000000000001", Step: new("main")}}
			var source atomic.Value
			source.Store(tc.source)
			srv, usage, refreshes := cronUsageDaemon(t, data, &source)
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
			if tc.source == "error" {
				unavailable := regexp.MustCompile(`Cost:\s+unavailable: internal_error: daemon busy · r: refresh`)
				require.Regexp(t, unavailable, body)
				require.Zero(t, usage.Load())
				// A source last read as off is not known to be off once its
				// next read fails; r recovers when the daemon answers again.
				for _, next := range []string{"none", "error", "agentsview"} {
					source.Store(next)
					m, cmd = updateModel(m, keyRune('r'))
					m = cronUsageSettle(t, m, cmd)
					body = stripANSI(m.viewBody())
					switch next {
					case "none":
						require.NotContains(t, body, "Cost:")
					case "error":
						require.Regexp(t, unavailable, body)
					case "agentsview":
						require.Regexp(t, regexp.MustCompile(`Cost:\s+\$2\.41\b`), body)
					}
				}
				require.Equal(t, int64(1), usage.Load())
				require.Zero(t, refreshes.Load())
				return
			}
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
			m, cmd = updateModel(m, keyRune('r'))
			m = cronUsageSettle(t, m, cmd)
			require.NotContains(t, stripANSI(m.viewBody()), "Cost:")
			require.Zero(t, usage.Load(), "r with usage off reads no usage")
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
		_, _ = w.Write([]byte(`{"run_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAY","state":"partial","cost_microdollars":2410000,"estimated":false,"sessions_incomplete":false,"fetched_at":"2026-01-01T00:00:00Z","sessions":[{"agent":"claude","session_id":"00000000-0000-4000-8000-000000000001","state":"priced","cost_microdollars":2410000,"cost_source":"reported"},{"agent":"codex","session_id":"00000000-0000-4000-8000-000000000002","state":"unavailable"}]}`))
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

// cronUsageSessions builds the run's two sessions as the daemon reports them;
// each priced session costs cost microdollars from source.
func cronUsageSessions(source string, cost int64, states ...generated.RunSessionState) []generated.RunSession {
	sessions := []generated.RunSession{}
	for i, state := range states {
		session := generated.RunSession{Agent: []string{"claude", "codex"}[i], SessionID: fmt.Sprintf("00000000-0000-4000-8000-00000000000%d", i+1), State: state}
		if state == generated.Priced {
			session.CostMicrodollars, session.CostSource = new(cost), new(source)
		}
		sessions = append(sessions, session)
	}
	return sessions
}

// The open run's cost through refresh and a newer daemon's state, then the
// stale-result fence: a late answer never replaces a newer one, including an
// answer for an earlier revision of the same run.
func TestCronRunUsageStates(t *testing.T) {
	complete := generated.RunUsage{State: generated.Complete, CostMicrodollars: new(int64(2410000)), Sessions: cronUsageSessions("reported", 1205000, generated.Priced, generated.Priced)}
	for _, name := range []string{"refresh", "newer state", "incomplete list", "revised after answer", "revised while pending", "leave and reopen", "project", "daemon", "row removed"} {
		t.Run(name, func(t *testing.T) {
			m, a, cmd := cronUsageModel(t, complete)
			switch name {
			case "newer state":
				a.usage.State = "future_state"
			case "incomplete list":
				a.usage.State, a.usage.SessionsIncomplete = generated.Partial, true
			}
			first := cronUsageRead(cmd)
			require.Len(t, first, 1)
			body := func() string { return stripANSI(m.viewBody()) }
			// revise loads a later observation of the open run that records a
			// third session, as a reporting executor would, and makes the daemon
			// price all three. It returns the reload's commands.
			revise := func() tea.Cmd {
				data := *m.cron.data
				data.runs = slices.Clone(data.runs)
				run := &data.runs[0]
				run.Revision++
				run.UpdatedAt = run.UpdatedAt.Add(time.Minute)
				run.Summary.Sessions = append(slices.Clone(run.Summary.Sessions), generated.SessionResponse{Agent: "claude", SessionID: "00000000-0000-4000-8000-000000000003", Step: new("fix")})
				third := generated.RunSession{Agent: "claude", SessionID: "00000000-0000-4000-8000-000000000003", State: generated.Priced, CostMicrodollars: new(int64(1205000)), CostSource: new("reported")}
				a.usage = generated.RunUsage{State: generated.Complete, CostMicrodollars: new(int64(3615000)), Sessions: append(cronUsageSessions("reported", 1205000, generated.Priced, generated.Priced), third)}
				var reload tea.Cmd
				m, reload = updateModel(m, cronLoadedMsg(data))
				return reload
			}
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
				require.Contains(t, body(), "Cost:       unavailable: daemon busy · r: refresh")
				require.NotContains(t, body(), "$2.41")
				require.Equal(t, []string{"01ARZ3NDEKTSV4RRFFQ69G5FAY false", "01ARZ3NDEKTSV4RRFFQ69G5FAY true"}, a.usageReads)
			case "newer state":
				m, _ = updateModel(m, first[0])
				require.Contains(t, body(), "Cost:       unavailable: unsupported usage state · r: refresh")
			case "incomplete list":
				m, _ = updateModel(m, first[0])
				require.Contains(t, body(), "Cost:       $2.41+ (session list incomplete)")
			case "revised after answer", "revised while pending":
				if name == "revised after answer" {
					m, _ = updateModel(m, first[0])
					require.Contains(t, body(), "Cost:       $2.41")
				}
				require.Empty(t, cronUsageRead(revise()), "a revised run is not read again until r")
				require.Contains(t, body(), "Cost:       unavailable: run changed · r: refresh")
				m, _ = updateModel(m, first[0])
				require.NotContains(t, body(), "$2.41")
				require.Contains(t, body(), "Cost:       unavailable: run changed · r: refresh")
				m, cmd = updateModel(m, keyRune('r'))
				revised := cronUsageRead(cmd)
				require.Len(t, revised, 1)
				m, _ = updateModel(m, revised[0])
				require.Contains(t, body(), "Cost:       $3.62")
				require.Equal(t, []string{"01ARZ3NDEKTSV4RRFFQ69G5FAY false", "01ARZ3NDEKTSV4RRFFQ69G5FAY true"}, a.usageReads)
			case "leave and reopen":
				// Switch to run B while A's read is pending; A's late answer
				// must not land on B.
				m = sendRune(m, 'h')
				m = sendRune(m, 'j')
				m, cmd = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
				other := cronUsageRead(cmd)
				require.Len(t, other, 1)
				m, _ = updateModel(m, first[0])
				require.Contains(t, body(), "runs / 01ARZ3NDEKTSV4RRFFQ69G5FAZ")
				require.Contains(t, body(), "Cost:       loading… · press r after it loads")
				m, _ = updateModel(m, other[0])
				require.Contains(t, body(), "Cost:       $2.41")
				m = sendRune(m, 'h')
				m = sendRune(m, 'k')
				m, cmd = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
				m, _ = updateModel(m, first[0])
				require.Contains(t, body(), "runs / 01ARZ3NDEKTSV4RRFFQ69G5FAY")
				require.Contains(t, body(), "Cost:       loading… · press r after it loads")
				var again tea.Cmd
				m, again = updateModel(m, keyRune('r'))
				require.Empty(t, cronUsageRead(again), "r waits for the first read")
				latest := cronUsageRead(cmd)
				require.Len(t, latest, 1)
				m, _ = updateModel(m, latest[0])
				require.Contains(t, body(), "Cost:       $2.41")
			case "project", "daemon":
				data := *m.cron.data
				if name == "daemon" {
					m.connGen++
				} else {
					m.scopeGen++
				}
				m, _ = updateModel(m, tea.FocusMsg{})
				m, _ = updateModel(m, first[0])
				require.Equal(t, cronUsage{}, m.cron.usage)
				// Reopening the same run after the reset starts its sequence
				// again, so the old answer differs only by daemon or project.
				data.conn, data.scope = m.connGen, m.scopeGen
				m, _ = updateModel(m, cronLoadedMsg(data))
				m, cmd = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
				reopened := cronUsageRead(cmd)
				require.Len(t, reopened, 1)
				m, _ = updateModel(m, first[0])
				require.Contains(t, body(), "Cost:       loading… · press r after it loads")
				m, _ = updateModel(m, reopened[0])
				require.Contains(t, body(), "Cost:       $2.41")
			case "row removed":
				data := *m.cron.data
				data.runs = data.runs[1:]
				m, _ = updateModel(m, cronLoadedMsg(data))
				m, _ = updateModel(m, first[0])
				require.Equal(t, cronUsage{}, m.cron.usage)
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
		{"complete", generated.RunUsage{State: generated.Complete, CostMicrodollars: cost, Sessions: cronUsageSessions("reported", 1205000, generated.Priced, generated.Priced)}},
		{"estimated", generated.RunUsage{State: generated.Complete, CostMicrodollars: cost, Estimated: true, Sessions: cronUsageSessions("computed", 1205000, generated.Priced, generated.Priced)}},
		{"partial", generated.RunUsage{State: generated.Partial, CostMicrodollars: cost, Estimated: true, Sessions: cronUsageSessions("computed", 2410000, generated.Priced, generated.NotFound)}},
		{"unknown", generated.RunUsage{State: generated.RunUsageStateUnknown, Sessions: cronUsageSessions("reported", 0, generated.RunSessionStateUnauthorized, generated.RunSessionStateUnavailable)}},
		{"no-sessions", generated.RunUsage{State: generated.NoSessions, Sessions: []generated.RunSession{}}},
		{"unavailable", generated.RunUsage{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, a, cmd := cronUsageModel(t, tc.usage)
			if tc.name == "unavailable" {
				a.usageErr = &APIError{Method: "GET", Path: "/api/v1/projects/7/cron/runs/01ARZ3NDEKTSV4RRFFQ69G5FAY/usage", Status: 502, Code: "usage_total_overflow", Message: "usage total cannot be represented"}
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
