package dbtest

import (
	"bytes"
	"context"
	"database/sql"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
	"testing"
)

// The approved scoped-transfer contract preserves selected original history
// and excludes unrelated issue bodies, project metadata and credentials.
func RunIssueTreeSnapshotScope(t *testing.T, store db.Storage) {
	ctx := t.Context()
	source, err := store.CreateProject(ctx, "source-project")
	require.NoError(t, err)
	root, created, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "Root", Author: "original-author"})
	require.NoError(t, err)
	unrelated, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "Unrelated", Body: "unrelated-private-body", Author: "other-author"})
	require.NoError(t, err)
	child, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "Deleted child", Author: "original-author"})
	require.NoError(t, err)
	_, err = store.CreateLink(ctx, db.CreateLinkParams{FromIssueID: child.ID, ToIssueID: root.ID, Type: "parent", Author: "original-author"})
	require.NoError(t, err)
	_, _, _, err = store.SoftDeleteIssue(ctx, child.ID, "operator")
	require.NoError(t, err)
	requestUID, err := uid.New()
	require.NoError(t, err)
	destinationUID, err := uid.New()
	require.NoError(t, err)
	authorityUID, err := uid.New()
	require.NoError(t, err)
	request := db.MoveTreeRequest{SourceAuthorityUID: store.InstanceUID(), SourceProjectUID: source.UID, IssueUID: root.UID, DestinationAuthorityUID: authorityUID, DestinationProjectUID: destinationUID, RequestUID: requestUID, ActorAuthorityUID: store.InstanceUID(), Actor: "operator"}
	snapshot, err := store.ExportIssueTree(ctx, request)
	require.NoError(t, err)
	require.Len(t, snapshot.Portions, 1)
	require.Len(t, snapshot.Portions[0].Issues, 2)
	require.NotEmpty(t, snapshot.Digest)
	wire, err := db.EncodeIssueTreeSnapshot(snapshot)
	require.NoError(t, err)
	require.False(t, bytes.Contains(wire, []byte(unrelated.UID)))
	require.False(t, bytes.Contains(wire, []byte("unrelated-private-body")))
	var retained *db.EventExport
	for i := range snapshot.Portions[0].Events {
		if snapshot.Portions[0].Events[i].UID == created.UID {
			retained = &snapshot.Portions[0].Events[i]
		}
	}
	require.NotNil(t, retained)
	require.Equal(t, created.ContentHash, retained.ContentHash)
	require.NotEmpty(t, retained.SourceEventBytes)
	decoded, err := db.DecodeIssueTreeSnapshot(bytes.NewReader(wire))
	require.NoError(t, err)
	require.Equal(t, snapshot.Digest, decoded.Digest)
	limited := db.WithAuthorizedProjects(ctx, []string{})
	denied, err := store.ExportIssueTree(limited, request)
	require.Error(t, err)
	require.Empty(t, denied.Portions)
}

// A retained series occurrence can be elsewhere even without a parent edge.
// Its UID row is the route owner; the full series must require that authority.
func RunIssueTreeMovedSeriesRoute(t *testing.T, store db.Storage) {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "series-project")
	require.NoError(t, err)
	root, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "First occurrence", Author: "author"})
	require.NoError(t, err)
	attached, err := store.CreateRecurrenceForIssue(ctx, db.CreateRecurrenceForIssueIn{IssueID: root.ID, Recurrence: db.CreateRecurrenceIn{ProjectID: project.ID, Actor: "author", Rule: "FREQ=DAILY", DTStart: "2026-10-12", Timezone: "UTC", Template: db.RecurrenceTemplate{Title: "Daily task"}}})
	require.NoError(t, err)
	next, err := store.MaterializeNext(ctx, attached.Recurrence.ID, "2026-10-12", "scheduler")
	require.NoError(t, err)
	ghost, err := store.IssueByUID(ctx, next.NewIssueUID, db.IncludeDeletedYes)
	require.NoError(t, err)
	requestUID, err := uid.New()
	require.NoError(t, err)
	remoteAuthority, err := uid.New()
	require.NoError(t, err)
	remoteProject, err := uid.New()
	require.NoError(t, err)
	q := store.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	})
	_, err = q.ExecContext(ctx, `UPDATE issues SET moved_to_authority_uid=$1,moved_to_project_uid=$2,moved_to_short_id=$3,moved_event_uid=$4 WHERE uid=$5`, remoteAuthority, remoteProject, ghost.ShortID, attached.Events[0].UID, ghost.UID)
	require.NoError(t, err)
	request := db.MoveTreeRequest{SourceAuthorityUID: store.InstanceUID(), SourceProjectUID: project.UID, IssueUID: root.UID, DestinationAuthorityUID: remoteAuthority, DestinationProjectUID: remoteProject, RequestUID: requestUID, ActorAuthorityUID: store.InstanceUID(), Actor: "operator", MoveSeries: true}
	snapshot, err := store.ExportIssueTree(ctx, request)
	require.NoError(t, err)
	require.Len(t, snapshot.Required, 1)
	require.Equal(t, ghost.UID, snapshot.Required[0].IssueUID)
	require.Equal(t, remoteAuthority, snapshot.Required[0].AuthorityUID)
	require.Equal(t, remoteProject, snapshot.Required[0].ProjectUID)
	require.Len(t, snapshot.Portions[0].Issues, 1, "ghost body stays out of the local portion")
}

func RunIssueTreeMovedChildRequirement(t *testing.T, store db.Storage) {
	ctx := t.Context()
	source, err := store.CreateProject(ctx, "source-project")
	require.NoError(t, err)
	target, err := store.CreateProject(ctx, "target-project")
	require.NoError(t, err)
	root, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "Root", Author: "author"})
	require.NoError(t, err)
	child, event, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "Moved child", Author: "author"})
	require.NoError(t, err)
	_, err = store.CreateLink(ctx, db.CreateLinkParams{FromIssueID: child.ID, ToIssueID: root.ID, Type: "parent", Author: "author"})
	require.NoError(t, err)
	remoteAuthority, err := uid.New()
	require.NoError(t, err)
	remoteProject, err := uid.New()
	require.NoError(t, err)
	requestUID, err := uid.New()
	require.NoError(t, err)
	q := store.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	})
	_, err = q.ExecContext(ctx, `UPDATE issues SET moved_to_authority_uid=$1,moved_to_project_uid=$2,moved_to_short_id=$3,moved_event_uid=$4 WHERE uid=$5`, remoteAuthority, remoteProject, child.ShortID, event.UID, child.UID)
	require.NoError(t, err)
	request := db.MoveTreeRequest{SourceAuthorityUID: store.InstanceUID(), SourceProjectUID: source.UID, IssueUID: root.UID, DestinationAuthorityUID: store.InstanceUID(), DestinationProjectUID: target.UID, RequestUID: requestUID, ActorAuthorityUID: store.InstanceUID(), Actor: "operator"}
	result, err := store.MoveIssueTree(ctx, request)
	var remote *db.MoveRemoteSelectionError
	require.ErrorAs(t, err, &remote)
	require.Len(t, remote.Required, 1)
	require.Equal(t, child.UID, remote.Required[0].IssueUID)
	require.Equal(t, remoteAuthority, remote.Required[0].AuthorityUID)
	require.Equal(t, "conflict", result.State)
	require.Empty(t, result.Applied)
	retained, err := store.IssueByUID(ctx, root.UID, db.IncludeDeletedYes)
	require.NoError(t, err)
	require.Equal(t, source.ID, retained.ProjectID)
}
