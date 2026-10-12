package dbtest

import (
	"context"
	"database/sql"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"testing"
	"time"
)

// Ordinary externally mapped owners keep the pre-move purge behavior. Nullable
// pointers are reserved for actual moved routing facts, not orphan normal maps.
func RunOrdinaryMappedProjectPurge(t *testing.T, store db.Storage) {
	t.Helper()
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "mapped-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Mapped issue", Author: "tester"})
	require.NoError(t, err)
	peer, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Peer", Author: "tester"})
	require.NoError(t, err)
	comment, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "tester", Body: "Imported comment"})
	require.NoError(t, err)
	link, err := store.CreateLink(ctx, db.CreateLinkParams{FromIssueID: issue.ID, ToIssueID: peer.ID, Type: "blocks", Author: "tester"})
	require.NoError(t, err)
	label := "imported"
	_, _, err = store.AddLabelAndEvent(ctx, issue.ID, db.LabelEventParams{EventType: "issue.labeled", Label: label, Actor: "tester"})
	require.NoError(t, err)
	for _, mapping := range []db.ImportMappingParams{
		{ObjectType: "issue", ExternalID: "issue-1", IssueID: &issue.ID},
		{ObjectType: "comment", ExternalID: "comment-1", IssueID: &issue.ID, CommentID: &comment.ID},
		{ObjectType: "link", ExternalID: "link-1", IssueID: &issue.ID, LinkID: &link.ID},
		{ObjectType: "label", ExternalID: "label-1", IssueID: &issue.ID, Label: &label},
	} {
		mapping.ProjectID = project.ID
		mapping.Source = "example-provider"
		_, err = store.UpsertImportMapping(ctx, mapping)
		require.NoError(t, err)
	}
	_, _, err = store.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: project.ID, Actor: "tester", Force: true})
	require.NoError(t, err)
	_, err = store.PurgeProject(ctx, db.PurgeProjectParams{ProjectID: project.ID, Actor: "tester"})
	require.NoError(t, err)
	mappings, err := store.ImportMappingsByProjectSource(ctx, project.ID, "example-provider")
	require.NoError(t, err)
	require.Empty(t, mappings)
	_, err = store.ProjectByID(ctx, project.ID)
	require.ErrorIs(t, err, db.ErrNotFound)
}

