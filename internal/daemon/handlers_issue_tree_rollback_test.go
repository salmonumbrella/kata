package daemon_test

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/uid"
)

// A real second-event insert failure rolls back the native transaction. Its
// provisional first event and source routing must never be published as facts.
func TestTypedIssueTreeRolledBackEvents(t *testing.T) {
	for _, phase := range []string{"accept", "finalize", "coordinator", "native-accept", "native-finalize"} {
		t.Run(phase, func(t *testing.T) {
			native := strings.HasPrefix(phase, "native-")
			operation := strings.TrimPrefix(phase, "native-")
			destinationSink, sourceSink := &recordingSink{}, &recordingSink{}
			destination := testenv.New(t, testenv.WithAuthToken("destination-token"), func(cfg *daemon.ServerConfig) { cfg.Hooks = destinationSink })
			source := testenv.New(t, testenv.WithAuthToken("source-token"), func(cfg *daemon.ServerConfig) {
				cfg.Hooks = sourceSink
				cfg.FederationCatalog = []config.CatalogDaemonConfig{{Name: "destination", URL: destination.URL, Token: "destination-token", InstanceUID: destination.DB.InstanceUID()}}
			})
			from, err := source.DB.CreateProject(t.Context(), "source-project")
			require.NoError(t, err)
			to, err := destination.DB.CreateProject(t.Context(), "destination-project")
			require.NoError(t, err)
			root, _, err := source.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: from.ID, Title: "First member", Author: "operator"})
			require.NoError(t, err)
			child, _, err := source.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: from.ID, Title: "Second member", Author: "operator"})
			require.NoError(t, err)
			_, err = source.DB.CreateLink(t.Context(), db.CreateLinkParams{FromIssueID: child.ID, ToIssueID: root.ID, Type: "parent", Author: "operator"})
			require.NoError(t, err)
			requestUID, err := uid.New()
			require.NoError(t, err)
			r := db.MoveTreeRequest{SourceAuthorityUID: source.DB.InstanceUID(), SourceProjectUID: from.UID, IssueUID: root.UID, DestinationAuthorityUID: destination.DB.InstanceUID(), DestinationProjectUID: to.UID, RequestUID: requestUID, ActorAuthorityUID: source.DB.InstanceUID(), Actor: "operator"}
			snapshot, err := source.DB.ExportIssueTree(t.Context(), r)
			require.NoError(t, err)
			members := snapshot.Portions[0].Issues
			require.Len(t, members, 2)
			secondUID := members[1].UID
			target, token, path := destination, "destination-token", "/api/v1/issue-moves/accept"
			body := map[string]any{"request": r, "snapshot": snapshot}
			eventType := "issue.move_received"
			if operation != "accept" {
				target, token, eventType = source, "source-token", "issue.moved"
				if operation == "finalize" {
					accepted, err := destination.DB.AcceptIssueTree(t.Context(), snapshot, r)
					require.NoError(t, err)
					path = "/api/v1/issue-moves/finalize"
					body = map[string]any{"request": r, "preview": db.MoveTreePreview{SnapshotDigest: snapshot.Digest}, "accepted": accepted}
				} else {
					path = "/api/v1/issue-moves"
					body = map[string]any{"request": r}
				}
			}
			// Test-only DDL on this disposable store; no product schema is changed.
			_, err = target.DB.ExecContext(t.Context(), fmt.Sprintf(`CREATE TRIGGER fail_second_move_event BEFORE INSERT ON events WHEN NEW.type='%s' AND NEW.issue_uid='%s' BEGIN SELECT RAISE(ABORT,'injected second move event failure'); END`, eventType, secondUID))
			require.NoError(t, err)
			if native {
				var result db.MoveTreeResult
				if operation == "accept" {
					result, err = destination.DB.AcceptIssueTree(t.Context(), snapshot, r)
				} else {
					accepted := body["accepted"].(db.MoveTreeResult)
					verified := db.WithMoveDestinationCommitVerifier(t.Context(), func(ctx context.Context, r db.MoveTreeRequest, proposed db.MoveTreeResult) error {
						actual, err := destination.DB.LookupIssueTreeAcceptance(ctx, r)
						if err != nil {
							return err
						}
						return db.ValidateCommittedMoveEvidence(proposed, actual)
					})
					result, err = source.DB.FinalizeIssueTree(verified, r, db.MoveTreePreview{SnapshotDigest: snapshot.Digest}, accepted)
				}
				require.Error(t, err)
				require.Empty(t, result.Events, "rolled-back events are not committed evidence")
				require.Empty(t, result.Finalized, "rolled-back source changes are not finalized")
				if operation == "accept" {
					require.Empty(t, result.Applied, "rolled-back receiver inserts are not committed evidence")
				} else {
					require.Len(t, result.Applied, 2, "retain independently verified destination evidence")
				}
				return
			}

			raw, err := json.Marshal(body)
			require.NoError(t, err)
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, target.URL+path, bytes.NewReader(raw))
			require.NoError(t, err)
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", "Bearer "+token)
			response, err := target.HTTP.Do(request)
			require.NoError(t, err)
			responseBody := readClose(t, response)
			if phase == "coordinator" {
				require.Equal(t, 200, response.StatusCode, string(responseBody))
				var out struct {
					Result db.MoveTreeResult `json:"result"`
				}
				require.NoError(t, json.Unmarshal(responseBody, &out))
				require.Empty(t, out.Result.Finalized, "rolled-back routing has no finalized evidence")
			} else {
				require.NotEqual(t, 200, response.StatusCode, string(responseBody))
			}
			if phase == "accept" {
				require.Empty(t, destinationSink.snapshot(), "rolled-back receipt was broadcast")
			} else {
				require.Empty(t, sourceSink.snapshot(), "rolled-back move was broadcast")
			}
			for _, member := range []db.Issue{root, child} {
				if phase == "accept" {
					_, err := destination.DB.IssueByUID(t.Context(), member.UID, db.IncludeDeletedYes)
					require.ErrorIs(t, err, db.ErrNotFound)
				} else {
					var moved *string
					require.NoError(t, source.DB.QueryRowContext(t.Context(), `SELECT moved_to_authority_uid FROM issues WHERE uid=$1`, member.UID).Scan(&moved))
					require.Nil(t, moved)
				}
			}
		})
	}
}
