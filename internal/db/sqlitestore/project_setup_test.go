package sqlitestore_test

import (
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db/dbtest"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"testing"
)

func TestProjectSetup(t *testing.T) { dbtest.RunProjectSetup(t, openTestDB(t)) }
func TestProjectSetupConcurrent(t *testing.T) {
	store := openTestDB(t)
	other, err := sqlitestore.Open(t.Context(), store.Path())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, other.Close()) })
	dbtest.RunProjectSetupConcurrent(t, store, other)
}
