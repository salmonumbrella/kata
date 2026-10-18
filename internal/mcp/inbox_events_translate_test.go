package mcpserver

import (
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/mcpevents"
	"go.kenn.io/kata/internal/notification"
	"go.kenn.io/kata/pkg/client/generated"
)

const sourceProject = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
const sourceOtherProject = "01ARZ3NDEKTSV4RRFFQ69G5FAW"
const sourceIssue = "01ARZ3NDEKTSV4RRFFQ69G5FAX"
const sourceDaemon = "01ARZ3NDEKTSV4RRFFQ69G5FAY"

func sourceEvent(kind string, n int64, payload string) generated.EventEnvelope {
	uid := fmt.Sprintf("01ARZ3NDEKTSV4RRFFQ%07d", n)
	issue := sourceIssue
	return generated.EventEnvelope{EventID: n, EventUID: uid, Type: kind, IssueUID: &issue, ProjectID: 1, ProjectUID: sourceProject, ProjectName: "spoke-project", CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Actor: "example-user", OriginInstanceUID: sourceDaemon, ContentHash: strings.Repeat("a", 64), Payload: []byte(payload)}
}

func TestSourceTranslatesOnlyExactInboxChanges(t *testing.T) {
	key := notification.MetadataKey("example-user/worker")
	valid := `{"from":"example-user","message":"do work"}`
	for _, tc := range []struct{ name, kind, payload, reason string }{
		{"notify", "issue.metadata_updated", `{"diff":{"` + key + `":{"from":null,"to":` + valid + `}}}`, "notification"},
		{"clear", "issue.metadata_updated", `{"diff":{"` + key + `":{"from":` + valid + `,"to":null}}}`, "notification"},
		{"create", "issue.created", `{"metadata":{"` + key + `":` + valid + `}}`, "notification"},
		{"snapshot", "issue.snapshot", `{"metadata":{"` + key + `":` + valid + `}}`, "notification"},
		{"snapshot removes", "issue.snapshot", `{"metadata":{}}`, "visibility"},
		{"wrong recipient", "issue.metadata_updated", `{"diff":{"` + notification.MetadataKey("example-user") + `":{"to":` + valid + `}}}`, ""},
		{"attention", "issue.metadata_updated", `{"diff":{"work.attention":{"to":"needs-human"}}}`, ""},
		{"comment", "issue.commented", `{"body":"notify example-user/worker"}`, ""},
		{"assignment", "issue.assigned", `{"owner":"example-user/worker"}`, ""},
		{"invalid notification", "issue.created", `{"metadata":{"` + key + `":{"from":"","message":"x"}}}`, ""},
		{"title", "issue.updated", `{"title":"new title"}`, ""},
		{"status", "issue.updated", `{"status":"closed"}`, "visibility"},
		{"closed", "issue.closed", `{}`, "visibility"},
		{"reopened", "issue.reopened", `{}`, "visibility"},
		{"deleted", "issue.soft_deleted", `{}`, "visibility"},
		{"restored", "issue.restored", `{}`, "visibility"},
		{"moved", "issue.moved", `{}`, "visibility"},
		{"project removed", "project.removed", `{}`, "visibility"},
		{"project restored", "project.restored", `{}`, "visibility"},
		{"project merged", "project.merged", `{}`, "visibility"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := sourceEvent(tc.kind, 1, tc.payload)
			require.Equal(t, tc.reason, inboxEventReason(row, mcpevents.Arguments{For: "example-user/worker"}))
		})
	}
}

func TestSourceParserPreservesInboxCompatibility(t *testing.T) {
	for _, handle := range []any{"worker", "@invalid", 42, nil} {
		value, ok := parseInboxNotification(map[string]any{"from": " sender ", "message": " work ", "teammate": handle})
		require.True(t, ok)
		require.Equal(t, " sender ", value.From)
		require.Equal(t, " work ", value.Message)
		if handle == "worker" {
			require.Equal(t, "worker", value.Teammate)
		} else {
			require.Empty(t, value.Teammate)
		}
	}
	for _, value := range []any{nil, "string", map[string]any{"from": "sender", "message": " "}, map[string]any{"from": 42, "message": "work"}} {
		_, ok := parseInboxNotification(value)
		require.False(t, ok)
	}
}

func TestSourceIssueFilterCannotWiden(t *testing.T) {
	key := notification.MetadataKey("example-user")
	row := sourceEvent("issue.created", 1, `{"metadata":{"`+key+`":{"from":"sender","message":"work"}}}`)
	args := mcpevents.Arguments{For: "example-user", ProjectUID: sourceProject, IssueUID: sourceIssue}
	require.Equal(t, "notification", inboxEventReason(row, args))
	other := "01ARZ3NDEKTSV4RRFFQ69G5FAZ"
	row.IssueUID = &other
	require.Empty(t, inboxEventReason(row, args))
	row.IssueUID = nil
	require.Empty(t, inboxEventReason(row, args))
	row.Type = "project.removed"
	require.Equal(t, "visibility", inboxEventReason(row, args))
}

func FuzzSourceExactRecipient(f *testing.F) {
	f.Add("example-user", "example-user/worker")
	f.Add("example-user/worker", "example-user/worker")
	f.Add("actor/équipe", "actor/équipe")
	f.Fuzz(func(t *testing.T, recipient, target string) {
		// The event matcher consumes only recipients validated by Resolve.
		recipient, e1 := notification.NormalizeRecipient(recipient)
		target, e2 := notification.NormalizeRecipient(target)
		if e1 != nil || e2 != nil {
			return
		}
		b, err := json.Marshal(map[string]any{"diff": map[string]any{notification.MetadataKey(recipient): map[string]any{"to": notification.Value{From: "sender", Message: "work"}}}})
		require.NoError(t, err)
		reason := inboxEventReason(sourceEvent("issue.metadata_updated", 1, string(b)), mcpevents.Arguments{For: target})
		want := ""
		if recipient == target {
			want = "notification"
		}
		require.Equal(t, want, reason)
	})
}
