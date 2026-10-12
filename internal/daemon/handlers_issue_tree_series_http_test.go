package daemon_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/uid"
)

// Neither parent links nor retained source bodies locate a moved occurrence.
// Starting at its current authority must also locate the actual series owner.
func TestTypedIssueTreeDistributedSeries(t *testing.T) {
	for _, pair := range []struct {
		name                    string
		sourcePG, destinationPG bool
	}{{"sqlite-sqlite", false, false}, {"postgres-postgres", true, true}, {"sqlite-postgres", false, true}, {"postgres-sqlite", true, false}} {
		t.Run(pair.name, func(t *testing.T) {
			if (pair.sourcePG || pair.destinationPG) && os.Getenv("KATA_TEST_POSTGRES_DSN") == "" {
				t.Skip("disposable PostgreSQL DSN required")
			}
			runDistributedSeriesHTTP(t, pair.sourcePG, pair.destinationPG)
		})
	}
}
func runDistributedSeriesHTTP(t *testing.T, sourcePG, destinationPG bool) {
	for _, start := range []string{"retained-series-owner", "current-occurrence-authority", "series-only-owner-finalization-rollback"} {
		t.Run(start, func(t *testing.T) {
			open := func(postgres bool) db.Storage {
				var store db.Storage
				var err error
				if postgres {
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
			original, current, destination := open(sourcePG), open(sourcePG), open(destinationPG)
			destinationQuery := destination.(interface {
				QueryRowContext(context.Context, string, ...any) *sql.Row
			})
			from, err := original.CreateProject(t.Context(), "source-project")
			require.NoError(t, err)
			currentProject, err := current.CreateProject(t.Context(), "current-project")
			require.NoError(t, err)
			to, err := destination.CreateProject(t.Context(), "destination-project")
			require.NoError(t, err)
			first, _, err := original.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: from.ID, Title: "First occurrence", Author: "original-author"})
			require.NoError(t, err)
			series, err := original.CreateRecurrenceForIssue(t.Context(), db.CreateRecurrenceForIssueIn{IssueID: first.ID, Recurrence: db.CreateRecurrenceIn{ProjectID: from.ID, Actor: "original-author", Rule: "FREQ=DAILY", DTStart: "2026-10-12", Timezone: "UTC", Template: db.RecurrenceTemplate{Title: "Daily task"}}})
			require.NoError(t, err)
			next, err := original.MaterializeNext(t.Context(), series.Recurrence.ID, "2026-10-13", "scheduler")
			require.NoError(t, err)
			other, err := original.IssueByUID(t.Context(), next.NewIssueUID, db.IncludeDeletedYes)
			require.NoError(t, err)
			_, _, _, err = original.CloseIssue(t.Context(), other.ID, "done", "operator", "", nil)
			require.NoError(t, err)
			next, err = original.MaterializeNext(t.Context(), series.Recurrence.ID, "2026-10-14", "scheduler")
			require.NoError(t, err)
			purged, err := original.IssueByUID(t.Context(), next.NewIssueUID, db.IncludeDeletedYes)
			require.NoError(t, err)
			_, _, _, err = original.SoftDeleteIssue(t.Context(), purged.ID, "operator")
			require.NoError(t, err)
			tombstone, err := original.PurgeIssue(t.Context(), purged.ID, "operator", nil)
			require.NoError(t, err)
			requestUID, err := uid.New()
			require.NoError(t, err)
			single := db.MoveTreeRequest{SourceAuthorityUID: original.InstanceUID(), SourceProjectUID: from.UID, IssueUID: first.UID, DestinationAuthorityUID: current.InstanceUID(), DestinationProjectUID: currentProject.UID, Selection: "issue-only", RequestUID: requestUID, ActorAuthorityUID: original.InstanceUID(), Actor: "operator"}
			snapshot, err := original.ExportIssueTree(t.Context(), single)
			require.NoError(t, err)
			accepted, err := current.AcceptIssueTree(t.Context(), snapshot, single)
			require.NoError(t, err)
			verified := db.WithMoveDestinationCommitVerifier(t.Context(), func(ctx context.Context, r db.MoveTreeRequest, proposed db.MoveTreeResult) error {
				actual, err := current.LookupIssueTreeAcceptance(ctx, r)
				if err != nil {
					return err
				}
				return db.ValidateCommittedMoveEvidence(proposed, actual)
			})
			_, err = original.FinalizeIssueTree(verified, single, db.MoveTreePreview{SnapshotDigest: snapshot.Digest}, accepted)
			require.NoError(t, err)
			if start == "series-only-owner-finalization-rollback" {
				second := single
				second.IssueUID = other.UID
				second.RequestUID, err = uid.New()
				require.NoError(t, err)
				secondSnapshot, err := original.ExportIssueTree(t.Context(), second)
				require.NoError(t, err)
				secondAccepted, err := current.AcceptIssueTree(t.Context(), secondSnapshot, second)
				require.NoError(t, err)
				_, err = original.FinalizeIssueTree(verified, second, db.MoveTreePreview{SnapshotDigest: secondSnapshot.Digest}, secondAccepted)
				require.NoError(t, err)
			}
			localProject, err := current.CreateProject(t.Context(), "local-destination-project")
			require.NoError(t, err)
			localMove := single
			localMove.SourceAuthorityUID = current.InstanceUID()
			localMove.SourceProjectUID = currentProject.UID
			localMove.DestinationAuthorityUID = current.InstanceUID()
			localMove.DestinationProjectUID = localProject.UID
			localMove.ActorAuthorityUID = current.InstanceUID()
			localMove.Selection = ""
			localMove.MoveSeries = true
			localMove.RequestUID, err = uid.New()
			require.NoError(t, err)
			blocked, err := current.MoveIssueTree(t.Context(), localMove)
			var remote *db.MoveRemoteSelectionError
			require.ErrorAs(t, err, &remote, "local facade must not move only the current occurrence")
			require.Len(t, remote.RequiredSeries, 1)
			require.Equal(t, series.Recurrence.UID, remote.RequiredSeries[0].RecurrenceUID)
			require.Equal(t, original.InstanceUID(), remote.RequiredSeries[0].AuthorityUID)
			require.Empty(t, blocked.Applied)
			destinationURL := serveIssueMoveStore(t, daemon.ServerConfig{DB: destination, Auth: config.AuthConfig{Token: "destination-token"}})
			destinationRoute := config.CatalogDaemonConfig{Name: "destination", URL: destinationURL, Token: "destination-token", InstanceUID: destination.InstanceUID()}
			originalURL := serveIssueMoveStore(t, daemon.ServerConfig{DB: original, Auth: config.AuthConfig{Token: "original-token"}, FederationCatalog: []config.CatalogDaemonConfig{destinationRoute}})
			var failSeriesFinalize atomic.Bool
			if start == "series-only-owner-finalization-rollback" {
				failSeriesFinalize.Store(true)
				origin, err := url.Parse(originalURL)
				require.NoError(t, err)
				proxy := httputil.NewSingleHostReverseProxy(origin)
				director := proxy.Director
				proxy.Director = func(r *http.Request) { director(r); r.Host = origin.Host }
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/api/v1/issue-moves/finalize" && failSeriesFinalize.Load() {
						http.Error(w, "source finalization unavailable", http.StatusServiceUnavailable)
						return
					}
					proxy.ServeHTTP(w, r)
				}))
				t.Cleanup(server.Close)
				originalURL = server.URL
			}
			currentURL := serveIssueMoveStore(t, daemon.ServerConfig{DB: current, Auth: config.AuthConfig{Token: "current-token"}, FederationCatalog: []config.CatalogDaemonConfig{destinationRoute, {Name: "series-owner", URL: originalURL, Token: "original-token", InstanceUID: original.InstanceUID()}}})
			// The original authority needs the current occurrence route as well. Its
			// serving config is immutable, so use a new listener with the complete map.
			originalURL = serveIssueMoveStore(t, daemon.ServerConfig{DB: original, Auth: config.AuthConfig{Token: "original-token"}, FederationCatalog: []config.CatalogDaemonConfig{destinationRoute, {Name: "occurrence-authority", URL: currentURL, Token: "current-token", InstanceUID: current.InstanceUID()}}})
			r := single
			r.Selection = ""
			r.MoveSeries = true
			r.DestinationAuthorityUID = destination.InstanceUID()
			r.DestinationProjectUID = to.UID
			r.RequestUID, err = uid.New()
			require.NoError(t, err)
			sourceURL, token := originalURL, "original-token"
			r.IssueUID = other.UID
			if start != "retained-series-owner" {
				sourceURL, token = currentURL, "current-token"
				r.SourceAuthorityUID = current.InstanceUID()
				r.SourceProjectUID = currentProject.UID
				r.IssueUID = first.UID
				r.ActorAuthorityUID = current.InstanceUID()
			}
			raw, err := json.Marshal(map[string]any{"request": r})
			require.NoError(t, err)
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, sourceURL+"/api/v1/issue-moves", bytes.NewReader(raw))
			require.NoError(t, err)
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", "Bearer "+token)
			response, err := (&http.Client{}).Do(request)
			require.NoError(t, err)
			body := readClose(t, response)
			require.Equal(t, 200, response.StatusCode, string(body))
			var out struct {
				Result db.MoveTreeResult `json:"result"`
			}
			require.NoError(t, json.Unmarshal(body, &out))
			if start == "series-only-owner-finalization-rollback" {
				require.Equal(t, "outcome-unknown", out.Result.State, "all issues finalized cannot hide a writable scheduler-only source")
				require.Contains(t, out.Result.UnknownRoots, original.InstanceUID())
				failSeriesFinalize.Store(false)
			} else {
				require.Equal(t, "completed", out.Result.State)
			}
			require.Len(t, out.Result.Applied, 2)
			require.ElementsMatch(t, []string{first.UID, other.UID}, out.Result.Finalized)
			retryRequest, err := http.NewRequestWithContext(t.Context(), http.MethodPost, sourceURL+"/api/v1/issue-moves", bytes.NewReader(raw))
			require.NoError(t, err)
			retryRequest.Header.Set("Content-Type", "application/json")
			retryRequest.Header.Set("Authorization", "Bearer "+token)
			retryResponse, err := (&http.Client{}).Do(retryRequest)
			require.NoError(t, err)
			retryBody := readClose(t, retryResponse)
			require.Equal(t, 200, retryResponse.StatusCode, string(retryBody))
			var retried struct {
				Result db.MoveTreeResult `json:"result"`
			}
			require.NoError(t, json.Unmarshal(retryBody, &retried))
			require.Equal(t, "completed", retried.Result.State)
			require.Equal(t, out.Result.Applied, retried.Result.Applied)
			require.ElementsMatch(t, out.Result.Finalized, retried.Result.Finalized)
			if start == "series-only-owner-finalization-rollback" {
				originalQuery := original.(interface {
					QueryRowContext(context.Context, string, ...any) *sql.Row
				})
				var movedTo string
				require.NoError(t, originalQuery.QueryRowContext(t.Context(), `SELECT moved_to_authority_uid FROM recurrences WHERE uid=$1`, series.Recurrence.UID).Scan(&movedTo))
				require.Equal(t, destination.InstanceUID(), movedTo, "resume must finalize the scheduler-only source")
				again, err := http.NewRequestWithContext(t.Context(), http.MethodPost, sourceURL+"/api/v1/issue-moves", bytes.NewReader(raw))
				require.NoError(t, err)
				again.Header.Set("Content-Type", "application/json")
				again.Header.Set("Authorization", "Bearer "+token)
				againResponse, err := (&http.Client{}).Do(again)
				require.NoError(t, err)
				againBody := readClose(t, againResponse)
				require.Equal(t, 200, againResponse.StatusCode, string(againBody))
				require.NoError(t, json.Unmarshal(againBody, &retried))
				require.Equal(t, "completed", retried.Result.State, "a finalized scheduler must resume through its body-free receipt")
				require.Equal(t, out.Result.Applied, retried.Result.Applied)
			}
			var seriesID int64
			require.NoError(t, destinationQuery.QueryRowContext(t.Context(), `SELECT id FROM recurrences WHERE uid=$1`, series.Recurrence.UID).Scan(&seriesID))
			var retainedUID string
			require.NoError(t, destinationQuery.QueryRowContext(t.Context(), `SELECT uid FROM purge_log WHERE issue_uid=$1`, purged.UID).Scan(&retainedUID))
			require.Equal(t, tombstone.UID, retainedUID)
			_, err = destination.IssueByUID(t.Context(), purged.UID, db.IncludeDeletedYes)
			require.ErrorIs(t, err, db.ErrNotFound)
			future, err := destination.MaterializeNext(t.Context(), seriesID, "2026-10-15", "scheduler")
			require.NoError(t, err)
			require.NotEmpty(t, future.NewIssueUID)
		})
	}
}
