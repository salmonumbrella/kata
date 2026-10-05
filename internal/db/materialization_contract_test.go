package db_test

import (
	"context"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

// Materialization retains the ordinary error-only storage contract. Generated
// execution-permission events are not part of this projection operation.
type ordinaryMaterializer interface {
	MaterializeFederatedProject(context.Context, int64) error
}

var (
	_ ordinaryMaterializer = (db.Storage)(nil)
	_ ordinaryMaterializer = (*sqlitestore.Store)(nil)
	_ ordinaryMaterializer = (*pgstore.Store)(nil)
)
