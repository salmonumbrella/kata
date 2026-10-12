package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/uid"
)

const setupProgressTestName = ".kata.setup-progress.json"

func setupTestWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "--quiet")
	return dir
}
func readSetupTestProgress(t *testing.T, dir string) map[string]any {
	t.Helper()
	bs, err := os.ReadFile(filepath.Join(dir, setupProgressTestName))
	require.NoError(t, err)
	var p map[string]any
	require.NoError(t, json.Unmarshal(bs, &p))
	return p
}

// A create request must use the UID already published durably in ignored
// client progress. The real daemon still performs creation and alias writes.
func TestInitSetupSavesIgnoredIdentityBeforeCreate(t *testing.T) {
	resetFlags(t)
	flags.Mode = outputJSON
	env := testenv.New(t)
	dir := setupTestWorkspace(t)
	seen := false
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/projects" {
			p := readSetupTestProgress(t, dir)
			dest := p["destination"].(map[string]any)
			require.Equal(t, env.DB.InstanceUID(), dest["authority_uid"])
			require.True(t, uid.Valid(dest["project_uid"].(string)))
			reqBody, err := io.ReadAll(r.Body)
			require.NoError(t, err)
			var request map[string]any
			require.NoError(t, json.Unmarshal(reqBody, &request))
			require.Equal(t, dest["project_uid"], request["project_uid"])
			check := exec.Command("git", "-C", dir, "check-ignore", "--quiet", setupProgressTestName)
			require.NoError(t, check.Run())
			seen = true
			r.Body = io.NopCloser(bytes.NewReader(reqBody))
		}
		target := env.URL + r.URL.RequestURI()
		req, err := http.NewRequestWithContext(r.Context(), r.Method, target, r.Body)
		require.NoError(t, err)
		req.Header = r.Header.Clone()
		response, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer response.Body.Close()
		for key, values := range response.Header {
			for _, v := range values {
				w.Header().Add(key, v)
			}
		}
		w.WriteHeader(response.StatusCode)
		_, err = io.Copy(w, response.Body)
		require.NoError(t, err)
	}))
	t.Cleanup(proxy.Close)
	_, err := callInit(t.Context(), proxy.URL, dir, callInitOpts{Setup: true, Project: "example-project", Actor: "setup-author"})
	require.NoError(t, err)
	require.True(t, seen)
	p := readSetupTestProgress(t, dir)
	dest := p["destination"].(map[string]any)
	project, err := env.DB.ProjectByUID(t.Context(), dest["project_uid"].(string))
	require.NoError(t, err)
	require.Equal(t, "example-project", project.Name)
	cfg, err := config.ReadProjectConfig(dir)
	require.NoError(t, err)
	require.Equal(t, project.Name, cfg.Project.Name)
}

func TestInitSetupReusesExistingDedicatedProject(t *testing.T) {
	resetFlags(t)
	flags.Mode = outputJSON
	env := testenv.New(t)
	dir := setupTestWorkspace(t)
	project, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	require.NoError(t, config.WriteProjectConfig(dir, project.Name))
	before, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)
	_, err = callInit(t.Context(), env.URL, dir, callInitOpts{Setup: true, Actor: "setup-author"})
	require.NoError(t, err)
	p := readSetupTestProgress(t, dir)
	dest := p["destination"].(map[string]any)
	require.Equal(t, project.UID, dest["project_uid"])
	after, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)
	require.Equal(t, before, after)
}

func TestInitSetupConcurrentRerunsShareIdentity(t *testing.T) {
	resetFlags(t)
	flags.Mode = outputJSON
	env := testenv.New(t)
	dir := setupTestWorkspace(t)
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, 3)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[i] = callInit(context.Background(), env.URL, dir, callInitOpts{Setup: true, Project: "example-project", Actor: "setup-author"})
		}()
	}
	close(start)
	wg.Wait()
	for _, err := range errs {
		require.NoError(t, err)
	}
	p := readSetupTestProgress(t, dir)
	dest := p["destination"].(map[string]any)
	projects, err := env.DB.ListProjects(t.Context())
	require.NoError(t, err)
	require.Len(t, projects, 1)
	require.Equal(t, projects[0].UID, dest["project_uid"])
	events, err := env.DB.EventsAfter(t.Context(), db.EventsAfterParams{ProjectID: projects[0].ID, Limit: 10})
	require.NoError(t, err)
	require.Len(t, events, 1)
}

