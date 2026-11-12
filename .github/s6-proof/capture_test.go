package tui

// Transient S6 proof driver (never committed). It drives the real Model.Update
// and command loop with the real *Client against a synthetic httptest daemon,
// writing each frame's View() as ANSI plus a message/HTTP transcript.

import (
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	kataapi "go.kenn.io/kata/internal/api"
)

const s6Options = `{"inputs":{"batch_size":{"type":"int","title":"Batch size","required":true,"min":1,"max":500,"description":"Rows imported per transaction; larger batches finish sooner but hold locks longer."},"dry_run":{"type":"bool","title":"Dry run","default":true},"note":{"type":"string","max_length":200},"target":{"type":"enum","title":"Target","values":["production","staging"],"default":"staging"}}}`

const s6Inputs = `[
 {"name":"batch_size","type":"int","title":"Batch size","required":true,"min":1,"max":500,"description":"Rows imported per transaction; larger batches finish sooner but hold locks longer.","source":"unset"},
 {"name":"dry_run","type":"bool","title":"Dry run","value":true,"source":"default"},
 {"name":"note","type":"string","max_length":200,"source":"unset"},
 {"name":"target","type":"enum","title":"Target","values":["production","staging"],"value":"staging","source":"default"}]`

type s6Daemon struct {
	t          *testing.T
	mu         sync.Mutex
	log        io.Writer
	job        []byte
	eventUID   string
	jobUID     string
	inputsHold chan struct{}
	postHold   chan struct{}
	posts      []string // status for each POST in order
	postBodies []string
}

func (d *s6Daemon) logf(format string, args ...any) {
	d.mu.Lock()
	defer d.mu.Unlock()
	fmt.Fprintf(d.log, format+"\n", args...)
}

func (d *s6Daemon) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	path := r.URL.Path
	reply := func(status int, v string) {
		d.logf("HTTP %s %s -> %d", r.Method, r.URL.RequestURI(), status)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, v)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	jobs := "/api/v1/projects/7/cron/jobs"
	switch {
	case path == "/api/v1/health":
		reply(200, `{"ok":true,"api_schema_version":"0.49.0","schema_version":1,"started_at":"`+now+`","uptime":"1m","version":"dev"}`)
	case path == "/api/v1/projects/7/cron/capabilities":
		reply(200, `{"controls":["job_pause","project_pause","run_request","cancel_request"],"event_features":[],"project_pause":null,"project_revision":1,"project_uid":"01ARZ3NDEKTSV4RRFFQ69G5FC0"}`)
	case path == jobs:
		reply(200, `{"controls":["job_pause","project_pause","run_request","cancel_request"],"jobs":[`+string(d.job)+`],"passovers":{},"project_pause":null}`)
	case path == jobs+"/"+d.jobUID:
		reply(200, `{"job":`+string(d.job)+`}`)
	case path == jobs+"/"+d.jobUID+"/inputs":
		if d.inputsHold != nil {
			hold := d.inputsHold
			d.inputsHold = nil
			d.logf("HTTP %s %s held", r.Method, path)
			<-hold
		}
		reply(200, `{"job_uid":"`+d.jobUID+`","definition_event_uid":"`+d.eventUID+`","declared_by":"job","inputs":`+s6Inputs+`,"errors":[{"name":"batch_size","code":"missing_required","message":"required input has no value"}]}`)
	case path == jobs+"/"+d.jobUID+"/requests" && r.Method == http.MethodPost:
		d.logf("HTTP POST body %s", body)
		d.mu.Lock()
		d.postBodies = append(d.postBodies, string(body))
		d.mu.Unlock()
		if d.postHold != nil {
			hold := d.postHold
			d.postHold = nil
			d.logf("HTTP POST held")
			<-hold
		}
		if len(d.posts) == 0 {
			reply(500, `{"status":500,"error":{"code":"unexpected","message":"unexpected request"}}`)
			return
		}
		next := d.posts[0]
		d.posts = d.posts[1:]
		var in struct {
			UID string `json:"uid"`
		}
		_ = json.Unmarshal(body, &in)
		switch next {
		case "400":
			reply(400, `{"status":400,"error":{"code":"validation","message":"batch_size: invalid input value"}}`)
		case "503":
			reply(503, `{"status":503,"error":{"code":"hub_unavailable","message":"hub did not answer"}}`)
		default:
			reply(200, `{"replayed":false,"request":{"uid":"`+in.UID+`","kind":"run","job_uid":"`+d.jobUID+`","requested_by":"operator","requested_at":"`+now+`","expires_at":"`+now+`","state":"pending"}}`)
		}
	case path == "/api/v1/projects/7/cron/workflows":
		reply(200, `{"workflows":[]}`)
	case path == "/api/v1/projects/7/cron/runs":
		reply(200, `{"runs":[]}`)
	case path == "/api/v1/projects/7/cron/requests":
		reply(200, `{"requests":[]}`)
	default:
		reply(404, `{"status":404,"error":{"code":"not_found","message":"synthetic daemon"}}`)
	}
}

