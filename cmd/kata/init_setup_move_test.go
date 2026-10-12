package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/uid"
)

type setupKickoffFixture struct {
	source, destination             *testenv.Env
	root, child                     db.Issue
	sourceFaults, destinationFaults *setupClientFaults
	ctx                             context.Context
	dir                             string
	destinationURL                  string
}

func newSetupKickoffFixture(t *testing.T) setupKickoffFixture {
	t.Helper()
	resetFlags(t)
	flags.Mode = outputJSON
	destination := testenv.New(t, testenv.WithAuthToken("destination-token"))
	destinationProxy, destinationFaults := newSetupFaultProxy(t, destination.URL)
	source := testenv.New(t, testenv.WithAuthToken("source-token"), func(cfg *daemon.ServerConfig) {
		cfg.FederationCatalog = []config.CatalogDaemonConfig{{Name: "destination-service", URL: destinationProxy.URL, Token: "destination-token", InstanceUID: destination.DB.InstanceUID()}}
	})
	sourceProxy, sourceFaults := newSetupFaultProxy(t, source.URL)
	project, err := source.DB.CreateProject(t.Context(), "intake-project")
	require.NoError(t, err)
	root, _, err := source.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Kickoff", Author: "setup-author"})
	require.NoError(t, err)
	child, _, err := source.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Completed child", Author: "setup-author"})
	require.NoError(t, err)
	_, err = source.DB.CreateLink(t.Context(), db.CreateLinkParams{FromIssueID: child.ID, ToIssueID: root.ID, Type: "parent", Author: "setup-author"})
	require.NoError(t, err)
	_, _, _, err = source.DB.CloseIssue(t.Context(), child.ID, "done", "setup-author", "", nil)
	require.NoError(t, err)
	dir := setupTestWorkspace(t)
	require.NoError(t, config.WriteProjectConfig(dir, project.Name))
	catalog := fmt.Sprintf("[[daemon]]\nname=\"source-service\"\nurl=%q\ntoken=\"source-token\"\n[[daemon]]\nname=\"destination-service\"\nurl=%q\ntoken=\"destination-token\"\n", sourceProxy.URL, destinationProxy.URL)
	require.NoError(t, os.WriteFile(filepath.Join(source.Home, "config.toml"), []byte(catalog), 0600))
	t.Setenv("KATA_AUTH_TOKEN", "destination-token")
	selected, err := client.PrepareResolvedNamed(t.Context(), "destination-service")
	require.NoError(t, err)
	return setupKickoffFixture{source: source, destination: destination, root: root, child: child, sourceFaults: sourceFaults, destinationFaults: destinationFaults, ctx: context.WithValue(t.Context(), resolvedDaemonContextKey{}, selected), dir: dir, destinationURL: destinationProxy.URL}
}

func (f setupKickoffFixture) start() (string, error) {
	return callInit(f.ctx, f.destinationURL, f.dir, callInitOpts{Project: "example-project", Replace: true, Kickoff: f.root.UID, SourceDaemon: "source-service", Actor: "setup-author"})
}

