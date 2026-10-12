package daemon_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json/v2"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/uid"
)

// These stores communicate through real TCP listeners. PostgreSQL cases use
// the runner's explicitly disposable DSN, never a shared database target.
func TestTypedIssueTreeHTTPBackends(t *testing.T) {
	for _, pair := range []struct {
		name                    string
		sourcePG, destinationPG bool
	}{
		{"sqlite-sqlite", false, false}, {"postgres-postgres", true, true},
		{"sqlite-postgres", false, true}, {"postgres-sqlite", true, false},
	} {
		t.Run(pair.name, func(t *testing.T) {
			if (pair.sourcePG || pair.destinationPG) && os.Getenv("KATA_TEST_POSTGRES_DSN") == "" {
				t.Skip("disposable PostgreSQL DSN required")
			}
			open := func(pg bool) db.Storage {
				var store db.Storage
				var err error
				if pg {
					dsn, cleanup := testenv.NewPostgresContainer(t, t.Context())
					t.Cleanup(cleanup)
					store, err = pgstore.Open(t.Context(), dsn)
				} else {
					store, err = sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "kata.db"))
				}
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, store.Close()) })
				return store
			}
			source, destination := open(pair.sourcePG), open(pair.destinationPG)
			destinationURL := serveIssueMoveStore(t, daemon.ServerConfig{DB: destination, Auth: config.AuthConfig{Token: "destination-token"}})
			sourceURL := serveIssueMoveStore(t, daemon.ServerConfig{DB: source, Auth: config.AuthConfig{Token: "source-token"}, FederationCatalog: []config.CatalogDaemonConfig{{Name: "destination", URL: destinationURL, Token: "destination-token", InstanceUID: destination.InstanceUID()}}})
			from, err := source.CreateProject(t.Context(), "source-project")
			require.NoError(t, err)
			to, err := destination.CreateProject(t.Context(), "destination-project")
			require.NoError(t, err)
			root, _, err := source.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: from.ID, Title: "Root", Author: "original-author"})
			require.NoError(t, err)
			closed, _, err := source.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: from.ID, Title: "Closed child", Author: "original-author"})
			require.NoError(t, err)
			deleted, _, err := source.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: from.ID, Title: "Deleted child", Author: "original-author"})
			require.NoError(t, err)
			for _, child := range []db.Issue{closed, deleted} {
				_, err = source.CreateLink(t.Context(), db.CreateLinkParams{FromIssueID: child.ID, ToIssueID: root.ID, Type: "parent", Author: "original-author"})
				require.NoError(t, err)
			}
			_, _, _, err = source.CloseIssue(t.Context(), closed.ID, "done", "original-author", "", nil)
			require.NoError(t, err)
			_, _, _, err = source.SoftDeleteIssue(t.Context(), deleted.ID, "original-author")
			require.NoError(t, err)
			_, _, err = source.CreateComment(t.Context(), db.CreateCommentParams{IssueID: root.ID, Body: "Original comment", Author: "comment-author"})
			require.NoError(t, err)
			requestUID, err := uid.New()
			require.NoError(t, err)
			r := db.MoveTreeRequest{SourceAuthorityUID: source.InstanceUID(), SourceProjectUID: from.UID, IssueUID: root.UID, DestinationAuthorityUID: destination.InstanceUID(), DestinationProjectUID: to.UID, RequestUID: requestUID, ActorAuthorityUID: source.InstanceUID(), Actor: "operator"}
			send := func() db.MoveTreeResult {
				raw, err := json.Marshal(map[string]any{"request": r})
				require.NoError(t, err)
				request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, sourceURL+"/api/v1/issue-moves", bytes.NewReader(raw))
				require.NoError(t, err)
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("Authorization", "Bearer source-token")
				response, err := (&http.Client{Timeout: 20 * time.Second}).Do(request)
				require.NoError(t, err)
				body := readClose(t, response)
				require.Equal(t, 200, response.StatusCode, string(body))
				var out struct {
					Result db.MoveTreeResult `json:"result"`
				}
				require.NoError(t, json.Unmarshal(body, &out))
				return out.Result
			}
			result := send()
			require.Equal(t, "completed", result.State)
			require.Len(t, result.Applied, 3)
			require.Len(t, result.Finalized, 3)
			require.NotEmpty(t, result.SnapshotDigest)
			for _, original := range []db.Issue{root, closed, deleted} {
				received, err := destination.IssueByUID(t.Context(), original.UID, db.IncludeDeletedYes)
				require.NoError(t, err)
				require.Equal(t, to.ID, received.ProjectID)
				require.Equal(t, original.Author, received.Author)
				q := source.(interface {
					QueryRowContext(context.Context, string, ...any) *sql.Row
				})
				var authority, project, eventUID string
				require.NoError(t, q.QueryRowContext(t.Context(), `SELECT moved_to_authority_uid,moved_to_project_uid,moved_event_uid FROM issues WHERE uid=$1`, original.UID).Scan(&authority, &project, &eventUID))
				require.Equal(t, destination.InstanceUID(), authority)
				require.Equal(t, to.UID, project)
				require.NotEmpty(t, eventUID)
			}
			retry := send()
			require.Equal(t, "completed", retry.State)
			require.Equal(t, result.Applied, retry.Applied)
			require.ElementsMatch(t, result.Finalized, retry.Finalized)
		})
	}
}

func serveIssueMoveStore(t *testing.T, cfg daemon.ServerConfig) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	cfg.Broadcaster = daemon.NewEventBroadcaster()
	server := daemon.NewServer(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, listener) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("move test daemon did not stop")
		}
	})
	url := "http://" + listener.Addr().String()
	require.Eventually(t, func() bool {
		response, err := http.Get(url + "/api/v1/ping")
		if err != nil {
			return false
		}
		defer response.Body.Close()
		return response.StatusCode == 200
	}, 3*time.Second, 10*time.Millisecond)
	return url
}