// This proxy forwards every operation to an actual daemon. Faults affect only
// acknowledgement delivery or response bytes, never mock a committed write.
type setupClientFaults struct {
	mu            sync.Mutex
	target        string
	dropAck       bool
	wrongUID      string
	projectUIDs   []string
	moveFault     string
	blockTransfer bool
}

func newSetupFaultProxy(t *testing.T, target string) (*httptest.Server, *setupClientFaults) {
	t.Helper()
	faults := &setupClientFaults{target: target}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bs, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		faults.mu.Lock()
		target := faults.target
		drop := faults.dropAck
		wrong := faults.wrongUID
		moveFault, blockTransfer := faults.moveFault, faults.blockTransfer
		isCreate := r.Method == "POST" && r.URL.Path == "/api/v1/projects"
		if isCreate {
			var body map[string]any
			_ = json.Unmarshal(bs, &body)
			projectUID, _ := body["project_uid"].(string)
			faults.projectUIDs = append(faults.projectUIDs, projectUID)
		}
		faults.mu.Unlock()
		if blockTransfer && strings.HasPrefix(r.URL.Path, "/api/v1/issue-moves/") {
			http.Error(w, "injected unavailable transfer", http.StatusServiceUnavailable)
			return
		}
		var moveBody struct {
			DryRun bool `json:"dry_run"`
		}
		_ = json.Unmarshal(bs, &moveBody)
		isMove := r.Method == "POST" && r.URL.Path == "/api/v1/issue-moves" && !moveBody.DryRun
		request, err := http.NewRequestWithContext(r.Context(), r.Method, target+r.URL.RequestURI(), bytes.NewReader(bs))
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		request.Header = r.Header.Clone()
		request.Header.Del("Accept-Encoding") // Let this transport decompress before response fault injection.
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			http.Error(w, err.Error(), 502)
			return
		}
		defer response.Body.Close()
		out, err := io.ReadAll(response.Body)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		if (isCreate && drop) || (isMove && moveFault == "lost-ack" && response.StatusCode == 200) {
			connection, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = connection.Close()
			}
			return
		}
		if isCreate && wrong != "" && response.StatusCode == 200 {
			var body map[string]any
			if json.Unmarshal(out, &body) == nil {
				body["project"].(map[string]any)["uid"] = wrong
				out, _ = json.Marshal(body)
			}
		}
		if isMove && moveFault == "missing-proof" && response.StatusCode == 200 {
			var body map[string]any
			if decodeErr := json.Unmarshal(out, &body); decodeErr == nil {
				body["result"].(map[string]any)["applied"].([]any)[0].(map[string]any)["committed_event_hash"] = ""
				out, _ = json.Marshal(body)
			} else {
				t.Errorf("decode actual response before fault injection: %v", decodeErr)
			}
		}
		for key, values := range response.Header {
			if strings.EqualFold(key, "Content-Length") {
				continue
			}
			for _, v := range values {
				w.Header().Add(key, v)
			}
		}
		w.WriteHeader(response.StatusCode)
		_, _ = w.Write(out)
	}))
	t.Cleanup(proxy.Close)
	return proxy, faults
}

