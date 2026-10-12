package jsonl_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"database/sql"
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

// The approved backup/cutover contract retains routing instead of reviving
// a finalized source projection as an active issue.
func TestIssueMoveRoutingJSONLRoundTrip(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
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
				source, err = pgstore.OpenWithConfig(t.Context(), dsn, pgstore.Config{Schema: "move_source", SchemaMode: pgstore.SchemaModeBootstrap})
				require.NoError(t, err)
				target, err = pgstore.OpenWithConfig(t.Context(), dsn, pgstore.Config{Schema: "move_target", SchemaMode: pgstore.SchemaModeBootstrap})
				require.NoError(t, err)
			}
			t.Cleanup(func() { require.NoError(t, source.Close()); require.NoError(t, target.Close()) })
			roundTripIssueMoveRouting(t, source, target)
		})
	}
}

func roundTripIssueMoveRouting(t *testing.T, source, target db.Storage) {
	ctx := t.Context()
	type sqlStore interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
		QueryRowContext(context.Context, string, ...any) *sql.Row
	}
	project, err := source.CreateProject(ctx, "source-project")
	require.NoError(t, err)
	issue, event, err := source.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Retained audit", Author: "tester"})
	require.NoError(t, err)
	_, err = source.(sqlStore).ExecContext(ctx, `INSERT INTO project_aliases(project_id,alias_identity,alias_kind,authority_uid,project_uid) VALUES(NULL,'retained-project','name',$1,$2)`, source.InstanceUID(), project.UID)
	require.NoError(t, err)
	_, err = source.(sqlStore).ExecContext(ctx, `INSERT INTO links(from_issue_id,to_issue_id,from_issue_uid,to_issue_uid,type,author,from_authority_uid,from_project_uid,to_authority_uid,to_project_uid) VALUES($1,NULL,$2,$3,'blocks','tester',$4,$5,$6,$7)`, issue.ID, issue.UID, "01AAAAAAAAAAAAAAAAAAAAAAAA", source.InstanceUID(), project.UID, "01BBBBBBBBBBBBBBBBBBBBBBBB", "01CCCCCCCCCCCCCCCCCCCCCCCC")
	require.NoError(t, err)
	mapping, err := source.UpsertImportMapping(ctx, db.ImportMappingParams{ProjectID: project.ID, Source: "example-provider", ExternalID: "task-1", ObjectType: "issue", IssueID: &issue.ID})
	require.NoError(t, err)
	_, err = source.(sqlStore).ExecContext(ctx, `UPDATE import_mappings SET moved_issue_uid=$1,moved_to_authority_uid=$2,moved_to_project_uid=$3 WHERE id=$4`, issue.UID, source.InstanceUID(), project.UID, mapping.ID)
	require.NoError(t, err)
	originalBytes, err := db.EncodeRelaySourceEvent(db.RemoteEventFromStored(event))
	require.NoError(t, err)
	_, err = source.(sqlStore).ExecContext(ctx, `UPDATE events SET source_project_uid=$1,source_event_bytes=$2 WHERE uid=$3`, project.UID, originalBytes, event.UID)
	require.NoError(t, err)
	historyIssue, historyEvent, err := source.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Remote history", Author: "tester"})
	require.NoError(t, err)
	historyOriginalBytes, err := db.EncodeRelaySourceEvent(db.RemoteEventFromStored(historyEvent))
	require.NoError(t, err)
	_, err = source.(sqlStore).ExecContext(ctx, `UPDATE events SET project_id=NULL,issue_id=NULL,source_project_uid=$1,source_event_bytes=$2 WHERE uid=$3`, project.UID, historyOriginalBytes, historyEvent.UID)
	require.NoError(t, err)
	series, _, err := source.CreateRecurrence(ctx, db.CreateRecurrenceIn{ProjectID: project.ID, Actor: "tester", Rule: "FREQ=DAILY", DTStart: "2026-10-12", Timezone: "UTC", Template: db.RecurrenceTemplate{Title: "Daily task"}})
	require.NoError(t, err)
	_, err = source.(sqlStore).ExecContext(ctx, `UPDATE recurrences SET location_generation=3,moved_to_authority_uid=$1,moved_to_project_uid=$2,moved_event_uid=$3 WHERE id=$4`, source.InstanceUID(), project.UID, event.UID, series.ID)
	require.NoError(t, err)
	_, err = source.(sqlStore).ExecContext(ctx, `UPDATE issues SET recurrence_uid=$1,recurrence_authority_uid=$2,recurrence_project_uid=$3,occurrence_key='2026-10-12',owner_authority_uid=$2 WHERE id=$4`, series.UID, source.InstanceUID(), project.UID, issue.ID)
	require.NoError(t, err)
	purgedOccurrence, _, err := source.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Purged occurrence", Author: "tester"})
	require.NoError(t, err)
	_, err = source.(sqlStore).ExecContext(ctx, `UPDATE issues SET recurrence_uid=$1,recurrence_authority_uid=$2,recurrence_project_uid=$3,occurrence_key='2026-10-13' WHERE id=$4`, series.UID, source.InstanceUID(), project.UID, purgedOccurrence.ID)
	require.NoError(t, err)
	_, err = source.PurgeIssue(ctx, purgedOccurrence.ID, "tester", nil)
	require.NoError(t, err)
	var purgedSeriesUID, purgedOccurrenceKey *string
	require.NoError(t, source.(sqlStore).QueryRowContext(ctx, `SELECT recurrence_uid,occurrence_key FROM purge_log WHERE issue_uid=$1`, purgedOccurrence.UID).Scan(&purgedSeriesUID, &purgedOccurrenceKey))
	require.Equal(t, &series.UID, purgedSeriesUID)
	require.Equal(t, new("2026-10-13"), purgedOccurrenceKey)
	localPeer, _, err := source.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Local peer", Author: "tester"})
	require.NoError(t, err)
	localLink, err := source.CreateLink(ctx, db.CreateLinkParams{FromIssueID: issue.ID, ToIssueID: localPeer.ID, Type: "related", Author: "tester"})
	require.NoError(t, err)
	_, err = source.(sqlStore).ExecContext(ctx, `UPDATE links SET from_authority_uid=$1,to_authority_uid=$1,from_project_uid=$2,to_project_uid=$2 WHERE id=$3`, source.InstanceUID(), project.UID, localLink.ID)
	require.NoError(t, err)
	originalProject, err := source.CreateProject(ctx, "received-history-source")
	require.NoError(t, err)
	receivedIssue, receivedEvent, err := source.CreateIssue(ctx, db.CreateIssueParams{ProjectID: originalProject.ID, Title: "Received history", Author: "tester"})
	require.NoError(t, err)
	_, err = source.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: receivedIssue.ID, FromProjectID: originalProject.ID, ToProjectID: project.ID, IfMatchRev: receivedIssue.Revision, Actor: "tester"})
	require.NoError(t, err)
	// Accepted history has a local destination owner and its actual original UID.
	_, err = source.(sqlStore).ExecContext(ctx, `UPDATE events SET project_id=$1,source_project_uid=$2 WHERE uid=$3`, project.ID, originalProject.UID, receivedEvent.UID)
	require.NoError(t, err)
	_, err = source.(sqlStore).ExecContext(ctx, `UPDATE issues SET location_generation=2,moved_to_authority_uid=$1,moved_to_project_uid=$2,moved_to_short_id=$3,moved_event_uid=$4 WHERE id=$5`, source.InstanceUID(), project.UID, issue.ShortID, event.UID, issue.ID)
	require.NoError(t, err)
	proofIssue, proof, proofPin := prepareMovedIssueProof(t, source, project.ID)
	var backup bytes.Buffer
	require.NoError(t, jsonl.Export(ctx, source, &backup, jsonl.ExportOptions{IncludeDeleted: true}))
	require.NoError(t, jsonl.Import(ctx, &backup, target))
	retainedProof, err := target.EntityAttribution(ctx, proofPin.ProjectUID, "issue", proofIssue.UID)
	require.NoError(t, err)
	require.Equal(t, proof, retainedProof)
	require.NoError(t, db.VerifyRootReceipt(proofPin, retainedProof))
	_, err = target.ProjectByUID(ctx, proofPin.ProjectUID)
	require.ErrorIs(t, err, db.ErrNotFound, "restore must not fabricate the original foreign project")

	require.NoError(t, target.(sqlStore).QueryRowContext(ctx, `SELECT recurrence_uid,occurrence_key FROM purge_log WHERE issue_uid=$1`, purgedOccurrence.UID).Scan(&purgedSeriesUID, &purgedOccurrenceKey))
	require.Equal(t, &series.UID, purgedSeriesUID)
	require.Equal(t, new("2026-10-13"), purgedOccurrenceKey)
	var receivedSourceUID, receivedHash string
	require.NoError(t, target.(sqlStore).QueryRowContext(ctx, `SELECT source_project_uid,content_hash FROM events WHERE uid=$1`, receivedEvent.UID).Scan(&receivedSourceUID, &receivedHash))
	require.Equal(t, originalProject.UID, receivedSourceUID)
	require.Equal(t, receivedEvent.ContentHash, receivedHash)
	var restoredSourceProject *string
	var restoredSourceBytes []byte
	require.NoError(t, target.(sqlStore).QueryRowContext(ctx, `SELECT source_project_uid,source_event_bytes FROM events WHERE uid=$1`, event.UID).Scan(&restoredSourceProject, &restoredSourceBytes))
	require.Equal(t, &project.UID, restoredSourceProject)
	require.Equal(t, originalBytes, restoredSourceBytes)
	var historyOwner *int64
	var historyIssueUID *string
	require.NoError(t, target.(sqlStore).QueryRowContext(ctx, `SELECT project_id,issue_uid,source_event_bytes FROM events WHERE uid=$1`, historyEvent.UID).Scan(&historyOwner, &historyIssueUID, &restoredSourceBytes))
	require.Nil(t, historyOwner)
	require.Equal(t, &historyIssue.UID, historyIssueUID)
	require.Equal(t, historyOriginalBytes, restoredSourceBytes)
	var seriesGeneration int64
	var seriesAuthority, seriesProject, seriesEvent *string
	require.NoError(t, target.(sqlStore).QueryRowContext(ctx, `SELECT location_generation,moved_to_authority_uid,moved_to_project_uid,moved_event_uid FROM recurrences WHERE uid=$1`, series.UID).Scan(&seriesGeneration, &seriesAuthority, &seriesProject, &seriesEvent))
	require.Equal(t, int64(3), seriesGeneration)
	require.Equal(t, new(source.InstanceUID()), seriesAuthority)
	require.Equal(t, &project.UID, seriesProject)
	require.Equal(t, &event.UID, seriesEvent)
	var occurrenceLocalSeries *int64
	require.NoError(t, target.(sqlStore).QueryRowContext(ctx, `SELECT recurrence_id FROM issues WHERE uid=$1`, issue.UID).Scan(&occurrenceLocalSeries))
	require.Nil(t, occurrenceLocalSeries, "logical remote series must not become a local scheduler owner")
	var occurrenceSeries, occurrenceAuthority, occurrenceProject, ownerAuthority *string
	require.NoError(t, target.(sqlStore).QueryRowContext(ctx, `SELECT recurrence_uid,recurrence_authority_uid,recurrence_project_uid,owner_authority_uid FROM issues WHERE uid=$1`, issue.UID).Scan(&occurrenceSeries, &occurrenceAuthority, &occurrenceProject, &ownerAuthority))
	require.Equal(t, &series.UID, occurrenceSeries)
	require.Equal(t, new(source.InstanceUID()), occurrenceAuthority)
	require.Equal(t, &project.UID, occurrenceProject)
	require.Equal(t, new(source.InstanceUID()), ownerAuthority)
	var generation int64
	var authority, projectUID, shortRef, eventUID *string
	require.NoError(t, target.(sqlStore).QueryRowContext(ctx, `SELECT location_generation,moved_to_authority_uid,moved_to_project_uid,moved_to_short_id,moved_event_uid FROM issues WHERE uid=$1`, issue.UID).Scan(&generation, &authority, &projectUID, &shortRef, &eventUID))
	require.Equal(t, int64(2), generation)
	require.Equal(t, new(source.InstanceUID()), authority)
	require.Equal(t, &project.UID, projectUID)
	require.Equal(t, &issue.ShortID, shortRef)
	require.Equal(t, &event.UID, eventUID)
	var retainedProject string
	require.NoError(t, target.(sqlStore).QueryRowContext(ctx, `SELECT project_uid FROM project_aliases WHERE alias_kind='name' AND alias_identity='retained-project'`).Scan(&retainedProject))
	require.Equal(t, project.UID, retainedProject)
	var localFromAuthority, localFromProject, localToAuthority, localToProject *string
	require.NoError(t, target.(sqlStore).QueryRowContext(ctx, `SELECT from_authority_uid,from_project_uid,to_authority_uid,to_project_uid FROM links WHERE from_issue_uid=$1 AND to_issue_uid=$2 AND type='related'`, issue.UID, localPeer.UID).Scan(&localFromAuthority, &localFromProject, &localToAuthority, &localToProject))
	require.Equal(t, new(source.InstanceUID()), localFromAuthority)
	require.Equal(t, &project.UID, localFromProject)
	require.Equal(t, new(source.InstanceUID()), localToAuthority)
	require.Equal(t, &project.UID, localToProject)
	var remoteUID, remoteAuthority, remoteProject string
	require.NoError(t, target.(sqlStore).QueryRowContext(ctx, `SELECT to_issue_uid,to_authority_uid,to_project_uid FROM links WHERE from_issue_uid=$1 AND type='blocks'`, issue.UID).Scan(&remoteUID, &remoteAuthority, &remoteProject))
	require.Equal(t, "01AAAAAAAAAAAAAAAAAAAAAAAA", remoteUID)
	require.Equal(t, "01BBBBBBBBBBBBBBBBBBBBBBBB", remoteAuthority)
	require.Equal(t, "01CCCCCCCCCCCCCCCCCCCCCCCC", remoteProject)
	var movedMappingIssue, movedMappingAuthority, movedMappingProject *string
	require.NoError(t, target.(sqlStore).QueryRowContext(ctx, `SELECT moved_issue_uid,moved_to_authority_uid,moved_to_project_uid FROM import_mappings WHERE external_id='task-1'`).Scan(&movedMappingIssue, &movedMappingAuthority, &movedMappingProject))
	require.Equal(t, &issue.UID, movedMappingIssue)
	require.Equal(t, new(source.InstanceUID()), movedMappingAuthority)
	require.Equal(t, &project.UID, movedMappingProject)

}

