package mcpserver

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/mcpevents"
	"go.kenn.io/kata/internal/notification"
)

func TestCronRecurringInboxWebhookEndToEnd(t *testing.T) {
	f := newEventsIntegrationFixture(t)
	discovered := f.method(t, "server/discover", map[string]any{})
	require.Contains(t, discovered["capabilities"].(map[string]any), "events")
	for _, loader := range []string{"load_issue_mutation", "load_issue_discovery", "load_coordination", "load_cron"} {
		f.tool(t, loader, map[string]any{})
	}
	parent := f.tool(t, "create", map[string]any{"title": "Recurring review", "idempotency_key": "cron-parent-fixture"})["issue"].(map[string]any)
	subscription := f.subscribe(t, "example-agent/worker")
	require.NotEmpty(t, subscription["id"])
	bootstrap := f.next(t, func(e mcpevents.Envelope) bool { return e.Data.Reason == "resync" })
	require.Equal(t, "example-agent/worker", bootstrap.Data.Recipient)
	jobUID := f.tool(t, "cron_new_uid", map[string]any{})["uid"].(string)
	created := f.tool(t, "cron_job_create", map[string]any{"uid": jobUID, "name": "Recurring review", "parent_ref": parent["uid"], "definition": map[string]any{"version": 1, "kind": "job", "enabled": true, "trigger": map[string]any{"kind": "interval", "interval_seconds": 60}, "action": map[string]any{"kind": "notify", "recipient": "example-agent/worker"}, "issue": map[string]any{"kind": "per-run", "title": "Review synthetic fixture"}, "overlap": "allow", "catchup": "latest"}})["job"].(map[string]any)
	boundAt, err := time.Parse(time.RFC3339Nano, created["created_at"].(string))
	require.NoError(t, err)
	due := boundAt.Add(61 * time.Second)
	f.clock.Store(due.UnixNano())
	sweeper := daemon.NewDueNotificationSweeper(f.store, daemon.NewEventPublisher(nil, nil), "UTC")
	require.NoError(t, sweeper.RunOnce(t.Context(), due))
	delivery := f.next(t, func(e mcpevents.Envelope) bool { return e.Data.Reason == "notification" })
	require.Equal(t, "kata.inbox.changed", delivery.Name)
	require.Equal(t, "example-agent/worker", delivery.Data.Recipient)
	require.Equal(t, f.project.UID, delivery.Data.ProjectUID)
	require.NotEmpty(t, delivery.Data.IssueUID)
	require.NotEmpty(t, delivery.Data.SourceEventUID)
	var requests []any
	cursor := ""
	for {
		args := map[string]any{"for": "example-agent/worker", "limit": 1}
		if cursor != "" {
			args["cursor"] = cursor
		}
		page := f.tool(t, "inbox", args)
		requests = append(requests, page["requests"].([]any)...)
		next, _ := page["next_cursor"].(string)
		if next == "" {
			break
		}
		require.NotEqual(t, cursor, next)
		cursor = next
	}
	require.Len(t, requests, 1)
	request := requests[0].(map[string]any)
	require.Equal(t, delivery.Data.IssueUID, request["issue_uid"])
	for _, recipient := range []string{"example-agent", "example-agent/other-worker"} {
		require.Empty(t, f.tool(t, "inbox", map[string]any{"for": recipient})["requests"])
	}
	shown := f.tool(t, "show", map[string]any{"ref": request["qualified_ref"]})["issue"].(map[string]any)
	require.Contains(t, shown["title"], "Review synthetic fixture")
	before, err := f.store.IssueByUID(t.Context(), delivery.Data.IssueUID, db.IncludeDeletedNo)
	require.NoError(t, err)
	runUID := f.tool(t, "cron_new_uid", map[string]any{})["uid"]
	observed := f.tool(t, "cron_run_observe", map[string]any{"uid": runUID, "expected_revision": 0, "job_uid": jobUID, "definition_event_uid": created["definition_event_uid"], "issue_uid": delivery.Data.IssueUID, "teammate": "worker", "status": "succeeded", "summary": map[string]any{"version": 1, "message": "Handled synthetic review"}})["run"].(map[string]any)
	require.Equal(t, "example-agent", observed["actor"])
	require.Equal(t, "worker", observed["teammate"])
	after, err := f.store.IssueByID(t.Context(), before.ID)
	require.NoError(t, err)
	require.Equal(t, before.Revision, after.Revision)
	require.Equal(t, before.Owner, after.Owner)
	require.Equal(t, before.Status, after.Status)
	require.Len(t, f.tool(t, "inbox", map[string]any{"for": "example-agent/worker"})["requests"], 1, "receipt, reads and observations do not clear the request")
	f.tool(t, "comment", map[string]any{"ref": request["qualified_ref"], "body": "Handled the synthetic review and recorded its result.", "teammate": "worker", "idempotency_key": "cron-result-fixture"})
	latest := f.tool(t, "show", map[string]any{"ref": request["qualified_ref"]})["issue"].(map[string]any)
	f.tool(t, "set_metadata", map[string]any{"ref": request["qualified_ref"], "revision": latest["revision"], "patch": map[string]any{notification.MetadataKey("example-agent/worker"): nil}})
	require.Empty(t, f.tool(t, "inbox", map[string]any{"for": "example-agent/worker"})["requests"])
	f.method(t, "events/unsubscribe", map[string]any{"name": "kata.inbox.changed", "arguments": map[string]any{"for": "example-agent/worker", "project_uid": f.project.UID}, "delivery": map[string]any{"mode": "webhook", "url": integrationCallback}})
}