func TestInitSetupLostAcknowledgementResumesSavedUID(t *testing.T) {
	resetFlags(t)
	flags.Mode = outputJSON
	env := testenv.New(t)
	dir := setupTestWorkspace(t)
	proxy, faults := newSetupFaultProxy(t, env.URL)
	faults.dropAck = true
	_, err := callInit(t.Context(), proxy.URL, dir, callInitOpts{Setup: true, Project: "example-project", Actor: "setup-author"})
	require.Error(t, err)
	p := readSetupTestProgress(t, dir)
	savedUID := p["destination"].(map[string]any)["project_uid"].(string)
	_, err = env.DB.ProjectByUID(t.Context(), savedUID)
	require.NoError(t, err, "the daemon committed before the acknowledgement was lost")
	_, err = config.ReadProjectConfig(dir)
	require.ErrorIs(t, err, config.ErrProjectConfigMissing, "never bind an unacknowledged identity")
	faults.mu.Lock()
	faults.dropAck = false
	faults.mu.Unlock()
	// No repeated setup/project flag: the saved client input controls resumption.
	_, err = callInit(t.Context(), proxy.URL, dir, callInitOpts{Actor: "setup-author"})
	require.NoError(t, err)
	faults.mu.Lock()
	requests := append([]string(nil), faults.projectUIDs...)
	faults.mu.Unlock()
	require.GreaterOrEqual(t, len(requests), 2)
	for _, id := range requests {
		require.Equal(t, savedUID, id)
	}
	projects, err := env.DB.ListProjects(t.Context())
	require.NoError(t, err)
	require.Len(t, projects, 1)
	events, err := env.DB.EventsAfter(t.Context(), db.EventsAfterParams{ProjectID: projects[0].ID, Limit: 10})
	require.NoError(t, err)
	require.Len(t, events, 1)
}

func TestInitSetupSavedAuthorityCannotChange(t *testing.T) {
	resetFlags(t)
	flags.Mode = outputJSON
	first := testenv.New(t)
	dir := setupTestWorkspace(t)
	proxy, faults := newSetupFaultProxy(t, first.URL)
	_, err := callInit(t.Context(), proxy.URL, dir, callInitOpts{Setup: true, Project: "example-project", Actor: "setup-author"})
	require.NoError(t, err)
	before, err := os.ReadFile(filepath.Join(dir, setupProgressTestName))
	require.NoError(t, err)
	second := testenv.New(t)
	faults.mu.Lock()
	faults.target = second.URL
	faults.mu.Unlock()
	_, err = callInit(t.Context(), proxy.URL, dir, callInitOpts{Actor: "setup-author"})
	require.Error(t, err)
	var ce *cliError
	require.ErrorAs(t, err, &ce)
	require.Equal(t, "setup_authority_conflict", ce.Code)
	after, err := os.ReadFile(filepath.Join(dir, setupProgressTestName))
	require.NoError(t, err)
	require.Equal(t, before, after)
	projects, err := second.DB.ListProjects(t.Context())
	require.NoError(t, err)
	require.Empty(t, projects)
	cfg, err := config.ReadProjectConfig(dir)
	require.NoError(t, err)
	require.Equal(t, "example-project", cfg.Project.Name)
}

func TestInitSetupMismatchedResponseNeverBinds(t *testing.T) {
	resetFlags(t)
	flags.Mode = outputJSON
	env := testenv.New(t)
	dir := setupTestWorkspace(t)
	proxy, faults := newSetupFaultProxy(t, env.URL)
	wrong, err := uid.New()
	require.NoError(t, err)
	faults.wrongUID = wrong
	_, err = callInit(t.Context(), proxy.URL, dir, callInitOpts{Setup: true, Project: "example-project", Actor: "setup-author"})
	require.Error(t, err)
	var ce *cliError
	require.ErrorAs(t, err, &ce)
	require.Equal(t, "setup_project_conflict", ce.Code)
	_, err = config.ReadProjectConfig(dir)
	require.ErrorIs(t, err, config.ErrProjectConfigMissing)
}

func TestInitSetupCorruptProgressNeverCreates(t *testing.T) {
	resetFlags(t)
	flags.Mode = outputJSON
	env := testenv.New(t)
	dir := setupTestWorkspace(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, setupProgressTestName), []byte("{broken"), 0600))
	_, err := callInit(t.Context(), env.URL, dir, callInitOpts{Project: "example-project", Actor: "setup-author"})
	require.Error(t, err)
	projects, err := env.DB.ListProjects(t.Context())
	require.NoError(t, err)
	require.Empty(t, projects)
	bs, err := os.ReadFile(filepath.Join(dir, setupProgressTestName))
	require.NoError(t, err)
	require.Equal(t, "{broken", string(bs))
}