func prepareMovedIssueProof(t *testing.T, source db.Storage, destinationID int64) (db.Issue, db.AttributionReceipt, db.RootKeyPin) {
	t.Helper()
	ctx := t.Context()
	originalProject, err := source.CreateProject(ctx, "proof-source-project")
	require.NoError(t, err)
	issue, event, err := source.CreateIssue(ctx, db.CreateIssueParams{ProjectID: originalProject.ID, Title: "Signed task", Author: "tester"})
	require.NoError(t, err)
	public, private, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	pin := db.RootKeyPin{ProjectUID: originalProject.UID, AuthorityUID: "01AAAAAAAAAAAAAAAAAAAAAAAA", KeyID: db.RootPublicKeyID(public), PublicKey: public}
	require.NoError(t, source.PinRootAuthority(ctx, pin))
	_, err = source.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: originalProject.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 42, HubProjectUID: originalProject.UID, Actor: "member", Enabled: true})
	require.NoError(t, err)
	proof, err := db.SignRootReceipt(db.AttributionReceipt{Version: 1, ProjectUID: originalProject.UID, AuthorityUID: pin.AuthorityUID, KeyID: pin.KeyID, EventUID: event.UID, ContentHash: event.ContentHash, AccountableActor: "member", SourceActor: event.Actor, IngressInstanceUID: source.InstanceUID(), AcceptedAt: time.Now().UTC(), ResetEpoch: 1, Sequence: 1}, private)
	require.NoError(t, err)
	require.NoError(t, source.ApplyUpstreamAttribution(ctx, pin, proof))
	_, err = source.LeaveFederationReplica(ctx, originalProject.ID)
	require.NoError(t, err)
	_, err = source.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: originalProject.ID, ToProjectID: destinationID, IfMatchRev: issue.Revision, Actor: "tester"})
	require.NoError(t, err)
	_, _, err = source.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: originalProject.ID, Actor: "tester"})
	require.NoError(t, err)
	_, err = source.PurgeProject(ctx, db.PurgeProjectParams{ProjectID: originalProject.ID, Actor: "tester"})
	require.NoError(t, err)

	var retainedNameProject string
	q := source.(interface {
		QueryRowContext(context.Context, string, ...any) *sql.Row
	})
	require.NoError(t, q.QueryRowContext(ctx, `SELECT project_uid FROM project_aliases WHERE alias_kind='name' AND alias_identity=$1 AND authority_uid=$2`, originalProject.Name, source.InstanceUID()).Scan(&retainedNameProject))
	require.Equal(t, originalProject.UID, retainedNameProject, "old references belong to the current service, separately from their original proof authority")
	return issue, proof, pin
}

