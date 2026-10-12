package main

import (
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"testing"
)

func TestMoveCLIWholeTreeIncludesCompletedChildren(t *testing.T) {
	env, dir, source, target, root := setupMoveCLIProjects(t)
	child, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.ID, Title: "Completed child", Author: "tester"})
	require.NoError(t, err)
	_, err = env.DB.CreateLink(t.Context(), db.CreateLinkParams{FromIssueID: child.ID, ToIssueID: root.ID, Type: "parent", Author: "tester"})
	require.NoError(t, err)
	_, _, _, err = env.DB.CloseIssue(t.Context(), child.ID, "done", "tester", "", nil)
	require.NoError(t, err)
	runCLI(t, env, dir, "--project", source.Name, "move", root.ShortID, target.Name)
	stored, err := env.DB.IssueByUID(t.Context(), child.UID, db.IncludeDeletedYes)
	require.NoError(t, err)
	require.Equal(t, target.ID, stored.ProjectID, "default CLI move transfers the completed child too")
}

func TestMoveCLIOverlappingBatchTransfersOnce(t *testing.T) {
	env, dir, source, target, root := setupMoveCLIProjects(t)
	child, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.ID, Title: "Child", Author: "tester"})
	require.NoError(t, err)
	_, err = env.DB.CreateLink(t.Context(), db.CreateLinkParams{FromIssueID: child.ID, ToIssueID: root.ID, Type: "parent", Author: "tester"})
	require.NoError(t, err)
	runCLI(t, env, dir, "--project", source.Name, "move", root.ShortID, child.ShortID, target.Name)
	for _, issue := range []db.Issue{root, child} {
		moved, err := env.DB.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedYes)
		require.NoError(t, err)
		require.Equal(t, target.ID, moved.ProjectID)
	}
	events, err := env.DB.EventsAfter(t.Context(), db.EventsAfterParams{Limit: 100})
	require.NoError(t, err)
	count := 0
	for _, event := range events {
		if event.Type == "issue.moved" {
			count++
		}
	}
	require.Equal(t, 2, count, "overlapping parent and child are moved only once")
}
