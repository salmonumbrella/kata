package daemon_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// A path-free init must honor the saved project UID, so losing the response
// cannot create a different project on a second setup attempt.
func TestInitProjectStableUID(t *testing.T) {
	database := openTestDB(t)
	sink := &recordingSink{}
	server := startTestServer(t, daemon.ServerConfig{DB: database.db, StartedAt: database.now, Hooks: sink})
	projectUID, err := uid.New()
	require.NoError(t, err)
	request := map[string]any{"name": "example-project", "project_uid": projectUID, "authority_uid": database.db.InstanceUID(), "actor": "setup-author", "alias": map[string]any{"kind": "local", "identity": "local:///client-only/example-workspace"}}
	response, body := postJSON(t, server, "/api/v1/projects", request)
	require.Equal(t, http.StatusOK, response.StatusCode, string(body))
	var first struct {
		Project db.Project      `json:"project"`
		Alias   db.ProjectAlias `json:"alias"`
		Created bool            `json:"created"`
	}
	require.NoError(t, json.Unmarshal(body, &first))
	require.Equal(t, projectUID, first.Project.UID)
	require.True(t, first.Created)
	response, body = postJSON(t, server, "/api/v1/projects", request)
	require.Equal(t, http.StatusOK, response.StatusCode, string(body))
	var retry struct {
		Project db.Project      `json:"project"`
		Alias   db.ProjectAlias `json:"alias"`
		Created bool            `json:"created"`
	}
	require.NoError(t, json.Unmarshal(body, &retry))
	require.Equal(t, first.Project, retry.Project)
	require.Equal(t, first.Alias, retry.Alias)
	require.False(t, retry.Created)
	captured := sink.snapshot()
	require.Len(t, captured, 1)
	require.Equal(t, "setup-author", captured[0].Actor)
	events, err := database.db.EventsByUIDs(t.Context(), first.Project.ID, []string{captured[0].UID})
	require.NoError(t, err)
	require.Equal(t, []db.Event{captured[0]}, events)
	request["name"] = "conflicting-project"
	otherUID, err := uid.New()
	require.NoError(t, err)
	request["project_uid"] = otherUID
	request["reassign"] = true
	response, body = postJSON(t, server, "/api/v1/projects", request)
	require.Equal(t, http.StatusBadRequest, response.StatusCode, string(body))
	request["reassign"] = false
	response, body = postJSON(t, server, "/api/v1/projects", request)
	require.Equal(t, http.StatusConflict, response.StatusCode, string(body))
	_, err = database.db.ProjectByUID(t.Context(), otherUID)
	require.ErrorIs(t, err, db.ErrNotFound)
}

func TestInitProjectStableUIDRejectsWrongAuthority(t *testing.T) {
	database := openTestDB(t)
	server := startTestServer(t, daemon.ServerConfig{DB: database.db, StartedAt: database.now})
	projectUID, err := uid.New()
	require.NoError(t, err)
	otherAuthority, err := uid.New()
	require.NoError(t, err)
	response, body := postJSON(t, server, "/api/v1/projects", map[string]any{"name": "example-project", "project_uid": projectUID, "authority_uid": otherAuthority, "actor": "setup-author"})
	require.Equal(t, http.StatusConflict, response.StatusCode, string(body))
	_, err = database.db.ProjectByUID(t.Context(), projectUID)
	require.ErrorIs(t, err, db.ErrNotFound)
}

func TestInitProjectStableUIDRetainedName(t *testing.T) {
	database := openTestDB(t)
	server := startTestServer(t, daemon.ServerConfig{DB: database.db, StartedAt: database.now})
	reservedUID, err := uid.New()
	require.NoError(t, err)
	_, err = database.db.ExecContext(t.Context(), `INSERT INTO project_aliases(project_id,alias_identity,alias_kind,authority_uid,project_uid) VALUES(NULL,$1,'name',$2,$3)`, "retained-project", database.db.InstanceUID(), reservedUID)
	require.NoError(t, err)
	requestedUID, err := uid.New()
	require.NoError(t, err)
	response, body := postJSON(t, server, "/api/v1/projects", map[string]any{"name": "retained-project", "project_uid": requestedUID, "authority_uid": database.db.InstanceUID(), "actor": "setup-author"})
	require.Equal(t, http.StatusConflict, response.StatusCode, string(body))
	require.Contains(t, string(body), "project_name_retained")
	_, err = database.db.ProjectByUID(t.Context(), requestedUID)
	require.ErrorIs(t, err, db.ErrNotFound)
}

// A failed response-only config write cannot hide a committed project event.
func TestInitProjectStableUIDConfigFailure(t *testing.T) {
	sink := &recordingSink{}
	broadcaster := daemon.NewEventBroadcaster()
	subscription := broadcaster.Subscribe(daemon.SubFilter{})
	defer subscription.Unsub()
	h := newServerWithGitWorkspace(t, "", withHooksSink(sink), withBroadcaster(broadcaster))
	require.NoError(t, os.Mkdir(filepath.Join(h.dir, ".kata.toml"), 0700))
	projectUID, err := uid.New()
	require.NoError(t, err)
	request := map[string]any{"name": "example-project", "start_path": h.dir, "project_uid": projectUID, "authority_uid": h.DB().InstanceUID(), "actor": "setup-author"}
	response, body := postJSON(t, h.ts.(*httptest.Server), "/api/v1/projects", request)
	require.Equal(t, http.StatusInternalServerError, response.StatusCode, string(body))
	msg := receiveMsg(t, subscription.Ch, time.Second, "atomic setup before config failure")
	require.Equal(t, daemon.StreamKindEvent, msg.Kind)
	require.NotNil(t, msg.Event)
	require.Equal(t, projectUID, msg.Event.ProjectUID)
	require.Equal(t, "setup-author", msg.Event.Actor)
	captured := sink.snapshot()
	require.Equal(t, []db.Event{*msg.Event}, captured)
	project, err := h.DB().ProjectByUID(t.Context(), projectUID)
	require.NoError(t, err)
	aliases, err := h.DB().ProjectAliases(t.Context(), project.ID)
	require.NoError(t, err)
	require.Len(t, aliases, 1)
	events, err := h.DB().EventsByUIDs(t.Context(), project.ID, []string{msg.Event.UID})
	require.NoError(t, err)
	require.Equal(t, []db.Event{*msg.Event}, events)
	require.NoError(t, os.Remove(filepath.Join(h.dir, ".kata.toml")))
	response, body = postJSON(t, h.ts.(*httptest.Server), "/api/v1/projects", request)
	require.Equal(t, http.StatusOK, response.StatusCode, string(body))
	require.Len(t, sink.snapshot(), 1)
}

func TestProjectAccessStableUIDSetup(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		status, _, body := f.request(t, http.MethodPost, "/api/v1/projects", "nonmember", map[string]any{"name": "new-project", "project_uid": f.private.UID, "authority_uid": store.InstanceUID(), "actor": "member", "alias": map[string]string{"identity": "local:///private-workspace", "kind": "local"}}, nil)
		require.Equal(t, http.StatusNotFound, status, string(body))
		require.NotContains(t, string(body), f.private.UID)
		_, err := store.AliasByIdentity(t.Context(), "local:///private-workspace")
		require.ErrorIs(t, err, db.ErrNotFound)
	})
}