func RunMappedLinkDeletion(t *testing.T, store db.Storage) {
	t.Helper()
	for _, flow := range []string{"direct", "event", "remove-parent", "replace-parent", "remove-blocks", "remove-blocked-by", "remove-related", "conditional-parent"} {
		for _, moved := range []bool{false, true} {
			variant := "ordinary"
			if moved {
				variant = "moved"
			}
			t.Run(flow+"/"+variant, func(t *testing.T) {
				ctx := t.Context()
				project, err := store.CreateProject(ctx, "links-"+flow+"-"+variant)
				require.NoError(t, err)
				issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Mapped endpoint", Author: "tester"})
				require.NoError(t, err)
				peer, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Peer", Author: "tester"})
				require.NoError(t, err)
				kind := "blocks"
				fromID, toID := issue.ID, peer.ID
				if flow == "remove-parent" || flow == "replace-parent" || flow == "conditional-parent" {
					kind = "parent"
				}
				if flow == "remove-related" {
					kind = "related"
				}
				if flow == "remove-blocked-by" {
					fromID, toID = peer.ID, issue.ID
				}
				link, err := store.CreateLink(ctx, db.CreateLinkParams{FromIssueID: fromID, ToIssueID: toID, Type: kind, Author: "tester"})
				require.NoError(t, err)
				mapping, err := store.UpsertImportMapping(ctx, db.ImportMappingParams{ProjectID: project.ID, Source: "example-provider", ExternalID: "edge-1", ObjectType: "link", IssueID: &issue.ID, LinkID: &link.ID})
				require.NoError(t, err)
				q := store.(interface {
					ExecContext(context.Context, string, ...any) (sql.Result, error)
					QueryRowContext(context.Context, string, ...any) *sql.Row
				})
				if moved {
					_, err = q.ExecContext(ctx, `UPDATE import_mappings SET moved_issue_uid=$1,moved_to_authority_uid=$2,moved_to_project_uid=$3 WHERE id=$4`, issue.UID, store.InstanceUID(), project.UID, mapping.ID)
					require.NoError(t, err)
				}
				edit := db.EditIssueAtomicParams{IssueID: issue.ID, Actor: "tester"}
				switch flow {
				case "direct":
					err = store.DeleteLinkByID(ctx, link.ID)
				case "event":
					_, err = store.DeleteLinkAndEvent(ctx, link, db.LinkEventParams{EventIssueID: issue.ID, EventType: "issue.links_changed", Actor: "tester", FromShortID: issue.ShortID, FromUID: issue.UID, ToShortID: peer.ShortID, ToUID: peer.UID})
				case "remove-parent":
					edit.RemoveParent = &peer.ID
					_, err = store.EditIssueAtomic(ctx, edit)
				case "replace-parent":
					replacement, _, createErr := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Replacement parent", Author: "tester"})
					require.NoError(t, createErr)
					edit.SetParent = &replacement.ID
					_, err = store.EditIssueAtomic(ctx, edit)
				case "conditional-parent":
					replacement, _, createErr := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Replacement parent", Author: "tester"})
					require.NoError(t, createErr)
					_, err = store.(db.ParentLinkReplacementStorage).ReplaceParentAndEvents(ctx, db.ReplaceParentAndEventsParams{
						ExpectedParentLinkID: link.ID, ExpectedParentIssueID: peer.ID,
						Link:        db.CreateLinkParams{FromIssueID: issue.ID, ToIssueID: replacement.ID, Type: "parent", Author: "tester"},
						UnlinkEvent: db.LinkEventParams{EventIssueID: issue.ID, EventType: "issue.unlinked", Actor: "tester"},
						LinkEvent:   db.LinkEventParams{EventIssueID: issue.ID, EventType: "issue.linked", Actor: "tester"},
					})
				case "remove-blocks":
					edit.RemoveBlocks = []int64{peer.ID}
					_, err = store.EditIssueAtomic(ctx, edit)
				case "remove-blocked-by":
					edit.RemoveBlockedBy = []int64{peer.ID}
					_, err = store.EditIssueAtomic(ctx, edit)
				case "remove-related":
					edit.RemoveRelated = []int64{peer.ID}
					_, err = store.EditIssueAtomic(ctx, edit)
				}
				require.NoError(t, err)
				var remaining int
				require.NoError(t, q.QueryRowContext(ctx, `SELECT COUNT(*) FROM links WHERE id=$1`, link.ID).Scan(&remaining))
				require.Zero(t, remaining)
				maps, err := store.ImportMappingsByProjectSource(ctx, project.ID, "example-provider")
				require.NoError(t, err)
				if moved {
					require.Len(t, maps, 1)
					require.Nil(t, maps[0].LinkID, "moved suppression survives without a fake edge")
					var movedUID string
					require.NoError(t, q.QueryRowContext(ctx, `SELECT moved_issue_uid FROM import_mappings WHERE id=$1`, mapping.ID).Scan(&movedUID))
					require.Equal(t, issue.UID, movedUID)
				} else {
					require.Empty(t, maps)
				}
			})
		}
	}
}

func RunMappedImportLinkRemoval(t *testing.T, store db.Storage) {
	t.Helper()
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "imported-links")
	require.NoError(t, err)
	first := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	params := db.ImportBatchParams{ProjectID: project.ID, Source: "example-provider", Actor: "importer", Items: []db.ImportItem{
		{ExternalID: "source-1", Title: "Source", Author: "tester", Status: "open", CreatedAt: first, UpdatedAt: first, Links: []db.ImportLink{{Type: "blocks", TargetExternalID: "target-1"}}},
		{ExternalID: "target-1", Title: "Target", Author: "tester", Status: "open", CreatedAt: first, UpdatedAt: first},
	}}
	_, _, err = store.ImportBatch(ctx, params)
	require.NoError(t, err)
	mapping, err := store.ImportMappingBySource(ctx, project.ID, params.Source, "link", "source-1:blocks:target-1")
	require.NoError(t, err)
	require.NotNil(t, mapping.LinkID)
	removedID := *mapping.LinkID
	params.Items[0].Links = nil
	params.Items[0].UpdatedAt = first.Add(time.Hour)
	_, _, err = store.ImportBatch(ctx, params)
	require.NoError(t, err)
	_, err = store.LinkByID(ctx, removedID)
	require.ErrorIs(t, err, db.ErrNotFound)
	_, err = store.ImportMappingBySource(ctx, project.ID, params.Source, "link", "source-1:blocks:target-1")
	require.ErrorIs(t, err, db.ErrNotFound)
}

