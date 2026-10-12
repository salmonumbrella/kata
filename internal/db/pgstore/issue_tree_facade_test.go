package pgstore_test

import (
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/testenv"
	"path/filepath"
	"testing"
)

func TestUIDMoveTreeFacade(t *testing.T) {
	ctx := t.Context()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	store, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	dbtest.RunUIDMoveTreeFacade(t, store)
}
func TestIssueTreeSnapshotScope(t *testing.T) {
	ctx := t.Context()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	store, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	dbtest.RunIssueTreeSnapshotScope(t, store)
}
func TestIssueTreeAcceptance(t *testing.T) {
	for _, pair := range []struct {
		name                    string
		sourcePG, destinationPG bool
	}{{"postgres-postgres", true, true}, {"sqlite-postgres", false, true}, {"postgres-sqlite", true, false}} {
		t.Run(pair.name, func(t *testing.T) {
			open := func(pg bool) db.Storage {
				if !pg {
					store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "kata.db"))
					require.NoError(t, err)
					t.Cleanup(func() { require.NoError(t, store.Close()) })
					return store
				}
				ctx := t.Context()
				dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
				t.Cleanup(cleanup)
				store, err := pgstore.Open(ctx, dsn)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, store.Close()) })
				return store
			}
			dbtest.RunIssueTreeAcceptance(t, open(pair.sourcePG), open(pair.destinationPG))
		})
	}
}

func TestIssueTreeMovedSeriesRoute(t *testing.T) {
	ctx := t.Context()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	store, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	dbtest.RunIssueTreeMovedSeriesRoute(t, store)
}

func TestIssueTreeMovedChildRequirement(t *testing.T) {
	ctx := t.Context()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	store, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	dbtest.RunIssueTreeMovedChildRequirement(t, store)
}

func runIssueTreeTransferPairs(t *testing.T, run func(*testing.T, db.Storage, db.Storage)) {
	for _, pair := range []struct {
		name                    string
		sourcePG, destinationPG bool
	}{{"postgres-postgres", true, true}, {"sqlite-postgres", false, true}, {"postgres-sqlite", true, false}} {
		t.Run(pair.name, func(t *testing.T) {
			open := func(pg bool) db.Storage {
				if !pg {
					store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "kata.db"))
					require.NoError(t, err)
					t.Cleanup(func() { require.NoError(t, store.Close()) })
					return store
				}
				ctx := t.Context()
				dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
				t.Cleanup(cleanup)
				store, err := pgstore.Open(ctx, dsn)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, store.Close()) })
				return store
			}
			run(t, open(pair.sourcePG), open(pair.destinationPG))
		})
	}
}
func TestIssueTreeFinalization(t *testing.T) {
	runIssueTreeTransferPairs(t, dbtest.RunIssueTreeFinalization)
}
func TestIssueTreeChangedSourceFinalization(t *testing.T) {
	runIssueTreeTransferPairs(t, dbtest.RunIssueTreeChangedSourceFinalization)
}
func TestIssueTreeSeriesTransfer(t *testing.T) {
	runIssueTreeTransferPairs(t, dbtest.RunIssueTreeSeriesTransfer)
}
func TestIssueTreeReverse(t *testing.T) { runIssueTreeTransferPairs(t, dbtest.RunIssueTreeReverse) }

func TestIssueTreeRolledBackEvidence(t *testing.T) {
	runIssueTreeTransferPairs(t, func(t *testing.T, source, destination db.Storage) {
		_, sourcePG := source.(*pgstore.Store)
		_, destinationPG := destination.(*pgstore.Store)
		dbtest.RunIssueTreeRolledBackEvidence(t, source, destination, sourcePG, destinationPG)
	})
}

func TestIssueTreeProviderDetach(t *testing.T) {
	ctx := t.Context()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	store, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	dbtest.RunIssueTreeProviderDetach(t, store)
}

func TestIssueTreeProviderDetachTransfer(t *testing.T) {
	runIssueTreeTransferPairs(t, dbtest.RunIssueTreeProviderDetachTransfer)
}
