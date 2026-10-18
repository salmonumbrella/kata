package mcpserver

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"strings"

	"go.kenn.io/kata/internal/mcpevents"
	"go.kenn.io/kata/internal/notification"
	"go.kenn.io/kata/internal/teammate"
	"go.kenn.io/kata/pkg/client/generated"
)

func inboxSourceEventID(daemonUID, eventUID, recipient string) string {
	data, _ := json.Marshal([]string{daemonUID, eventUID, "kata.inbox.changed", recipient})
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

func inboxEventReason(row generated.EventEnvelope, args mcpevents.Arguments) string {
	if args.IssueUID != "" && strings.HasPrefix(row.Type, "issue.") && (row.IssueUID == nil || *row.IssueUID != args.IssueUID) {
		return ""
	}
	switch row.Type {
	case "issue.closed", "issue.reopened", "issue.soft_deleted", "issue.restored", "issue.moved", "project.removed", "project.restored", "project.merged", "project.renamed":
		return "visibility"
	case "issue.created", "issue.snapshot", "issue.metadata_updated", "issue.updated":
	default:
		return ""
	}
	var payload map[string]any
	if json.Unmarshal(row.Payload, &payload) != nil {
		return "visibility"
	}
	key := notification.MetadataKey(args.For)
	switch row.Type {
	case "issue.created", "issue.snapshot":
		metadata, _ := payload["metadata"].(map[string]any)
		if _, ok := parseInboxNotification(metadata[key]); ok {
			return "notification"
		}
		if row.Type == "issue.snapshot" {
			return "visibility"
		}
	case "issue.metadata_updated":
		diff, ok := payload["diff"].(map[string]any)
		if !ok {
			return "visibility"
		}
		change, present := diff[key]
		if !present {
			return ""
		}
		values, ok := change.(map[string]any)
		if !ok {
			return "visibility"
		}
		_, from := parseInboxNotification(values["from"])
		_, to := parseInboxNotification(values["to"])
		if from || to {
			return "notification"
		}
	case "issue.updated":
		for _, field := range []string{"status", "deleted_at", "closed_at"} {
			if _, ok := payload[field]; ok {
				return "visibility"
			}
		}
	}
	return ""
}

// parseInboxNotification is shared by current inbox reads and event matching.
// Bad teammate metadata is ignored without hiding an otherwise valid request.
func parseInboxNotification(raw any) (notification.Value, bool) {
	var value struct {
		From     string `json:"from"`
		Message  string `json:"message"`
		Teammate any    `json:"teammate"`
	}
	b, err := json.Marshal(raw)
	if err != nil || json.Unmarshal(b, &value) != nil || strings.TrimSpace(value.From) == "" || strings.TrimSpace(value.Message) == "" {
		return notification.Value{}, false
	}
	handle, _ := value.Teammate.(string)
	if teammate.Validate(handle) != nil {
		handle = ""
	}
	return notification.Value{From: value.From, Message: value.Message, Teammate: handle}, true
}
