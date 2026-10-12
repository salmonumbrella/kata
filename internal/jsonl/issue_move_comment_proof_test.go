package jsonl_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/jsonl"
	"go.kenn.io/kata/internal/testenv"
)

type proofSQL interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func proofStores(t *testing.T, backend string) (db.Storage, db.Storage) {
	t.Helper()
	var source, target db.Storage
	var err error
	if backend == "sqlite" {
		source, err = sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "source.db"))
		require.NoError(t, err)
		target, err = sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "target.db"))
		require.NoError(t, err)
	} else {
		dsn, cleanup := testenv.NewPostgresContainer(t, t.Context())
		t.Cleanup(cleanup)
		source, err = pgstore.OpenWithConfig(t.Context(), dsn, pgstore.Config{Schema: "proof_source", SchemaMode: pgstore.SchemaModeBootstrap})
		require.NoError(t, err)
		target, err = pgstore.OpenWithConfig(t.Context(), dsn, pgstore.Config{Schema: "proof_target", SchemaMode: pgstore.SchemaModeBootstrap})
		require.NoError(t, err)
	}
	t.Cleanup(func() { require.NoError(t, source.Close()); require.NoError(t, target.Close()) })
	return source, target
}

// Approved contract: original signed comment/reply creation history follows
// the moved issue and its real purge identity, even without the original project.
func TestMovedCommentProofAfterPurge(t *testing.T) {
	movedCommentProofLifecycle(t, "purge")
}

func TestMovedCommentProofAfterSourceReset(t *testing.T) { movedCommentProofLifecycle(t, "reset") }