func RunMappedFederationBoundaryCleanup(t *testing.T, store db.Storage) {
	t.Helper()
	for _, transition := range []string{"different-origins", "leave"} {
		for _, moved := range []bool{false, true} {
			variant := "ordinary"
			if moved {
				variant = "moved"
			}
			t.Run(transition+"/"+variant, func(t *testing.T) {
				ctx := t.Context()
				source, err := store.CreateProject(ctx, "boundary-source-"+transition+"-"+variant)
				require.NoError(t, err)
				target, err := store.CreateProject(ctx, "boundary-target-"+transition+"-"+variant)
				require.NoError(t, err)
				issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "Source", Author: "tester"})
				require.NoError(t, err)
				peer, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: target.ID, Title: "Target", Author: "tester"})
				require.NoError(t, err)
				link, _, err := store.CreateLinkAndEvent(ctx, db.CreateLinkParams{FromIssueID: issue.ID, ToIssueID: peer.ID, Type: "blocks", Author: "tester"}, db.LinkEventParams{EventIssueID: issue.ID, EventType: "issue.linked", Actor: "tester", FromUID: issue.UID, ToUID: peer.UID, FromShortID: issue.ShortID, ToShortID: peer.ShortID})
				require.NoError(t, err)
				mapping, err := store.UpsertImportMapping(ctx, db.ImportMappingParams{ProjectID: source.ID, Source: "example-provider", ExternalID: "boundary-1", ObjectType: "link", IssueID: &issue.ID, LinkID: &link.ID})
				require.NoError(t, err)
				if moved {
					q := store.(interface {
						ExecContext(context.Context, string, ...any) (sql.Result, error)
					})
					_, err = q.ExecContext(ctx, `UPDATE import_mappings SET moved_issue_uid=$1,moved_to_authority_uid=$2,moved_to_project_uid=$3 WHERE id=$4`, issue.UID, store.InstanceUID(), target.UID, mapping.ID)
					require.NoError(t, err)
				}
				_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: source.ID, Role: db.FederationRoleSpoke, HubURL: "https://first-hub.example", HubProjectID: 41, HubProjectUID: source.UID, Actor: "member", Enabled: true})
				require.NoError(t, err)
				_, err = store.LinkByID(ctx, link.ID)
				require.NoError(t, err, "standalone boundary still follows the existing enrollment contract")
				peerOrigin := "https://second-hub.example"
				if transition == "leave" {
					peerOrigin = "https://first-hub.example"
				}
				_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: target.ID, Role: db.FederationRoleSpoke, HubURL: peerOrigin, HubProjectID: 42, HubProjectUID: target.UID, Actor: "member", Enabled: true})
				require.NoError(t, err)
				if transition == "leave" {
					_, err = store.LinkByID(ctx, link.ID)
					require.NoError(t, err, "compatible group keeps the actual link before leaving")
					_, err = store.LeaveFederationReplica(ctx, source.ID)
					require.NoError(t, err)
				}
				_, err = store.LinkByID(ctx, link.ID)
				require.ErrorIs(t, err, db.ErrNotFound)
				mappings, err := store.ImportMappingsByProjectSource(ctx, source.ID, "example-provider")
				require.NoError(t, err)
				if moved {
					require.Len(t, mappings, 1)
					require.Nil(t, mappings[0].LinkID)
				} else {
					require.Empty(t, mappings)
				}
			})
		}
	}
}