type s6Driver struct {
	t      *testing.T
	m      Model
	msgs   chan tea.Msg
	dir    string
	width  int
	frame  int
	log    io.Writer
	daemon *s6Daemon
}

func (d *s6Driver) run(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	go func() {
		msg := cmd()
		if batch, ok := msg.(tea.BatchMsg); ok {
			for _, c := range batch {
				d.run(c)
			}
			return
		}
		if msg != nil {
			d.msgs <- msg
		}
	}()
}

func (d *s6Driver) send(msg tea.Msg) {
	if _, ok := msg.(tea.KeyPressMsg); ok {
		d.daemon.logf("KEY %s", msg.(tea.KeyPressMsg).String())
	} else {
		d.daemon.logf("MSG %T", msg)
	}
	next, cmd := d.m.Update(msg)
	d.m = next.(Model)
	d.run(cmd)
}

// settle delivers command results until none arrive for a quiet period.
func (d *s6Driver) settle() {
	for {
		select {
		case msg := <-d.msgs:
			d.send(msg)
		case <-time.After(400 * time.Millisecond):
			return
		}
	}
}

func (d *s6Driver) keys(keys ...tea.KeyPressMsg) {
	for _, k := range keys {
		d.send(k)
		d.settle()
	}
}

func (d *s6Driver) capture(name, caption string) {
	d.frame++
	base := fmt.Sprintf("%02d-%s-%d", d.frame, name, d.width)
	content := d.m.View().Content
	if err := os.WriteFile(filepath.Join(d.dir, base+".ansi"), []byte(content), 0o600); err != nil {
		d.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d.dir, base+".caption"), []byte(caption), 0o600); err != nil {
		d.t.Fatal(err)
	}
	d.daemon.logf("FRAME %s: %s", base, caption)
}

func s6Start(t *testing.T, width int, dir string) *s6Driver {
	t.Helper()
	applyDefaultColorMode()
	rec := executeRecord(s6Options)
	rec.Name = "Nightly import"
	rec.ProjectID = 7
	raw, err := json.Marshal(kataapi.CronJobFrom(rec))
	if err != nil {
		t.Fatal(err)
	}
	logFile, err := os.Create(filepath.Join(dir, fmt.Sprintf("transcript-%d.txt", width)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = logFile.Close() })
	daemon := &s6Daemon{t: t, log: logFile, job: raw, eventUID: rec.DefinitionEventUID, jobUID: rec.UID}
	srv := httptest.NewServer(daemon)
	t.Cleanup(srv.Close)
	m := cronFixture(t)
	m.api = NewClient(srv.URL, srv.Client())
	m.list.actor = "operator"
	m.view, m.cron.tab = viewCron, cronJobs
	m.cron.data, m.cron.out = nil, false
	m.cron.events++
	d := &s6Driver{t: t, m: m, msgs: make(chan tea.Msg, 64), dir: dir, width: width, log: logFile, daemon: daemon}
	d.send(tea.WindowSizeMsg{Width: width, Height: 24})
	d.settle()
	return d
}

