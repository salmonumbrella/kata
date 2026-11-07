package mcpserver

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/testenv"
	kataclient "go.kenn.io/kata/pkg/client"
)

func newTypedCommentSession(t *testing.T) (*sdkmcp.ClientSession, *sqlitestore.Store) {
	t.Helper()
	env := testenv.New(t)
	project, err := env.DB.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)
	_, err = env.DB.CreateProject(t.Context(), "other-project")
	require.NoError(t, err)
	client, err := kataclient.NewWithHTTPClient(env.URL, env.HTTP)
	require.NoError(t, err)
	scope, err := NewBoundScope(ProjectIdentity{ID: project.ID, UID: project.UID, Name: project.Name})
	require.NoError(t, err)
	return connectTestServerWithOptions(t, Options{Client: client, Scope: scope, Actor: "example-agent", Version: "test"}), env.DB
}

// A typed comment must retain its relation in the mutation receipt, not only
// after a separate show call. Retries must not create a second assertion.
func TestMCPTypedCommentWorkflow(t *testing.T) {
	for _, kind := range []string{"reply", "confirm", "refute", "supersede"} {
		t.Run(kind, func(t *testing.T) {
			session, _ := newTypedCommentSession(t)
			created := callWorkflowTool(t, session, "kata.create", map[string]any{"title": "Inspect the fixture finding", "idempotency_key": "typed-create"})
			ref := created["issue"].(map[string]any)["ref"].(string)
			root := callWorkflowTool(t, session, "kata.comment", map[string]any{"ref": ref, "body": "The fixture needs inspection.", "idempotency_key": "typed-root"})["comment"].(map[string]any)
			require.NotContains(t, root, "reply")
			shown := callWorkflowTool(t, session, "kata.show", map[string]any{"ref": ref})
			handle := shown["issue"].(map[string]any)["comments"].([]any)[0].(map[string]any)["handle"].(string)
			args := map[string]any{"ref": ref, "reply_to": handle, "kind": kind, "body": "Checked the isolated fixture and recorded the observed result.", "teammate": "reviewer", "idempotency_key": "typed-answer"}
			first := callWorkflowTool(t, session, "kata.comment", args)
			comment := first["comment"].(map[string]any)
			require.Equal(t, true, first["changed"])
			require.Equal(t, "issue.commented", first["event"].(map[string]any)["type"])
			require.Equal(t, "example-agent", comment["author"])
			require.Equal(t, "reviewer", comment["teammate"])
			require.Contains(t, comment, "reply", "the mutation receipt must retain the resolved typed relation")
			require.Equal(t, root["uid"], comment["reply"].(map[string]any)["uid"])
			require.Equal(t, kind, comment["reply"].(map[string]any)["kind"])

			retry := callWorkflowTool(t, session, "kata.comment", args)
			require.Equal(t, false, retry["changed"])
			require.Equal(t, true, retry["reused"])
			require.Equal(t, comment, retry["comment"])
			require.NotContains(t, retry, "event")

			threadOutput := callWorkflowTool(t, session, "kata.show", map[string]any{"ref": ref, "thread": handle})
			require.Contains(t, threadOutput["issue"], "comments", "%s", mustJSON(t, threadOutput))
			thread := threadOutput["issue"].(map[string]any)["comments"].([]any)
			require.Len(t, thread, 2, "the retry must not duplicate the comment")
			require.Contains(t, thread[0], "backlinks", "%s", mustJSON(t, threadOutput))
			backlinks := thread[0].(map[string]any)["backlinks"].([]any)
			require.Len(t, backlinks, 1)
			require.Equal(t, comment["uid"], backlinks[0].(map[string]any)["uid"])
			require.Equal(t, kind, backlinks[0].(map[string]any)["kind"])
			require.Equal(t, root["uid"], thread[1].(map[string]any)["reply"].(map[string]any)["uid"])
			require.Equal(t, kind, thread[1].(map[string]any)["reply"].(map[string]any)["kind"])

			duplicate := maps.Clone(args)
			duplicate["idempotency_key"] = "typed-duplicate"
			message := typedCommentError(t, session, duplicate)
			require.Contains(t, message, "duplicate_reply")
			require.Contains(t, message, thread[1].(map[string]any)["handle"].(string))
			duplicate["force"] = true
			forced := callWorkflowTool(t, session, "kata.comment", duplicate)
			require.Equal(t, true, forced["changed"])
			require.NotEqual(t, comment["uid"], forced["comment"].(map[string]any)["uid"])
			require.Equal(t, comment["reply"], forced["comment"].(map[string]any)["reply"])
			require.Equal(t, false, callWorkflowTool(t, session, "kata.comment", duplicate)["changed"])

			// A distinct teammate can independently assess the same finding.
			duplicate["force"], duplicate["teammate"], duplicate["idempotency_key"] = false, "second-reviewer", "typed-second-reviewer"
			callWorkflowTool(t, session, "kata.comment", duplicate)
			for field, value := range map[string]any{
				"kind":     map[string]string{"reply": "confirm", "confirm": "refute", "refute": "supersede", "supersede": "reply"}[kind],
				"reply_to": forced["comment"].(map[string]any)["uid"],
				"body":     "Changed the assessment after checking a different fixture result.",
				"teammate": "different-reviewer",
			} {
				conflict := maps.Clone(args)
				conflict[field] = value
				require.Contains(t, typedCommentError(t, session, conflict), "idempotency_mismatch", field)
			}
		})
	}
}

