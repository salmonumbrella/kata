package daemon_test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestUIIssueReferenceRejectsMovedArchivedProject(t *testing.T) {
	env := testenv.New(t)
	source, target, issue := seedMovePair(t, env)
	_, err := env.DB.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "worker"})
	require.NoError(t, err)
	_, _, err = env.DB.RemoveProject(t.Context(), db.RemoveProjectParams{ProjectID: target.ID, Actor: "worker", Force: true})
	require.NoError(t, err)
	response, raw := envDoRaw(t, env, http.MethodGet, fmt.Sprintf("/api/v1/ui/issue-reference?project_id=%d&ref=%s", source.ID, issue.ShortID), nil, nil)
	assertAPIError(t, response.StatusCode, raw, http.StatusNotFound, "project_not_found")
}

func TestMoveIssueChecksEveryClaimAndKeepsPreviewReadOnly(t *testing.T) {
	for _, claimed := range []string{"root", "descendant"} {
		t.Run(claimed, func(t *testing.T) {
			env := testenv.New(t)
			source, root := createClaimHubIssueNamed(t, env, "source-project")
			target, _ := createClaimHubIssueNamed(t, env, "target-project")
			childProject, _ := createClaimHubIssueNamed(t, env, "child-project")
			child, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: childProject.ID, Title: "child", Author: "worker", Links: []db.InitialLink{{Type: "parent", ToNumber: root.ID}}})
			require.NoError(t, err)
			claimProject, issue := source, root
			if claimed == "descendant" {
				claimProject, issue = childProject, child
			}
			acquireClaimGateIssue(t, env, claimProject, issue, "other")
			before, err := env.DB.MaxEventID(t.Context())
			require.NoError(t, err)
			path := issuePathRef(source.ID, root.ShortID, "actions/move")
			response, raw := envDoRaw(t, env, http.MethodPost, path, map[string]any{"actor": "worker", "to_project_uid": target.UID, "dry_run": true}, nil)
			assertAPIError(t, response.StatusCode, raw, http.StatusConflict, "claim_denied")
			after, err := env.DB.MaxEventID(t.Context())
			require.NoError(t, err)
			require.Equal(t, before, after, "preview must not mutate leases or events")
			response, raw = envDoRaw(t, env, http.MethodPost, path, map[string]any{"actor": "worker", "to_project_uid": target.UID}, map[string]string{"If-Match": fmt.Sprintf(`"rev-%d"`, root.Revision)})
			assertAPIError(t, response.StatusCode, raw, http.StatusConflict, "claim_denied")
			for _, original := range []db.Issue{root, child} {
				stored, err := env.DB.IssueByID(t.Context(), original.ID)
				require.NoError(t, err)
				require.Equal(t, original.ProjectID, stored.ProjectID)
			}
		})
	}
}

func TestMoveIssuePreviewDoesNotExpireClaims(t *testing.T) {
	env := testenv.New(t)
	source, issue := createClaimHubIssueNamed(t, env, "source-project")
	target, _ := createClaimHubIssueNamed(t, env, "target-project")
	_, err := env.DB.AcquireClaim(t.Context(), db.AcquireClaimParams{ProjectID: source.ID, IssueRef: issue.UID, Principal: claimGatePrincipal(env, "worker"), ClaimKind: "timed", TTL: time.Minute, Now: time.Now().Add(-2 * time.Minute)})
	require.NoError(t, err)
	before, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)
	response, raw := envDoRaw(t, env, http.MethodPost, issuePathRef(source.ID, issue.ShortID, "actions/move"), map[string]any{"actor": "worker", "to_project_uid": target.UID, "dry_run": true}, nil)
	require.Equal(t, http.StatusOK, response.StatusCode, string(raw))
	after, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)
	require.Equal(t, before, after)
}

func TestUIIssueReferenceChecksMovedProjectAccess(t *testing.T) {
	dbh := openTestDB(t)
	store := dbh.db
	source, err := store.CreateProject(t.Context(), "source-project")
	require.NoError(t, err)
	target, err := store.CreateProject(t.Context(), "target-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.ID, Title: "moved issue", Author: "worker"})
	require.NoError(t, err)
	_, err = store.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "worker"})
	require.NoError(t, err)
	access := &scopedUIReferencesHostAccess{deniedProjectID: target.ID}
	server := daemon.NewServer(daemon.ServerConfig{DB: store, StartedAt: dbh.now, HostAccess: access})
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := daemon.WithPrincipal(r.Context(), daemon.Principal{Kind: daemon.PrincipalHost, Subject: "example-user", Actor: "worker"})
		server.Handler().ServeHTTP(w, r.WithContext(ctx))
	}))
	t.Cleanup(local.Close)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("%s/api/v1/ui/issue-reference?project_id=%d&ref=%s", local.URL, source.ID, issue.ShortID), nil)
	require.NoError(t, err)
	response, err := local.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound, response.StatusCode, string(raw))
	require.NotEmpty(t, access.requests)
	require.ElementsMatch(t, []int64{source.ID, target.ID}, access.requests[len(access.requests)-1].Operation.ProjectIDs)
}