func RunMappedFederationReset(t *testing.T, store db.Storage) {
	t.Helper()
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "reset-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Imported owner", Author: "tester"})
	require.NoError(t, err)
	comment, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "tester", Body: "Imported comment"})
	require.NoError(t, err)
	for _, mapping := range []db.ImportMappingParams{
		{ObjectType: "issue", ExternalID: "ordinary-issue", IssueID: &issue.ID},
		{ObjectType: "comment", ExternalID: "ordinary-comment", IssueID: &issue.ID, CommentID: &comment.ID},
		{ObjectType: "comment", ExternalID: "moved-comment", IssueID: &issue.ID, CommentID: &comment.ID},
	} {
		mapping.ProjectID = project.ID
		mapping.Source = "example-provider"
		stored, mapErr := store.UpsertImportMapping(ctx, mapping)
		require.NoError(t, mapErr)
		if mapping.ExternalID == "moved-comment" {
			q := store.(interface {
				ExecContext(context.Context, string, ...any) (sql.Result, error)
			})
			_, err = q.ExecContext(ctx, `UPDATE import_mappings SET moved_issue_uid=$1,moved_to_authority_uid=$2,moved_to_project_uid=$3 WHERE id=$4`, issue.UID, store.InstanceUID(), project.UID, stored.ID)
			require.NoError(t, err)
		}
	}
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 42, HubProjectUID: project.UID, Actor: "member", Enabled: true})
	require.NoError(t, err)
	require.NoError(t, store.ResetFederatedProject(ctx, project.ID, 20, 19))
	_, err = store.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
	require.ErrorIs(t, err, db.ErrNotFound)
	mappings, err := store.ImportMappingsByProjectSource(ctx, project.ID, "example-provider")
	require.NoError(t, err)
	require.Len(t, mappings, 1)
	require.Equal(t, "moved-comment", mappings[0].ExternalID)
	require.Nil(t, mappings[0].IssueID)
	require.Nil(t, mappings[0].CommentID)
}

// Re-folding authoritative history removes cached rows not represented by it;
// ordinary imports follow deleted owners, moved source identities remain.
func RunMappedProjectionReconcile(t *testing.T, store db.Storage) {
	t.Helper()
	for _, kind := range []string{"comment", "issue"} {
		for _, moved := range []bool{false, true} {
			variant := "ordinary"
			if moved {
				variant = "moved"
			}
			t.Run(kind+"/"+variant, func(t *testing.T) {
				ctx := t.Context()
				project, err := store.CreateProject(ctx, "reconcile-"+kind+"-"+variant)
				require.NoError(t, err)
				_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
				require.NoError(t, err)
				issue, issueEvent, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Cached row", Author: "tester"})
				require.NoError(t, err)
				mapping := db.ImportMappingParams{ProjectID: project.ID, Source: "example-provider", ExternalID: "cached-owner", ObjectType: kind, IssueID: &issue.ID}
				removedEventUID := issueEvent.UID
				if kind == "comment" {
					comment, event, commentErr := store.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "tester", Body: "Cached comment"})
					require.NoError(t, commentErr)
					mapping.CommentID = &comment.ID
					removedEventUID = event.UID
				}
				stored, err := store.UpsertImportMapping(ctx, mapping)
				require.NoError(t, err)
				q := store.(interface {
					ExecContext(context.Context, string, ...any) (sql.Result, error)
				})
				if moved {
					_, err = q.ExecContext(ctx, `UPDATE import_mappings SET moved_issue_uid=$1,moved_to_authority_uid=$2,moved_to_project_uid=$3 WHERE id=$4`, issue.UID, store.InstanceUID(), project.UID, stored.ID)
					require.NoError(t, err)
				}
				_, err = q.ExecContext(ctx, `DELETE FROM events WHERE uid=$1`, removedEventUID)
				require.NoError(t, err)
				require.NoError(t, store.MaterializeFederatedProject(ctx, project.ID))
				mappings, err := store.ImportMappingsByProjectSource(ctx, project.ID, "example-provider")
				require.NoError(t, err)
				if moved {
					require.Len(t, mappings, 1)
					if kind == "comment" {
						require.Nil(t, mappings[0].CommentID)
					} else {
						require.Nil(t, mappings[0].IssueID)
					}
				} else {
					require.Empty(t, mappings)
				}
			})
		}
	}
}