func movedCommentProofLifecycle(t *testing.T, lifecycle string) {
	t.Helper()
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			source, target := proofStores(t, backend)
			ctx := t.Context()
			original, err := source.CreateProject(ctx, "original-project")
			require.NoError(t, err)
			destination, err := source.CreateProject(ctx, "destination-project")
			require.NoError(t, err)
			issue, _, err := source.CreateIssue(ctx, db.CreateIssueParams{ProjectID: original.ID, Title: "Moved discussion", Author: "assistant"})
			require.NoError(t, err)
			comment, event, err := source.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "assistant", Teammate: "writer", Body: "Original message"})
			require.NoError(t, err)
			reply, replyEvent, err := source.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "assistant", Teammate: "writer", Body: "Original reply", ReplyToUID: comment.UID, ReplyKind: "reply", ValidateReply: true})
			require.NoError(t, err)
			public, private, err := ed25519.GenerateKey(nil)
			require.NoError(t, err)
			pin := db.RootKeyPin{ProjectUID: original.UID, AuthorityUID: "01AAAAAAAAAAAAAAAAAAAAAAAA", KeyID: db.RootPublicKeyID(public), PublicKey: public}
			require.NoError(t, source.PinRootAuthority(ctx, pin))
			_, err = source.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: original.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 42, HubProjectUID: original.UID, Actor: "member", Enabled: true, PushEnabled: true})
			require.NoError(t, err)
			receipts := make([]db.AttributionReceipt, 2)
			originals := make([][]byte, 2)
			for i, ev := range []db.Event{event, replyEvent} {
				remote := db.RemoteEvent{EventUID: ev.UID, OriginInstanceUID: ev.OriginInstanceUID, ProjectUID: ev.ProjectUID, ProjectName: ev.ProjectName, IssueUID: ev.IssueUID, RelatedIssueUID: ev.RelatedIssueUID, Type: ev.Type, Actor: ev.Actor, HLCPhysicalMS: ev.HLCPhysicalMS, HLCCounter: ev.HLCCounter, ContentHash: ev.ContentHash, Payload: jsontext.Value(ev.Payload), CreatedAt: ev.CreatedAt}
				_, _, err = db.ValidateRemoteEventContentHash(remote)
				require.NoError(t, err)
				originals[i], err = db.EncodeRelaySourceEvent(remote)
				require.NoError(t, err)
				receipts[i], err = db.SignRootReceipt(db.AttributionReceipt{Version: 1, ProjectUID: original.UID, AuthorityUID: pin.AuthorityUID, KeyID: pin.KeyID, EventUID: ev.UID, ContentHash: ev.ContentHash, AccountableActor: "member", SourceActor: ev.Actor, Teammate: "writer", IngressInstanceUID: source.InstanceUID(), AcceptedAt: time.Now().UTC(), ResetEpoch: 1, Sequence: int64(i + 1)}, private)
				require.NoError(t, err)
				require.NoError(t, source.ApplyUpstreamAttribution(ctx, pin, receipts[i]))
			}
			_, err = source.LeaveFederationReplica(ctx, original.ID)
			require.NoError(t, err)
			_, err = source.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: original.ID, ToProjectID: destination.ID, IfMatchRev: issue.Revision, Actor: "assistant"})
			require.NoError(t, err)

			visible, err := source.CommentsByIssue(ctx, issue.ID)
			require.NoError(t, err)
			require.Len(t, visible, 2)
			for _, entry := range visible {
				require.Equal(t, "verified", entry.Verification, "moved comment creation proof remains readable")
				require.Equal(t, "member", entry.AccountableActor)
				require.Equal(t, pin.AuthorityUID, entry.AuthorityUID)
			}

			// A grant for the old source project cannot reveal moved creation
			// receipts while that source project still exists locally.
			for _, scope := range []struct {
				name    string
				allowed []string
				count   int
			}{
				{"current-owner-live", []string{destination.UID}, 4}, {"old-source-live", []string{original.UID}, 0}, {"empty-live", []string{}, 0},
			} {
				t.Run(scope.name, func(t *testing.T) {
					count := 0
					for record, err := range source.ExportAttribution(db.WithAuthorizedProjects(ctx, scope.allowed), db.ExportFilter{IncludeDeleted: true}) {
						require.NoError(t, err)
						switch record := record.(type) {
						case *db.AttributionReceipt:
							if record.EventUID == event.UID || record.EventUID == replyEvent.UID {
								count++
							}
						case *db.EntityProvenance:
							if record.EntityUID == comment.UID || record.EntityUID == reply.UID {
								count++
							}
						}
					}
					require.Equal(t, scope.count, count)
				})
			}

			if lifecycle == "purge" {
				t.Run("signed-export-rejects-history-tampering", func(t *testing.T) {
					_, err := source.(proofSQL).ExecContext(ctx, `UPDATE events SET actor='other' WHERE uid=$1`, event.UID)
					require.NoError(t, err)
					t.Cleanup(func() {
						_, err := source.(proofSQL).ExecContext(ctx, `UPDATE events SET actor=$1 WHERE uid=$2`, event.Actor, event.UID)
						require.NoError(t, err)
					})
					var exportErr error
					for _, err := range source.ExportEvents(ctx, db.ExportFilter{IncludeDeleted: true}) {
						if err != nil {
							exportErr = err
							break
						}
					}
					require.ErrorIs(t, exportErr, db.ErrRemoteEventHashMismatch)
				})
			}

			if lifecycle == "compact" {
				tx, err := source.(interface {
					BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error)
				}).BeginTx(ctx, nil)
				require.NoError(t, err)
				defer func() { _ = tx.Rollback() }()
				// Existing compaction fixtures delete the ordinary project event
				// namespace. Preserve moved subjects in that same transaction.
				require.NoError(t, db.RetainRelocatedProjectHistory(ctx, tx, original.ID, backend == "postgres"))
				_, err = tx.ExecContext(ctx, `DELETE FROM events WHERE project_id=$1`, original.ID)
				require.NoError(t, err)
				require.NoError(t, tx.Commit())
			}
			if lifecycle == "adopt" {
				_, err = source.AdoptProjectIntoFederation(ctx, db.AdoptProjectIntoFederationParams{ProjectID: original.ID, HubURL: "https://hub.example", HubProjectID: 43, HubProjectUID: "01MMMMMMMMMMMMMMMMMMMMMMMM", Actor: "member", EmptyOnly: true})
				require.NoError(t, err)
				_, err = source.LeaveFederationReplica(ctx, original.ID)
				require.NoError(t, err)
			}
			if lifecycle == "reset" || lifecycle == "signed-reset" || lifecycle == "signed-reset-old" {
				_, err = source.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: original.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 42, HubProjectUID: original.UID, Actor: "member", Enabled: true, PushEnabled: true})
				require.NoError(t, err)
				if lifecycle == "reset" {
					require.NoError(t, source.ResetFederatedProject(ctx, original.ID, 0, 0))
				} else {
					bindingUID := "01JJJJJJJJJJJJJJJJJJJJJJJJ"
					_, err = source.SetRelayBindingConfig(ctx, original.ID, db.RelayBindingConfig{ProtocolVersion: db.RelayProtocolVersion, BindingUID: bindingUID, AuthorityUID: pin.AuthorityUID, UpstreamInstanceUID: pin.AuthorityUID, HubPath: []string{pin.AuthorityUID, source.InstanceUID()}, LocalActor: "member", ResetEpoch: 1})
					require.NoError(t, err)
					resetReceipts := receipts
					if lifecycle == "signed-reset-old" {
						resetReceipts = []db.AttributionReceipt{}
					}
					provenance, err := json.Marshal(db.RootResetProvenance{Keys: []db.RootKeyPin{pin}, Receipts: resetReceipts, Entities: []db.EntityProvenance{}})
					require.NoError(t, err)
					snapshot := db.RootResetSnapshot{Events: []byte(`[]`), Entities: []byte(`[]`), Provenance: provenance, Artifacts: []byte(`[]`)}
					var historyID int64
					require.NoError(t, source.(proofSQL).QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM events WHERE project_id=$1`, original.ID).Scan(&historyID))
					manifest, err := db.SignRootResetManifest(db.RootResetManifest{HistoryEventID: historyID, Version: 1, ProjectUID: original.UID, AuthorityUID: pin.AuthorityUID, KeyID: pin.KeyID, ResetEpoch: 2, SnapshotUID: "01KKKKKKKKKKKKKKKKKKKKKKKK"}, snapshot, private)
					require.NoError(t, err)
					translation := db.RelayResetTranslation{Authority: db.RelayHopAuthority{BindingUID: bindingUID, ProjectUID: original.UID, AuthorityUID: pin.AuthorityUID, SenderInstanceUID: pin.AuthorityUID, ReceiverInstanceUID: source.InstanceUID(), Epoch: 2}, SnapshotUID: manifest.SnapshotUID, SnapshotDigest: manifest.SnapshotDigest}
					require.NoError(t, source.(db.RelayResetStore).InstallRelayReset(ctx, bindingUID, manifest, snapshot, translation))
				}
				_, err = source.LeaveFederationReplica(ctx, original.ID)
				require.NoError(t, err)
			}
			_, _, err = source.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: original.ID, Actor: "assistant"})
			require.NoError(t, err)
			_, err = source.PurgeProject(ctx, db.PurgeProjectParams{ProjectID: original.ID, Actor: "assistant"})
			require.NoError(t, err)

			var reservedSource string
			require.NoError(t, source.(proofSQL).QueryRowContext(ctx, `SELECT project_uid FROM project_aliases WHERE alias_kind='name' AND authority_uid=$1 AND alias_identity=$2`, source.InstanceUID(), original.Name).Scan(&reservedSource))
			require.Equal(t, original.UID, reservedSource, "old references keep the actual original source namespace after lifecycle cleanup")
			if lifecycle == "owner-project-purge" {
				_, _, err = source.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: destination.ID, Actor: "assistant", Force: true})
				require.NoError(t, err)
				_, err = source.PurgeProject(ctx, db.PurgeProjectParams{ProjectID: destination.ID, Actor: "assistant"})
				require.NoError(t, err)
			} else {
				_, err = source.PurgeIssue(ctx, issue.ID, "assistant", nil)
			}
			require.NoError(t, err)
			for _, scope := range []struct {
				name    string
				allowed []string
				count   int
			}{
				{"owner", nil, 5}, {"current-owner", []string{destination.UID}, 5}, {"old-source", []string{original.UID}, 0}, {"empty", []string{}, 0},
			} {
				t.Run(scope.name, func(t *testing.T) {
					readCtx := ctx
					if scope.allowed != nil {
						readCtx = db.WithAuthorizedProjects(ctx, scope.allowed)
					}
					count := 0
					for _, err := range source.ExportAttribution(readCtx, db.ExportFilter{IncludeDeleted: true}) {
						require.NoError(t, err)
						count++
					}
					require.Equal(t, scope.count, count)
				})
			}

			if lifecycle == "purge" {
				for _, scope := range []struct {
					name    string
					allowed []string
					count   int
				}{
					{"current-history", []string{destination.UID}, 2}, {"old-source-history", []string{original.UID}, 0}, {"empty-history", []string{}, 0},
				} {
					t.Run(scope.name, func(t *testing.T) {
						count := 0
						for ev, err := range source.ExportEvents(db.WithAuthorizedProjects(ctx, scope.allowed), db.ExportFilter{IncludeDeleted: true}) {
							require.NoError(t, err)
							if ev.UID == event.UID || ev.UID == replyEvent.UID {
								count++
							}
						}
						require.Equal(t, scope.count, count)
					})
				}
				t.Run("current-project-history", func(t *testing.T) {
					count := 0
					for ev, err := range source.ExportEvents(ctx, db.ExportFilter{ProjectID: &destination.ID, IncludeDeleted: true}) {
						require.NoError(t, err)
						if ev.UID == event.UID || ev.UID == replyEvent.UID {
							count++
						}
					}
					require.Equal(t, 2, count)
				})
			}

			for _, scope := range []struct {
				name    string
				allowed []string
				visible bool
			}{
				{"current-proof-read", []string{destination.UID}, true}, {"old-source-proof-read", []string{original.UID}, false}, {"empty-proof-read", []string{}, false},
			} {
				t.Run(scope.name, func(t *testing.T) {
					actual, err := source.EntityAttribution(db.WithAuthorizedProjects(ctx, scope.allowed), original.UID, "comment", comment.UID)
					if scope.visible {
						require.NoError(t, err)
						require.Equal(t, receipts[0], actual)
					} else {
						require.ErrorIs(t, err, db.ErrNotFound)
					}
				})
			}
			var output bytes.Buffer
			require.NoError(t, jsonl.Export(ctx, source, &output, jsonl.ExportOptions{IncludeDeleted: true}))
			require.NoError(t, jsonl.Import(ctx, bytes.NewReader(output.Bytes()), target))
			for i, c := range []db.Comment{comment, reply} {
				actual, err := target.EntityAttribution(ctx, original.UID, "comment", c.UID)
				require.NoError(t, err)
				require.Equal(t, receipts[i], actual)
				require.NoError(t, db.VerifyRootReceipt(pin, actual))
				var bodyCount int
				require.NoError(t, target.(proofSQL).QueryRowContext(ctx, `SELECT count(*) FROM comments WHERE uid=$1`, c.UID).Scan(&bodyCount))
				require.Zero(t, bodyCount)
				var restored []byte
				require.NoError(t, target.(proofSQL).QueryRowContext(ctx, `SELECT source_event_bytes FROM events WHERE uid=$1`, receipts[i].EventUID).Scan(&restored))
				require.Equal(t, originals[i], restored)
			}
			if lifecycle == "purge" {
				rejectedCommentProofRestores(t, target, output.Bytes(), event.UID, comment.UID, issue.UID, original.UID, receipts[0])
			}

			require.NoError(t, target.(proofSQL).QueryRowContext(ctx, `SELECT project_uid FROM project_aliases WHERE alias_kind='name' AND authority_uid=$1 AND alias_identity=$2`, source.InstanceUID(), original.Name).Scan(&reservedSource))
			require.Equal(t, original.UID, reservedSource)
			_, err = target.ProjectByUID(ctx, original.UID)
			require.ErrorIs(t, err, db.ErrNotFound)
			_, err = target.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
			require.ErrorIs(t, err, db.ErrNotFound)
		})
	}
}