func TestInitSetupCLIFlag(t *testing.T) {
	env := testenv.New(t)
	dir := setupTestWorkspace(t)
	ctx := context.WithValue(t.Context(), client.BaseURLKey{}, env.URL)
	_, _, err := executeRootCapture(t, ctx, "--workspace", dir, "--json", "init", "--project", "example-project", "--setup")
	require.NoError(t, err)
	readSetupTestProgress(t, dir)
}

// Bare kickoff refs belong to the original binding, even when the same short
// ID exists in the intended destination and --project selects that destination.
func TestInitSetupCapturesKickoffBeforeBinding(t *testing.T) {
	resetFlags(t)
	flags.Mode = outputJSON
	env := testenv.New(t)
	dir := setupTestWorkspace(t)
	original, err := env.DB.CreateProject(t.Context(), "intake-project")
	require.NoError(t, err)
	target, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	kickoff, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: original.ID, UID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", Title: "Kickoff", Author: "setup-author"})
	require.NoError(t, err)
	collision, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: target.ID, UID: "01ARZ3NDEKTSV4RRFFQ69H5FAV", Title: "Another task", Author: "setup-author"})
	require.NoError(t, err)
	require.Equal(t, kickoff.ShortID, collision.ShortID)
	require.NoError(t, config.WriteProjectConfig(dir, original.Name))
	_, err = callInit(t.Context(), env.URL, dir, callInitOpts{Project: target.Name, Replace: true, Kickoff: kickoff.ShortID, Actor: "setup-author"})
	require.NoError(t, err)
	progress := readSetupTestProgress(t, dir)
	source := progress["source"].(map[string]any)
	require.Equal(t, "01ARZ3NDEKTSV4RRFFQ69G5FAV", source["issue_uid"])
	require.Equal(t, original.UID, source["project_uid"])
	require.Equal(t, env.DB.InstanceUID(), source["authority_uid"])
	cfg, err := config.ReadProjectConfig(dir)
	require.NoError(t, err)
	require.Equal(t, target.Name, cfg.Project.Name)
	before, err := os.ReadFile(filepath.Join(dir, setupProgressTestName))
	require.NoError(t, err)
	_, err = callInit(t.Context(), env.URL, dir, callInitOpts{Actor: "setup-author"})
	require.NoError(t, err)
	after, err := os.ReadFile(filepath.Join(dir, setupProgressTestName))
	require.NoError(t, err)
	require.Equal(t, before, after)
	unchanged, err := env.DB.IssueByUID(t.Context(), kickoff.UID, db.IncludeDeletedYes)
	require.NoError(t, err)
	require.Equal(t, target.ID, unchanged.ProjectID)
}

func TestInitSetupMissingKickoffPreservesBinding(t *testing.T) {
	resetFlags(t)
	flags.Mode = outputJSON
	env := testenv.New(t)
	dir := setupTestWorkspace(t)
	original, err := env.DB.CreateProject(t.Context(), "intake-project")
	require.NoError(t, err)
	require.NoError(t, config.WriteProjectConfig(dir, original.Name))
	_, err = callInit(t.Context(), env.URL, dir, callInitOpts{Project: "example-project", Replace: true, Kickoff: "01ARZ3NDEKTSV4RRFFQ69G5FAV", Actor: "setup-author"})
	require.Error(t, err)
	cfg, err := config.ReadProjectConfig(dir)
	require.NoError(t, err)
	require.Equal(t, original.Name, cfg.Project.Name)
	_, err = env.DB.ProjectByName(t.Context(), "example-project")
	require.ErrorIs(t, err, db.ErrNotFound)
}