func s6Key(r rune) tea.KeyPressMsg { return tea.KeyPressMsg{Code: r, Text: string(r)} }
func s6Ctrl(r rune) tea.KeyPressMsg { return tea.KeyPressMsg{Code: r, Mod: tea.ModCtrl} }

var (
	s6Tab   = tea.KeyPressMsg{Code: tea.KeyTab}
	s6Left  = tea.KeyPressMsg{Code: tea.KeyLeft}
	s6Right = tea.KeyPressMsg{Code: tea.KeyRight}
	s6Back  = tea.KeyPressMsg{Code: tea.KeyBackspace}
)

func TestS6CaptureFrames(t *testing.T) {
	dir := os.Getenv("S6_FRAMES")
	if dir == "" {
		t.Skip("S6_FRAMES unset")
	}
	for _, width := range []int{80, 60} {
		d := s6Start(t, width, dir)
		d.capture("list", "Jobs tab: Nightly import selected")
		d.daemon.inputsHold = make(chan struct{})
		release := d.daemon.inputsHold
		d.send(s6Key('R'))
		time.Sleep(200 * time.Millisecond)
		d.capture("loading", "R: reading the job's inputs")
		close(release)
		d.settle()
		d.capture("required", "Form opens: Batch size is required")
		d.keys(s6Tab, s6Right)
		d.capture("bool", "tab, →: Dry run true → false")
		d.keys(s6Tab, s6Key('n'), s6Key('i'), s6Key('g'), s6Key('h'), s6Key('t'), s6Key('l'), s6Key('y'))
		d.capture("string", "tab, type: Note \"nightly\"")
		d.keys(s6Tab, s6Left)
		d.capture("enum", "tab, ←: Target staging → production")
		d.keys(s6Tab, s6Key('9'), s6Key('0'), s6Key('0'))
		d.capture("int", "tab, type 900: Batch size above its limit")
		d.keys(s6Ctrl('o'))
		d.capture("invalid", "ctrl+o: refused locally, nothing sent")
		d.keys(s6Back, s6Back, s6Back, s6Key('5'), s6Key('0'))
		d.daemon.posts = []string{"400", "200", "503"}
		d.daemon.postHold = make(chan struct{})
		post := d.daemon.postHold
		d.send(s6Ctrl('o'))
		time.Sleep(300 * time.Millisecond)
		d.capture("submitting", "Batch size 50, ctrl+o: sending request")
		close(post)
		d.settle()
		d.capture("refused", "Hub answers 400: draft kept, edit and resend")
		d.keys(s6Back, s6Key('4'))
		d.capture("edited", "backspace, 4: Batch size 54")
		d.keys(s6Ctrl('o'))
		d.capture("success", "ctrl+o: request accepted, back to Jobs")
		d.keys(s6Key('R'), s6Key('2'), s6Key('5'), s6Ctrl('o'))
		d.capture("retry", "R, 25, ctrl+o: no answer; values locked")
		d.daemon.logf("POSTS %d", len(d.daemon.postBodies))
	}
}

// TestS6CaptureBefore runs on the baseline: the same job and viewport, R
// opens the existing confirmation.
func TestS6CaptureBefore(t *testing.T) {
	dir := os.Getenv("S6_FRAMES")
	if dir == "" {
		t.Skip("S6_FRAMES unset")
	}
	for _, width := range []int{80, 60} {
		d := s6Start(t, width, dir)
		d.capture("list", "Before: Jobs tab, Nightly import selected")
		d.keys(s6Key('R'))
		d.capture("confirm", "Before: R asks to confirm; no input values")
	}
}
