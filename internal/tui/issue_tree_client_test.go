package tui

import (
	"context"
	"encoding/json/v2"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/pkg/client/generated"
	"net/http"
	"testing"
)

func TestTUIIssueTreeClientPreservesPartialOutcome(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/issue-moves", r.URL.Path)
		var body generated.IssueTreeMoveRequestBody
		require.NoError(t, json.UnmarshalRead(r.Body, &body))
		require.Equal(t, "01AAAAAAAAAAAAAAAAAAAAAAAA", body.Request.RequestUID)
		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte(`{"result":{"state":"partial","request_uid":"01AAAAAAAAAAAAAAAAAAAAAAAA","applied":[],"finalized":[],"conflicts":[],"unknown_roots":["01BBBBBBBBBBBBBBBBBBBBBBBB"],"retry_action":"retry with the same request UID"}}`))
		require.NoError(t, err)
	})
	api, ok := any(client).(interface {
		MoveTree(context.Context, generated.MoveTreeRequest, bool) (*generated.IssueTreeMoveResponseBody, error)
	})
	require.True(t, ok, "TUI client exposes typed complete-tree moves")
	if !ok {
		return
	}
	result, err := api.MoveTree(t.Context(), generated.MoveTreeRequest{RequestUID: "01AAAAAAAAAAAAAAAAAAAAAAAA", Actor: "tester", ActorAuthorityUID: "01AAAAAAAAAAAAAAAAAAAAAAAA", SourceAuthorityUID: "01AAAAAAAAAAAAAAAAAAAAAAAA", SourceProjectUID: "01BBBBBBBBBBBBBBBBBBBBBBBB", IssueUID: "01CCCCCCCCCCCCCCCCCCCCCCCC", DestinationAuthorityUID: "01AAAAAAAAAAAAAAAAAAAAAAAA", DestinationProjectUID: "01DDDDDDDDDDDDDDDDDDDDDDDD"}, false)
	require.NoError(t, err)
	require.Equal(t, "partial", result.Result.State)
	require.Equal(t, "retry with the same request UID", *result.Result.RetryAction)
}

func TestTUIDetailMoveOpensDestinationPrompt(t *testing.T) {
	api, detail, keys := setupMutationTest(t)
	_, command := detail.Update(runeKey('M'), keys, api)
	require.NotNil(t, command, "M opens a move preview destination prompt in the detail view")
}
