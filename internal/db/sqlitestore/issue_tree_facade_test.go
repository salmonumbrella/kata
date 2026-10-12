package sqlitestore_test

import (
	"go.kenn.io/kata/internal/db/dbtest"
	"testing"
)

func TestUIDMoveTreeFacade(t *testing.T)      { dbtest.RunUIDMoveTreeFacade(t, openTestDB(t)) }
func TestIssueTreeSnapshotScope(t *testing.T) { dbtest.RunIssueTreeSnapshotScope(t, openTestDB(t)) }
func TestIssueTreeAcceptance(t *testing.T) {
	dbtest.RunIssueTreeAcceptance(t, openTestDB(t), openTestDB(t))
}

func TestIssueTreeMovedSeriesRoute(t *testing.T) {
	dbtest.RunIssueTreeMovedSeriesRoute(t, openTestDB(t))
}
func TestIssueTreeMovedChildRequirement(t *testing.T) {
	dbtest.RunIssueTreeMovedChildRequirement(t, openTestDB(t))
}
func TestIssueTreeFinalization(t *testing.T) {
	dbtest.RunIssueTreeFinalization(t, openTestDB(t), openTestDB(t))
}
func TestIssueTreeChangedSourceFinalization(t *testing.T) {
	dbtest.RunIssueTreeChangedSourceFinalization(t, openTestDB(t), openTestDB(t))
}
func TestIssueTreeSeriesTransfer(t *testing.T) {
	dbtest.RunIssueTreeSeriesTransfer(t, openTestDB(t), openTestDB(t))
}
func TestIssueTreeReverse(t *testing.T) { dbtest.RunIssueTreeReverse(t, openTestDB(t), openTestDB(t)) }

func TestIssueTreeRolledBackEvidence(t *testing.T) {
	dbtest.RunIssueTreeRolledBackEvidence(t, openTestDB(t), openTestDB(t), false, false)
}

func TestIssueTreeProviderDetach(t *testing.T) { dbtest.RunIssueTreeProviderDetach(t, openTestDB(t)) }

func TestIssueTreeProviderDetachTransfer(t *testing.T) {
	dbtest.RunIssueTreeProviderDetachTransfer(t, openTestDB(t), openTestDB(t))
}