func typedCommentError(t *testing.T, session *sdkmcp.ClientSession, args map[string]any) string {
	t.Helper()
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.comment", Arguments: args})
	require.NoError(t, err)
	require.True(t, result.IsError, "%s", mustJSON(t, result))
	var message strings.Builder
	for _, content := range result.Content {
		if text, ok := content.(*sdkmcp.TextContent); ok {
			message.WriteString(text.Text)
		}
	}
	return message.String()
}

// References may address a root on another issue in the same project. Even
// force must reject a target in a different project or a removed target.
func TestMCPTypedCommentTargets(t *testing.T) {
	for _, form := range []string{"uid", "issue", "project"} {
		t.Run(form, func(t *testing.T) {
			session, store := newTypedCommentSession(t)
			project, err := store.ProjectByName(t.Context(), "spoke-project")
			require.NoError(t, err)
			source, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Record the assessment", Author: "finder"})
			require.NoError(t, err)
			target, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Inspect the finding", Author: "finder"})
			require.NoError(t, err)
			root, _, err := store.CreateComment(t.Context(), db.CreateCommentParams{IssueID: target.ID, Author: "finder", Body: "The fixture finding."})
			require.NoError(t, err)
			replyTo := strings.ToLower(root.UID)
			if form != "uid" {
				replyTo = target.ShortID + ":" + strings.ToLower(root.UID[20:])
			}
			if form == "project" {
				replyTo = project.Name + "#" + replyTo
			}
			args := map[string]any{"ref": source.ShortID, "reply_to": replyTo, "kind": "supersede", "body": "Use the updated fixture result.", "idempotency_key": "target-form"}
			out := callWorkflowTool(t, session, "kata.comment", args)
			require.Equal(t, root.UID, out["comment"].(map[string]any)["reply"].(map[string]any)["uid"])
			// The same resolved UID is an exact retry, irrespective of its spelling.
			args["reply_to"] = root.UID
			require.Equal(t, false, callWorkflowTool(t, session, "kata.comment", args)["changed"])
			inbound := callWorkflowTool(t, session, "kata.show", map[string]any{"ref": target.ShortID, "inbound": "finder", "kind": "supersede"})["issue"].(map[string]any)["comments"].([]any)
			require.Len(t, inbound, 1)
			require.Equal(t, out["comment"].(map[string]any)["uid"], inbound[0].(map[string]any)["uid"])

			other, err := store.ProjectByName(t.Context(), "other-project")
			require.NoError(t, err)
			target, err = store.IssueByID(t.Context(), target.ID)
			require.NoError(t, err)
			_, err = store.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: target.ID, FromProjectID: project.ID, ToProjectID: other.ID, IfMatchRev: target.Revision, Actor: "finder"})
			require.NoError(t, err)
			// A committed retry is still reusable, but cannot expose a target
			// that has moved outside the MCP server's startup scope.
			retry := callWorkflowTool(t, session, "kata.comment", args)
			require.Equal(t, false, retry["changed"])
			require.NotContains(t, retry["comment"].(map[string]any), "reply")
			args["force"], args["idempotency_key"] = true, "outside-target"
			require.Contains(t, typedCommentError(t, session, args), "comment_not_found")
		})
	}
}

