package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
)

func TestClaimGatePendingMoveUsesCachedLease(t *testing.T) {
	for _, competing := range []bool{false, true} {
		t.Run(map[bool]string{false: "unclaimed", true: "competing lease"}[competing], func(t *testing.T) {
			store := openClaimGateHelperDB(t)
			source, issue := createClaimGateHelperIssue(t, store)
			target, err := store.CreateProject(t.Context(), "target-project")
			require.NoError(t, err)
			hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": "issue_not_found", "message": "not synchronized yet"}})
			}))
			t.Cleanup(hub.Close)
			enableClaimGateHelperSpoke(t, store, source, hub.URL, 99)
			enableClaimGateHelperSpoke(t, store, target, hub.URL, 100)
			moved, err := store.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "agent"})
			require.NoError(t, err)
			if competing {
				now := time.Now().UTC()
				principal := db.ClaimPrincipal{HolderInstanceUID: store.InstanceUID(), Holder: "other"}
				require.NoError(t, store.ApplyClaimStatus(t.Context(), target.ID, issue.UID, db.ClaimStatus{Held: true, Holder: principal, HubNow: now, Claim: &db.IssueClaim{ClaimUID: newClaimGateHelperUID(t), IssueUID: issue.UID, HolderInstanceUID: principal.HolderInstanceUID, Holder: principal.Holder, ClaimKind: "hard", AcquiredAt: now, UpdatedAt: now, Revision: 1}}))
			}
			err = requireFederatedIssueClaim(t.Context(), ServerConfig{DB: store}, target.ID, moved.Issue, "agent")
			if competing {
				assertClaimGateHelperAPIError(t, err, http.StatusConflict, "claim_denied")
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestSyncForwardedAssignmentDestinationReportsStalledSync(t *testing.T) {
	ctx := t.Context()
	store := openClaimGateHelperDB(t)
	source, issue := createClaimGateHelperIssue(t, store)
	destination, err := store.CreateProject(ctx, "destination-project")
	require.NoError(t, err)
	hubURL := "https://hub.example"
	enableClaimGateHelperSpoke(t, store, source, hubURL, 99)
	enableClaimGateHelperSpoke(t, store, destination, hubURL, 100)
	sourceBinding, err := store.FederationBindingByProject(ctx, source.ID)
	require.NoError(t, err)

	err = syncForwardedAssignmentDestination(ctx, ServerConfig{
		DB: store,
		FederationPullOnce: func(context.Context, db.FederationBinding, config.FederationCredential) error {
			return nil
		},
	}, sourceBinding, issue.UID, destination.UID)

	assertClaimGateHelperAPIError(t, err, http.StatusBadGateway, "federation_pull_failed")
	require.EqualError(t, err,
		"502 federation_pull_failed: destination sync stalled before loading the moved issue")
}