func TestInitSetupExplicitAliasReassignmentKeepsVerifiedDestination(t *testing.T) {
	resetFlags(t)
	flags.Mode = outputJSON
	env := testenv.New(t)
	dir := setupTestWorkspace(t)
	_, err := callInit(t.Context(), env.URL, dir, callInitOpts{Project: "intake-project", Actor: "setup-author"})
	require.NoError(t, err)
	original, err := env.DB.ProjectByName(t.Context(), "intake-project")
	require.NoError(t, err)
	kickoff, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: original.ID, Title: "Kickoff", Author: "setup-author"})
	require.NoError(t, err)
	_, err = callInit(t.Context(), env.URL, dir, callInitOpts{Project: "example-project", Replace: true, Reassign: true, Kickoff: kickoff.ShortID, Actor: "setup-author"})
	require.NoError(t, err)
	target, err := env.DB.ProjectByName(t.Context(), "example-project")
	require.NoError(t, err)
	alias, err := env.DB.AliasByIdentity(t.Context(), "local://"+dir)
	require.NoError(t, err)
	require.Equal(t, target.ID, alias.ProjectID)
	cfg, err := config.ReadProjectConfig(dir)
	require.NoError(t, err)
	require.Equal(t, target.Name, cfg.Project.Name)
	progress := readSetupTestProgress(t, dir)
	require.Equal(t, kickoff.UID, progress["source"].(map[string]any)["issue_uid"])
}

func TestInitSetupNamedSourceAndDestinationKeepCredentialsScoped(t *testing.T) {
	resetFlags(t)
	flags.Mode = outputJSON
	targetEnv := testenv.New(t, testenv.WithAuthToken("destination-token-for-test"))
	sourceEnv := testenv.New(t, testenv.WithAuthToken("source-token-for-test"), func(cfg *daemon.ServerConfig) {
		cfg.FederationCatalog = []config.CatalogDaemonConfig{{Name: "destination-service", URL: targetEnv.URL, Token: "destination-token-for-test", InstanceUID: targetEnv.DB.InstanceUID()}}
	})
	source, err := sourceEnv.DB.CreateProject(t.Context(), "intake-project")
	require.NoError(t, err)
	issue, _, err := sourceEnv.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.ID, Title: "Kickoff", Author: "setup-author"})
	require.NoError(t, err)
	child, _, err := sourceEnv.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.ID, Title: "Completed child", Author: "setup-author"})
	require.NoError(t, err)
	_, err = sourceEnv.DB.CreateLink(t.Context(), db.CreateLinkParams{FromIssueID: child.ID, ToIssueID: issue.ID, Type: "parent", Author: "setup-author"})
	require.NoError(t, err)
	_, _, _, err = sourceEnv.DB.CloseIssue(t.Context(), child.ID, "done", "setup-author", "", nil)
	require.NoError(t, err)
	t.Setenv("KATA_AUTH_TOKEN", "destination-token-for-test")
	dir := setupTestWorkspace(t)
	t.Setenv("EXAMPLE_SOURCE_TOKEN", "source-token-for-test")
	t.Setenv("EXAMPLE_DESTINATION_TOKEN", "destination-token-for-test")
	catalog := fmt.Sprintf("[[daemon]]\nname=\"source-service\"\nurl=%q\ntoken_env=\"EXAMPLE_SOURCE_TOKEN\"\n\n[[daemon]]\nname=\"destination-service\"\nurl=%q\ntoken_env=\"EXAMPLE_DESTINATION_TOKEN\"\n", sourceEnv.URL, targetEnv.URL)
	require.NoError(t, os.WriteFile(filepath.Join(sourceEnv.Home, "config.toml"), []byte(catalog), 0600))
	target, err := client.PrepareResolvedNamed(t.Context(), "destination-service")
	require.NoError(t, err)
	ctx := context.WithValue(t.Context(), resolvedDaemonContextKey{}, target)
	_, err = callInit(ctx, target.BaseURL, dir, callInitOpts{Project: "example-project", Kickoff: source.Name + "#" + issue.ShortID, SourceDaemon: "source-service", Actor: "setup-author"})
	require.NoError(t, err)
	progress := readSetupTestProgress(t, dir)
	captured := progress["source"].(map[string]any)
	require.Equal(t, issue.UID, captured["issue_uid"])
	require.Equal(t, sourceEnv.DB.InstanceUID(), captured["authority_uid"])
	require.Equal(t, "source-service", captured["route"].(map[string]any)["daemon"])
	local, err := config.ReadLocalConfig(dir)
	require.NoError(t, err)
	require.Equal(t, targetEnv.URL, local.Server.URL)
	require.Empty(t, local.Server.Daemon)
	selected, err := client.PrepareResolvedInWorkspace(t.Context(), dir)
	require.NoError(t, err)
	require.Equal(t, targetEnv.URL, selected.BaseURL)
	bs, err := os.ReadFile(filepath.Join(dir, setupProgressTestName))
	require.NoError(t, err)
	require.NotContains(t, string(bs), "source-token-for-test")
	require.NotContains(t, string(bs), "destination-token-for-test")
	project, err := targetEnv.DB.ProjectByName(t.Context(), "example-project")
	require.NoError(t, err)
	for _, member := range []db.Issue{issue, child} {
		moved, err := targetEnv.DB.IssueByUID(t.Context(), member.UID, db.IncludeDeletedYes)
		require.NoError(t, err)
		require.Equal(t, project.ID, moved.ProjectID)
	}
	before, err := targetEnv.DB.MaxEventID(t.Context())
	require.NoError(t, err)
	_, err = callInit(ctx, target.BaseURL, dir, callInitOpts{Actor: "setup-author"})
	require.NoError(t, err)
	after, err := targetEnv.DB.MaxEventID(t.Context())
	require.NoError(t, err)
	require.Equal(t, before, after, "retry must use the same committed request")
}