// Evidence is measured after Unicode whitespace normalization, not in bytes.
// Force only bypasses duplicates; it cannot relax this evidence requirement.
func TestMCPTypedCommentEvidence(t *testing.T) {
	session, _ := newTypedCommentSession(t)
	created := callWorkflowTool(t, session, "kata.create", map[string]any{"title": "Check evidence boundaries", "idempotency_key": "evidence-create"})
	ref := created["issue"].(map[string]any)["ref"].(string)
	root := callWorkflowTool(t, session, "kata.comment", map[string]any{"ref": ref, "body": "Finding", "idempotency_key": "evidence-root"})["comment"].(map[string]any)
	rapid.Check(t, func(rt *rapid.T) {
		kind := rapid.SampledFrom([]string{"confirm", "refute"}).Draw(rt, "kind")
		count := rapid.IntRange(1, 45).Draw(rt, "characters")
		letter := rapid.SampledFrom([]string{"x", "é", "界", "🙂"}).Draw(rt, "letter")
		body := "\u2003\t" + strings.Repeat(letter, count) + " \n"
		args := map[string]any{"ref": ref, "reply_to": root["uid"], "kind": kind, "body": body, "force": true, "idempotency_key": fmt.Sprintf("evidence-%s-%d-%s", kind, count, letter)}
		result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.comment", Arguments: args})
		require.NoError(rt, err)
		require.Equal(rt, count < 40, result.IsError, "%s", mustJSON(t, result))
	})
	for _, kind := range []string{"confirm", "refute"} {
		for _, body := range []string{strings.Repeat("é", 39), strings.Repeat("é \t\n", 20)} {
			args := map[string]any{"ref": ref, "reply_to": root["uid"], "kind": kind, "body": body, "force": true, "idempotency_key": "short-evidence-" + kind}
			require.Contains(t, typedCommentError(t, session, args), "40 characters")
		}
		args := map[string]any{"ref": ref, "reply_to": root["uid"], "kind": kind, "body": strings.Repeat("é", 40), "force": true, "idempotency_key": "exact-evidence-" + kind}
		callWorkflowTool(t, session, "kata.comment", args)
	}
}

// Invalid requests cannot create comments, including when force is requested.
func TestMCPTypedCommentRejections(t *testing.T) {
	session, store := newTypedCommentSession(t)
	created := callWorkflowTool(t, session, "kata.create", map[string]any{"title": "Reject invalid relations", "idempotency_key": "rejection-create"})
	issue := created["issue"].(map[string]any)
	root := callWorkflowTool(t, session, "kata.comment", map[string]any{"ref": issue["ref"], "body": "Finding", "idempotency_key": "rejection-root"})["comment"].(map[string]any)
	base := map[string]any{"ref": issue["ref"], "reply_to": root["uid"], "kind": "refute", "body": "Checked the isolated fixture and recorded the observed result.", "force": true, "idempotency_key": "invalid-relation"}
	before, err := store.MaxEventID(t.Context())
	require.NoError(t, err)
	for _, tc := range []struct {
		name, field string
		value       any
		want        string
	}{
		{"kind without target", "reply_to", nil, "supplied together"},
		{"target without kind", "kind", nil, "supplied together"},
		{"misspelled kind", "kind", "refure", "kind"},
		{"malformed target", "reply_to", "c:a", "invalid comment reference"},
		{"unknown target", "reply_to", "c:000000", "comment_not_found"},
		{"short evidence", "body", "Confirmed", "40 characters"},
		{"blank body", "body", " \t\n", "body must not be empty"},
		{"blank key", "idempotency_key", " \t", "idempotency_key must not be empty"},
		{"outside source scope", "ref", "other-project#abcd", "outside the MCP startup scope"},
		{"forged actor", "actor", "someone-else", "actor"},
		{"client-chosen self UID", "uid", root["uid"], "uid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := maps.Clone(base)
			if tc.value == nil {
				delete(args, tc.field)
			} else {
				args[tc.field] = tc.value
			}
			require.Contains(t, typedCommentError(t, session, args), tc.want)
		})
	}
	after, err := store.MaxEventID(t.Context())
	require.NoError(t, err)
	require.Equal(t, before, after, "rejections must not persist a comment or event")

	callWorkflowTool(t, session, "kata.close", map[string]any{"ref": issue["ref"], "reason": "done", "message": "Completed the isolated fixture validation checks.", "evidence": []any{map[string]any{"type": "test", "command": "fixture check"}}})
	for _, kind := range []string{"reply", "confirm", "refute", "supersede"} {
		base["kind"] = kind
		require.Contains(t, typedCommentError(t, session, base), "issue_closed")
	}
}