// Failures surround actual receiver and source transactions. No move outcome
// is mocked: the tests inspect the resulting disposable database copies.
func TestInitSetupKickoffInterruption(t *testing.T) {
	for _, fault := range []string{"lost-ack", "missing-proof", "source-finalization", "unreachable-acceptance"} {
		t.Run(fault, func(t *testing.T) {
			f := newSetupKickoffFixture(t)
			expectedState := "outcome-unknown"
			switch fault {
			case "lost-ack", "missing-proof":
				f.sourceFaults.moveFault = fault
			case "source-finalization":
				expectedState = "partial"
				// Ephemeral fault injection only; product DDL is unchanged.
				_, err := f.source.DB.ExecContext(t.Context(), `CREATE TRIGGER block_kickoff_finalize BEFORE INSERT ON events WHEN NEW.type='issue.moved' BEGIN SELECT RAISE(ABORT,'injected finalization failure'); END`)
				require.NoError(t, err)
			case "unreachable-acceptance":
				f.destinationFaults.blockTransfer = true
			}
			_, err := f.start()
			var ce *cliError
			require.ErrorAs(t, err, &ce)
			require.Equal(t, "kickoff_move_"+expectedState, ce.Code)
			var data map[string]any
			require.NoError(t, json.Unmarshal(ce.Data, &data))
			require.Equal(t, "bound", data["setup_state"])
			require.Equal(t, expectedState, data["kickoff_state"])
			require.NotEmpty(t, ce.Hint)
			selected, err := client.PrepareResolvedInWorkspace(t.Context(), f.dir)
			require.NoError(t, err)
			require.Equal(t, f.destinationURL, selected.BaseURL)
			cfg, err := config.ReadProjectConfig(f.dir)
			require.NoError(t, err)
			require.Equal(t, "example-project", cfg.Project.Name)
			before, err := os.ReadFile(filepath.Join(f.dir, setupProgressFilename))
			require.NoError(t, err)
			if fault != "unreachable-acceptance" {
				for _, member := range []db.Issue{f.root, f.child} {
					_, err := f.destination.DB.IssueByUID(t.Context(), member.UID, db.IncludeDeletedYes)
					require.NoError(t, err, "destination actually committed")
				}
			}
			if fault == "source-finalization" {
				_, err = f.source.DB.ExecContext(t.Context(), `DROP TRIGGER block_kickoff_finalize`)
				require.NoError(t, err)
			}
			f.sourceFaults.mu.Lock()
			f.sourceFaults.moveFault = ""
			f.sourceFaults.mu.Unlock()
			f.destinationFaults.mu.Lock()
			f.destinationFaults.blockTransfer = false
			f.destinationFaults.mu.Unlock()
			out, err := callInit(f.ctx, f.destinationURL, f.dir, callInitOpts{Actor: "setup-author"})
			require.NoError(t, err)
			var response map[string]any
			require.NoError(t, json.Unmarshal([]byte(out), &response))
			result := response["kickoff"].(map[string]any)
			require.Equal(t, "completed", result["state"])
			require.Len(t, result["applied"], 2)
			require.Len(t, result["finalized"], 2)
			after, err := os.ReadFile(filepath.Join(f.dir, setupProgressFilename))
			require.NoError(t, err)
			require.Equal(t, before, after, "retry retains the complete request inputs")
			eventID, err := f.destination.DB.MaxEventID(t.Context())
			require.NoError(t, err)
			_, err = callInit(f.ctx, f.destinationURL, f.dir, callInitOpts{Actor: "setup-author"})
			require.NoError(t, err)
			afterEventID, err := f.destination.DB.MaxEventID(t.Context())
			require.NoError(t, err)
			require.Equal(t, eventID, afterEventID)
		})
	}
}

func TestInitSetupRejectsConflictingServerEnvironment(t *testing.T) {
	f := newSetupKickoffFixture(t)
	t.Setenv("KATA_SERVER", f.source.URL)
	_, err := f.start()
	var ce *cliError
	require.ErrorAs(t, err, &ce)
	require.Equal(t, "setup_route_conflict", ce.Code)
	cfg, err := config.ReadProjectConfig(f.dir)
	require.NoError(t, err)
	require.Equal(t, "intake-project", cfg.Project.Name)
	_, err = f.destination.DB.ProjectByName(t.Context(), "example-project")
	require.ErrorIs(t, err, db.ErrNotFound)
}

func TestInitSetupSourceSelectorRequiresKickoff(t *testing.T) {
	resetFlags(t)
	flags.Mode = outputJSON
	env := testenv.New(t)
	dir := setupTestWorkspace(t)
	_, err := callInit(t.Context(), env.URL, dir, callInitOpts{Setup: true, Project: "example-project", Actor: "setup-author"})
	require.NoError(t, err)
	_, err = callInit(t.Context(), env.URL, dir, callInitOpts{Setup: true, SourceDaemon: "missing-source", Actor: "setup-author"})
	var ce *cliError
	require.ErrorAs(t, err, &ce)
	require.Equal(t, kindUsage, ce.Kind)
}

func TestInitSetupLocalProfileBinding(t *testing.T) {
	resetFlags(t)
	flags.Mode = outputJSON
	env := testenv.New(t)
	dir := setupTestWorkspace(t)
	catalog := fmt.Sprintf("[[daemon]]\nname=\"destination-profile\"\nlocal=true\nhome=%q\ninstance_uid=%q\n", env.Home, env.DB.InstanceUID())
	require.NoError(t, os.WriteFile(filepath.Join(env.Home, "config.toml"), []byte(catalog), 0600))
	selection, err := client.InspectSelection(t.Context(), dir, "destination-profile")
	require.NoError(t, err)
	selected := selection.Resolved.WithRunning(client.RunningDaemon{BaseURL: env.URL, Network: "tcp", Scheme: "http"})
	ctx := context.WithValue(t.Context(), resolvedDaemonContextKey{}, selected)
	_, err = callInit(ctx, env.URL, dir, callInitOpts{Setup: true, Project: "example-project", Actor: "setup-author"})
	require.NoError(t, err)
	local, err := config.ReadLocalConfig(dir)
	require.NoError(t, err)
	require.Equal(t, "destination-profile", local.Server.Daemon)
	require.Empty(t, local.Server.URL)
	bound, err := client.InspectSelection(t.Context(), dir, "")
	require.NoError(t, err)
	require.Equal(t, env.DB.InstanceUID(), bound.Profile.InstanceUID)
}