// Actual purge tombstones retain moved issue creation proofs after both source
// project and current body are gone; restoring must not manufacture either.
func TestIssueMovePurgedProofJSONLRoundTrip(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := t.Context()
			var source, target db.Storage
			if backend == "sqlite" {
				var err error
				source, err = sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "source.db"))
				require.NoError(t, err)
				target, err = sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "target.db"))
				require.NoError(t, err)
			} else {
				dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
				t.Cleanup(cleanup)
				var err error
				source, err = pgstore.Open(ctx, dsn)
				require.NoError(t, err)
				dsn2, cleanup2 := testenv.NewPostgresContainer(t, ctx)
				t.Cleanup(cleanup2)
				target, err = pgstore.Open(ctx, dsn2)
				require.NoError(t, err)
			}
			t.Cleanup(func() { require.NoError(t, source.Close()); require.NoError(t, target.Close()) })
			destination, err := source.CreateProject(ctx, "destination-project")
			require.NoError(t, err)
			issue, proof, pin := prepareMovedIssueProof(t, source, destination.ID)
			current, err := source.IssueByID(ctx, issue.ID)
			require.NoError(t, err)
			require.Equal(t, "verified", current.Verification, "creation attribution follows the stable moved issue UID")
			require.Equal(t, proof.AccountableActor, current.AccountableActor)
			require.Equal(t, pin.AuthorityUID, current.AuthorityUID)

			_, err = source.PurgeIssue(ctx, issue.ID, "tester", nil)
			require.NoError(t, err)
			for _, scope := range []struct {
				name    string
				allowed []string
				count   int
			}{
				{"owner", nil, 3}, {"current-project", []string{destination.UID}, 3},
				{"original-project", []string{pin.ProjectUID}, 0}, {"no-projects", []string{}, 0},
			} {
				t.Run(scope.name, func(t *testing.T) {
					readCtx := ctx
					if scope.allowed != nil {
						readCtx = db.WithAuthorizedProjects(ctx, scope.allowed)
					}
					count := 0
					for record, exportErr := range source.ExportAttribution(readCtx, db.ExportFilter{IncludeDeleted: true}) {
						require.NoError(t, exportErr)
						require.NotNil(t, record)
						count++
					}
					require.Equal(t, scope.count, count, "purged proof follows actual current tombstone scope")
				})
			}
			var output bytes.Buffer
			require.NoError(t, jsonl.Export(ctx, source, &output, jsonl.ExportOptions{IncludeDeleted: true}))
			require.NoError(t, jsonl.Import(ctx, bytes.NewReader(output.Bytes()), target))
			actual, err := target.EntityAttribution(ctx, pin.ProjectUID, "issue", issue.UID)
			require.NoError(t, err)
			require.Equal(t, proof, actual)
			require.NoError(t, db.VerifyRootReceipt(pin, actual))
			_, err = target.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
			require.ErrorIs(t, err, db.ErrNotFound)
			_, err = target.ProjectByUID(ctx, pin.ProjectUID)
			require.ErrorIs(t, err, db.ErrNotFound)
		})
	}
}