func TestInitSetupRetryRejectsChangedWorkspaceAlias(t *testing.T) {
	resetFlags(t)
	flags.Mode = outputJSON
	env := testenv.New(t)
	dir := setupTestWorkspace(t)
	runGit(t, dir, "remote", "add", "origin", "https://example.com/example/project.git")
	proxy, faults := newSetupFaultProxy(t, env.URL)
	faults.dropAck = true
	_, err := callInit(t.Context(), proxy.URL, dir, callInitOpts{Setup: true, Project: "example-project", Actor: "setup-author"})
	require.Error(t, err)
	before, err := os.ReadFile(filepath.Join(dir, setupProgressTestName))
	require.NoError(t, err)
	runGit(t, dir, "remote", "set-url", "origin", "https://example.com/example/another.git")
	faults.mu.Lock()
	faults.dropAck = false
	faults.mu.Unlock()
	_, err = callInit(t.Context(), proxy.URL, dir, callInitOpts{Actor: "setup-author"})
	var ce *cliError
	require.ErrorAs(t, err, &ce)
	require.Equal(t, "setup_input_conflict", ce.Code)
	after, err := os.ReadFile(filepath.Join(dir, setupProgressTestName))
	require.NoError(t, err)
	require.Equal(t, before, after)
	projects, err := env.DB.ListProjects(t.Context())
	require.NoError(t, err)
	require.Len(t, projects, 1)
	aliases, err := env.DB.ProjectAliases(t.Context(), projects[0].ID)
	require.NoError(t, err)
	require.Len(t, aliases, 1)
}

func TestInitSetupRetryAcceptsExplicitAliasConsent(t *testing.T) {
	resetFlags(t)
	flags.Mode = outputJSON
	env := testenv.New(t)
	dir := setupTestWorkspace(t)
	_, err := callInit(t.Context(), env.URL, dir, callInitOpts{Project: "intake-project", Actor: "setup-author"})
	require.NoError(t, err)
	_, err = callInit(t.Context(), env.URL, dir, callInitOpts{Setup: true, Project: "example-project", Replace: true, Actor: "setup-author"})
	require.Error(t, err)
	before := readSetupTestProgress(t, dir)
	_, err = callInit(t.Context(), env.URL, dir, callInitOpts{Reassign: true, Actor: "setup-author"})
	require.NoError(t, err)
	after := readSetupTestProgress(t, dir)
	require.Equal(t, before["request_uid"], after["request_uid"])
	require.Equal(t, before["destination"], after["destination"])
	project, err := env.DB.ProjectByName(t.Context(), "example-project")
	require.NoError(t, err)
	alias, err := env.DB.AliasByIdentity(t.Context(), "local://"+dir)
	require.NoError(t, err)
	require.Equal(t, project.ID, alias.ProjectID)
}