func TestInitSetupKickoffRequestSavedBeforeLostCreateReply(t *testing.T) {
	f := newSetupKickoffFixture(t)
	f.destinationFaults.dropAck = true
	_, err := f.start()
	require.Error(t, err)
	progress := readSetupTestProgress(t, f.dir)
	require.NotNil(t, progress["kickoff_request"], "complete kickoff inputs must survive a lost project create reply")
	request := progress["kickoff_request"].(map[string]any)["request"].(map[string]any)
	require.Equal(t, progress["request_uid"], request["request_uid"])
	require.Equal(t, f.root.UID, request["issue_uid"])
	f.destinationFaults.mu.Lock()
	f.destinationFaults.dropAck = false
	f.destinationFaults.mu.Unlock()
	_, err = callInit(f.ctx, f.destinationURL, f.dir, callInitOpts{Actor: "setup-author"})
	require.NoError(t, err)
	projects, err := f.destination.DB.ListProjects(t.Context())
	require.NoError(t, err)
	require.Len(t, projects, 1)
}

func TestInitSetupKickoffRejectsChangedSavedRequest(t *testing.T) {
	f := newSetupKickoffFixture(t)
	f.sourceFaults.moveFault = "lost-ack"
	_, err := f.start()
	require.Error(t, err)
	progress := readSetupTestProgress(t, f.dir)
	changedUID, err := uid.New()
	require.NoError(t, err)
	progress["kickoff_request"].(map[string]any)["request"].(map[string]any)["request_uid"] = changedUID
	bs, err := json.Marshal(progress)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(f.dir, setupProgressFilename), bs, 0600))
	f.sourceFaults.mu.Lock()
	f.sourceFaults.moveFault = ""
	f.sourceFaults.mu.Unlock()
	_, err = callInit(f.ctx, f.destinationURL, f.dir, callInitOpts{Actor: "setup-author"})
	var ce *cliError
	require.ErrorAs(t, err, &ce)
	require.Equal(t, "setup_progress_invalid", ce.Code)
}

func TestInitSetupKickoffSeriesAndProviderOptions(t *testing.T) {
	f := newSetupKickoffFixture(t)
	attached, err := f.source.DB.CreateRecurrenceForIssue(t.Context(), db.CreateRecurrenceForIssueIn{IssueID: f.root.ID, Recurrence: db.CreateRecurrenceIn{ProjectID: f.root.ProjectID, Actor: "setup-author", Rule: "FREQ=DAILY", DTStart: "2026-10-12", Timezone: "UTC", Template: db.RecurrenceTemplate{Title: "Daily task"}}})
	require.NoError(t, err)
	_, err = f.source.DB.MaterializeNext(t.Context(), attached.Recurrence.ID, "2026-10-13", "scheduler")
	require.NoError(t, err)
	out, _, err := executeRootCapture(t, f.ctx, "--daemon", "destination-service", "--workspace", f.dir, "--json", "init", "--project", "example-project", "--replace", "--kickoff", f.root.UID, "--source-daemon", "source-service", "--move-series", "--provider-disposition", "detach")
	require.NoError(t, err)
	var response map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &response))
	require.Len(t, response["kickoff"].(map[string]any)["applied"], 3)
	progress := readSetupTestProgress(t, f.dir)
	request := progress["kickoff_request"].(map[string]any)["request"].(map[string]any)
	require.Equal(t, true, request["move_series"])
	require.Equal(t, "detach", request["provider_disposition"])
	var projectID int64
	require.NoError(t, f.destination.DB.QueryRowContext(t.Context(), `SELECT project_id FROM recurrences WHERE uid=$1`, attached.Recurrence.UID).Scan(&projectID))
	project, err := f.destination.DB.ProjectByName(t.Context(), "example-project")
	require.NoError(t, err)
	require.Equal(t, project.ID, projectID)
}

func TestInitSetupResumedBareKickoffUsesOriginalBinding(t *testing.T) {
	f := newSetupKickoffFixture(t)
	_, err := callInit(f.ctx, f.destinationURL, f.dir, callInitOpts{Project: "example-project", Setup: true, Replace: true, Actor: "setup-author"})
	require.NoError(t, err)
	_, err = callInit(f.ctx, f.destinationURL, f.dir, callInitOpts{Project: "example-project", Replace: true, Kickoff: f.root.ShortID, SourceDaemon: "source-service", Actor: "setup-author"})
	require.NoError(t, err, "a bare kickoff added after setup resolves against the retained original binding")
	moved, err := f.destination.DB.IssueByUID(t.Context(), f.root.UID, db.IncludeDeletedYes)
	require.NoError(t, err)
	target, err := f.destination.DB.ProjectByID(t.Context(), moved.ProjectID)
	require.NoError(t, err)
	require.Equal(t, "example-project", target.Name)
}