func TestMCPTypedCommentRemovedTarget(t *testing.T) {
	session, store := newTypedCommentSession(t)
	project, err := store.ProjectByName(t.Context(), "spoke-project")
	require.NoError(t, err)
	source, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Assess a removed finding", Author: "finder"})
	require.NoError(t, err)
	target, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Original finding", Author: "finder"})
	require.NoError(t, err)
	root, _, err := store.CreateComment(t.Context(), db.CreateCommentParams{IssueID: target.ID, Author: "finder", Body: "Finding"})
	require.NoError(t, err)
	_, _, _, err = store.SoftDeleteIssue(t.Context(), target.ID, "finder")
	require.NoError(t, err)
	before, err := store.MaxEventID(t.Context())
	require.NoError(t, err)
	for _, kind := range []string{"reply", "confirm", "refute", "supersede"} {
		args := map[string]any{"ref": source.ShortID, "reply_to": root.UID, "kind": kind, "body": "Checked the isolated fixture and recorded the observed result.", "force": true, "idempotency_key": "removed-target-" + kind}
		require.Contains(t, typedCommentError(t, session, args), "comment_not_found")
	}
	after, err := store.MaxEventID(t.Context())
	require.NoError(t, err)
	require.Equal(t, before, after)
}

// Force must not elevate a read-only principal. Token-bound writes use the
// daemon's actor, including duplicate detection and teammate attribution.
func TestMCPTypedCommentPermissions(t *testing.T) {
	for _, readonly := range []bool{false, true} {
		t.Run(fmt.Sprintf("readonly=%t", readonly), func(t *testing.T) {
			option := testenv.WithRequireTokenIdentity()
			if readonly {
				option = testenv.WithInsecureReadonly()
			}
			env := testenv.New(t, option)
			project, err := env.DB.CreateProject(t.Context(), "spoke-project")
			require.NoError(t, err)
			issue, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Check comment authority", Author: "finder"})
			require.NoError(t, err)
			root, _, err := env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: issue.ID, Author: "finder", Body: "Finding"})
			require.NoError(t, err)
			var options []kataclient.Option
			if !readonly {
				_, _, err = env.DB.CreateAPIToken(t.Context(), db.CreateAPITokenParams{PlaintextToken: "fixture-token", Actor: "bound-actor", AdminActor: "fixture-admin"})
				require.NoError(t, err)
				options = append(options, kataclient.WithRequestEditor(func(_ context.Context, r *http.Request) error {
					r.Header.Set("Authorization", "Bearer fixture-token")
					return nil
				}))
			}
			client, err := kataclient.NewWithHTTPClient(env.URL, env.HTTP, options...)
			require.NoError(t, err)
			scope, err := NewBoundScope(ProjectIdentity{ID: project.ID, UID: project.UID, Name: project.Name})
			require.NoError(t, err)
			session := connectTestServerWithOptions(t, Options{Client: client, Scope: scope, Actor: "request-actor", Teammate: "default-reviewer", Version: "test"})
			before, err := env.DB.MaxEventID(t.Context())
			require.NoError(t, err)
			for _, kind := range []string{"reply", "confirm", "refute", "supersede"} {
				args := map[string]any{"ref": issue.ShortID, "reply_to": root.UID, "kind": kind, "body": "Checked the isolated fixture and recorded the observed result.", "force": true, "idempotency_key": "authority-" + kind}
				if readonly {
					require.Contains(t, typedCommentError(t, session, args), "insecure-readonly")
					continue
				}
				out := callWorkflowTool(t, session, "kata.comment", args)
				comment := out["comment"].(map[string]any)
				require.Equal(t, "bound-actor", comment["author"])
				require.Equal(t, "default-reviewer", comment["teammate"])
				require.Equal(t, "bound-actor", out["event"].(map[string]any)["actor"])
				args["force"], args["idempotency_key"] = false, "bound-duplicate-"+kind
				require.Contains(t, typedCommentError(t, session, args), "duplicate_reply")
				args["teammate"] = ""
				suppressed := callWorkflowTool(t, session, "kata.comment", args)["comment"].(map[string]any)
				require.Equal(t, "bound-actor", suppressed["author"])
				require.NotContains(t, suppressed, "teammate")
			}
			if readonly {
				after, err := env.DB.MaxEventID(t.Context())
				require.NoError(t, err)
				require.Equal(t, before, after)
			}
		})
	}
}
