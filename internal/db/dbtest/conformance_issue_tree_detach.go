package dbtest

import (
	"context"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
	"testing"
	"time"
)

func RunIssueTreeProviderDetach(t *testing.T, store db.Storage) {
	runIssueTreeProviderDetach(t, store, store)
}
func RunIssueTreeProviderDetachTransfer(t *testing.T, source, destination db.Storage) {
	runIssueTreeProviderDetach(t, source, destination)
}
func runIssueTreeProviderDetach(t *testing.T, store, destination db.Storage) {
	ctx := t.Context()
	source, err := store.CreateProject(ctx, "source-project")
	require.NoError(t, err)
	target, err := destination.CreateProject(ctx, "target-project")
	require.NoError(t, err)
	instant := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	batch := db.ImportBatchParams{ProjectID: source.ID, Source: "example-provider", Actor: "importer", Items: []db.ImportItem{{ExternalID: "source-1", Title: "Imported work", Author: "tester", Status: "open", CreatedAt: instant, UpdatedAt: instant}}}
	_, _, err = store.ImportBatch(ctx, batch)
	require.NoError(t, err)
	mapping, err := store.ImportMappingBySource(ctx, source.ID, batch.Source, "issue", "source-1")
	require.NoError(t, err)
	require.NotNil(t, mapping.IssueID)
	issue, err := store.IssueByID(ctx, *mapping.IssueID)
	require.NoError(t, err)
	requestUID, err := uid.New()
	require.NoError(t, err)
	request := db.MoveTreeRequest{SourceAuthorityUID: store.InstanceUID(), SourceProjectUID: source.UID, IssueUID: issue.UID, DestinationAuthorityUID: destination.InstanceUID(), DestinationProjectUID: target.UID, RequestUID: requestUID, ActorAuthorityUID: store.InstanceUID(), Actor: "tester", ProviderDisposition: "detach"}
	var result db.MoveTreeResult
	if store.InstanceUID() == destination.InstanceUID() {
		result, err = store.MoveIssueTree(ctx, request)
	} else {
		snapshot, exportErr := store.ExportIssueTree(ctx, request)
		require.NoError(t, exportErr)
		accepted, acceptErr := destination.AcceptIssueTree(ctx, snapshot, request)
		require.NoError(t, acceptErr)
		verified := db.WithMoveDestinationCommitVerifier(ctx, func(ctx context.Context, r db.MoveTreeRequest, provided db.MoveTreeResult) error {
			actual, err := destination.LookupIssueTreeAcceptance(ctx, r)
			if err != nil {
				return err
			}
			return db.ValidateCommittedMoveEvidence(provided, actual)
		})
		result, err = store.FinalizeIssueTree(verified, request, db.MoveTreePreview{SnapshotDigest: snapshot.Digest, Members: snapshot.Portions[0].Members}, accepted)
	}
	require.NoError(t, err)
	require.Equal(t, "completed", result.State)
	_, err = destination.ImportMappingBySource(ctx, target.ID, batch.Source, "issue", "source-1")
	require.ErrorIs(t, err, db.ErrNotFound, "detach leaves no destination provider owner")
	suppression, err := store.ImportMappingBySource(ctx, source.ID, batch.Source, "issue", "source-1")
	require.NoError(t, err)
	if store.InstanceUID() == destination.InstanceUID() {
		require.Nil(t, suppression.IssueID)
		require.Equal(t, issue.UID, *suppression.MovedIssueUID)
	}
	batch.Items[0].UpdatedAt = instant.Add(time.Hour)
	polled, events, err := store.ImportBatch(ctx, batch)
	require.NoError(t, err)
	require.Zero(t, polled.Created)
	require.Zero(t, polled.Updated)
	require.Empty(t, events)
}
