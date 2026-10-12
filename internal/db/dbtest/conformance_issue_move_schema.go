package dbtest

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// RunIssueMoveRoutingSchema exercises the approved routing constraints on
// disposable native stores. A partial route must never become retained state.
func RunIssueMoveRoutingSchema(t *testing.T, store db.Storage) {
	t.Helper()
	ctx := t.Context()
	sqlStore := store.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
		QueryRowContext(context.Context, string, ...any) *sql.Row
	})
	project, err := store.CreateProject(ctx, "routing-project")
	require.NoError(t, err)
	target, err := store.CreateProject(ctx, "destination-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Retained task", Author: "tester"})
	require.NoError(t, err)
	moved, err := store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: project.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "tester"})
	require.NoError(t, err)
	require.NotNil(t, moved.Event)
	var generation int64
	var authority, destination, shortRef, eventUID *string
	require.NoError(t, sqlStore.QueryRowContext(ctx, `SELECT location_generation,moved_to_authority_uid,moved_to_project_uid,moved_to_short_id,moved_event_uid FROM issues WHERE id=$1`, issue.ID).Scan(&generation, &authority, &destination, &shortRef, &eventUID))
	require.Equal(t, int64(2), generation, "same-store relocation advances the location generation")
	require.Nil(t, authority)
	require.Nil(t, destination)
	require.Nil(t, shortRef)
	require.Nil(t, eventUID)
	_, err = sqlStore.ExecContext(ctx, `UPDATE issues SET moved_to_authority_uid=$1 WHERE id=$2`, "01AAAAAAAAAAAAAAAAAAAAAAAA", issue.ID)
	require.Error(t, err, "incomplete routing coordinates must be rejected")
	_, err = sqlStore.ExecContext(ctx, `UPDATE issues SET location_generation=0 WHERE id=$1`, issue.ID)
	require.Error(t, err, "location generations must be positive")
	_, err = sqlStore.ExecContext(ctx, `UPDATE issues SET moved_to_authority_uid=$1,moved_to_project_uid=$2,moved_to_short_id=$3,moved_event_uid=$4 WHERE id=$5`, "01AAAAAAAAAAAAAAAAAAAAAAAA", target.UID, issue.ShortID, "01BBBBBBBBBBBBBBBBBBBBBBBB", issue.ID)
	require.Error(t, err, "a route must retain a real audit event")
	_, err = sqlStore.ExecContext(ctx, `UPDATE issues SET moved_to_authority_uid=$1,moved_to_project_uid=$2,moved_to_short_id=$3,moved_event_uid=$4 WHERE id=$5`, "01AAAAAAAAAAAAAAAAAAAAAAAA", target.UID, issue.ShortID, moved.Event.UID, issue.ID)
	require.NoError(t, err)
	_, err = sqlStore.ExecContext(ctx, `DELETE FROM events WHERE uid=$1`, moved.Event.UID)
	require.Error(t, err, "routing audit evidence must survive event deletion")
}

// RunIssueMoveOwnerSchema checks the existing owners used by zero-table moves.
// These are persistence contracts, not substitutes for transfer acceptance.
func RunIssueMoveOwnerSchema(t *testing.T, store db.Storage) {
	t.Helper()
	ctx := t.Context()
	sqlStore := store.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
		QueryRowContext(context.Context, string, ...any) *sql.Row
	})
	project, err := store.CreateProject(ctx, "owner-project")
	require.NoError(t, err)
	issue, event, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Task", Author: "tester"})
	require.NoError(t, err)
	t.Run("orphan history retains original identity and bytes", func(t *testing.T) {
		original := []byte(`{"source":"unchanged original envelope"}`)
		_, err := sqlStore.ExecContext(ctx, `UPDATE events SET source_project_uid=$1,source_event_bytes=$2,project_id=NULL WHERE uid=$3`, project.UID, original, event.UID)
		require.NoError(t, err)
		var projectID *int64
		var source string
		var got []byte
		require.NoError(t, sqlStore.QueryRowContext(ctx, `SELECT project_id,source_project_uid,source_event_bytes FROM events WHERE uid=$1`, event.UID).Scan(&projectID, &source, &got))
		require.Nil(t, projectID)
		require.Equal(t, project.UID, source)
		require.Equal(t, original, got)
	})
	t.Run("retained names reserve their original authority", func(t *testing.T) {
		_, err := sqlStore.ExecContext(ctx, `INSERT INTO project_aliases(project_id,alias_identity,alias_kind,authority_uid,project_uid) VALUES(NULL,$1,'name',$2,$3)`, "retained-project", store.InstanceUID(), project.UID)
		require.NoError(t, err)
		_, err = sqlStore.ExecContext(ctx, `INSERT INTO project_aliases(project_id,alias_identity,alias_kind,authority_uid,project_uid) VALUES(NULL,$1,'name',$2,$3)`, "retained-project", store.InstanceUID(), "01AAAAAAAAAAAAAAAAAAAAAAAA")
		require.Error(t, err)
		_, err = sqlStore.ExecContext(ctx, `INSERT INTO project_aliases(project_id,alias_identity,alias_kind,authority_uid,project_uid) VALUES(NULL,$1,'name',$2,$3)`, "retained-project", "01BBBBBBBBBBBBBBBBBBBBBBBB", project.UID)
		require.NoError(t, err)
		_, err = sqlStore.ExecContext(ctx, `INSERT INTO project_aliases(project_id,alias_identity,alias_kind) VALUES(NULL,'workspace-alias','local')`)
		require.Error(t, err, "workspace aliases still require a real local project")
	})
	t.Run("reply edges keep distinct comments in the same issue", func(t *testing.T) {
		var comments []db.Comment
		for _, body := range []string{"First", "Second", "Third"} {
			c, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "tester", Body: body})
			require.NoError(t, err)
			comments = append(comments, c)
		}
		for _, target := range comments[1:] {
			_, err := sqlStore.ExecContext(ctx, `INSERT INTO links(from_issue_id,to_issue_id,from_issue_uid,to_issue_uid,type,author,from_comment_uid,to_comment_uid,reply_kind) VALUES($1,$1,$2,$2,'reply','tester',$3,$4,'reply')`, issue.ID, issue.UID, comments[0].UID, target.UID)
			require.NoError(t, err)
		}
		_, err := sqlStore.ExecContext(ctx, `INSERT INTO links(from_issue_id,to_issue_id,from_issue_uid,to_issue_uid,type,author,from_comment_uid,to_comment_uid,reply_kind) VALUES($1,$1,$2,$2,'reply','tester',$3,$4,'reply')`, issue.ID, issue.UID, comments[0].UID, comments[1].UID)
		require.Error(t, err, "the logical comment edge cannot duplicate")
	})
	t.Run("remote issue endpoints do not require fake local bodies", func(t *testing.T) {
		_, err := sqlStore.ExecContext(ctx, `INSERT INTO links(from_issue_id,to_issue_id,from_issue_uid,to_issue_uid,type,author,from_authority_uid,from_project_uid,to_authority_uid,to_project_uid) VALUES($1,NULL,$2,$3,'related','tester',$4,$5,$6,$7)`, issue.ID, issue.UID, "01AAAAAAAAAAAAAAAAAAAAAAAA", store.InstanceUID(), project.UID, "01BBBBBBBBBBBBBBBBBBBBBBBB", "01CCCCCCCCCCCCCCCCCCCCCCCC")
		require.NoError(t, err)
		_, err = sqlStore.ExecContext(ctx, `INSERT INTO links(from_issue_id,to_issue_id,from_issue_uid,to_issue_uid,type,author) VALUES($1,NULL,$2,$3,'blocks','tester')`, issue.ID, "01DDDDDDDDDDDDDDDDDDDDDDDD", issue.UID)
		require.Error(t, err, "a known local ID must still match its UID")
	})

	t.Run("remote recurrence identity reserves issued occurrence keys", func(t *testing.T) {
		peer, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Another occurrence", Author: "tester"})
		require.NoError(t, err)
		for i, id := range []int64{issue.ID, peer.ID} {
			_, err = sqlStore.ExecContext(ctx, `UPDATE issues SET recurrence_uid=$1,recurrence_authority_uid=$2,recurrence_project_uid=$3,owner_authority_uid=$4,occurrence_key=$5 WHERE id=$6`, "01EEEEEEEEEEEEEEEEEEEEEEEE", "01BBBBBBBBBBBBBBBBBBBBBBBB", "01CCCCCCCCCCCCCCCCCCCCCCCC", store.InstanceUID(), "2026-10-12", id)
			if i == 0 {
				require.NoError(t, err)
			} else {
				require.Error(t, err, "logical series keys are unique even without a local recurrence row")
			}
		}
	})
	t.Run("recurrence routing coordinates commit together", func(t *testing.T) {
		var id int64
		err := sqlStore.QueryRowContext(ctx, `INSERT INTO recurrences(uid,project_id,rrule,dtstart,timezone,template_title,author) VALUES($1,$2,'FREQ=DAILY','2026-10-12T00:00:00Z','UTC','Recurring task','tester') RETURNING id`, "01EEEEEEEEEEEEEEEEEEEEEEEE", project.ID).Scan(&id)
		require.NoError(t, err)
		_, err = sqlStore.ExecContext(ctx, `UPDATE recurrences SET moved_to_authority_uid=$1 WHERE id=$2`, store.InstanceUID(), id)
		require.Error(t, err)
		_, err = sqlStore.ExecContext(ctx, `UPDATE recurrences SET moved_to_authority_uid=$1,moved_to_project_uid=$2,moved_event_uid=$3 WHERE id=$4`, store.InstanceUID(), project.UID, event.UID, id)
		require.NoError(t, err)
	})

	t.Run("purged series keys remain unique tombstones", func(t *testing.T) {
		for i, auditUID := range []string{"01KKKKKKKKKKKKKKKKKKKKKKKK", "01MMMMMMMMMMMMMMMMMMMMMMMM"} {
			_, err := sqlStore.ExecContext(ctx, `INSERT INTO purge_log(uid,origin_instance_uid,project_id,purged_issue_id,project_name,issue_title,issue_author,comment_count,link_count,label_count,event_count,actor,recurrence_uid,occurrence_key) VALUES($1,$2,$3,999,'owner-project','Purged occurrence','tester',0,0,0,0,'tester',$4,'2026-10-12')`, auditUID, store.InstanceUID(), project.ID, "01EEEEEEEEEEEEEEEEEEEEEEEE")
			if i == 0 {
				require.NoError(t, err)
			} else {
				require.Error(t, err, "a purged occurrence key cannot be reserved twice")
			}
		}
	})
	t.Run("foreign creation proofs survive local owner removal", func(t *testing.T) {
		projectUID := "01FFFFFFFFFFFFFFFFFFFFFFFF"
		keyID := strings.Repeat("a", 64)
		_, err := sqlStore.ExecContext(ctx, `INSERT INTO federation_root_keys(project_uid,authority_uid,key_id,public_key) VALUES($1,$2,$3,$4)`, projectUID, store.InstanceUID(), keyID, "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
		require.NoError(t, err, "retained foreign proofs cannot require a fabricated local project")
		_, err = sqlStore.ExecContext(ctx, `INSERT INTO federation_event_provenance(project_uid,event_uid,content_hash,reset_epoch,sequence,key_id,receipt) VALUES($1,$2,$3,1,1,$4,'{}')`, projectUID, event.UID, event.ContentHash, keyID)
		require.NoError(t, err)
		_, err = sqlStore.ExecContext(ctx, `INSERT INTO federation_entity_provenance(project_uid,kind,entity_uid,event_uid) VALUES($1,'issue',$2,$3)`, projectUID, issue.UID, event.UID)
		require.NoError(t, err)
		_, err = sqlStore.ExecContext(ctx, `DELETE FROM federation_root_keys WHERE project_uid=$1`, projectUID)
		require.Error(t, err, "removing a key must not cascade away live creation evidence")
		_, err = sqlStore.ExecContext(ctx, `DELETE FROM federation_event_provenance WHERE project_uid=$1`, projectUID)
		require.Error(t, err, "removing an event proof must not cascade away its creation reference")
	})
	t.Run("move retry facts are unique per issue and request", func(t *testing.T) {
		for i, eventUID := range []string{"01GGGGGGGGGGGGGGGGGGGGGGGG", "01HHHHHHHHHHHHHHHHHHHHHHHH"} {
			_, err := sqlStore.ExecContext(ctx, `INSERT INTO events(uid,origin_instance_uid,project_id,project_name,issue_id,issue_uid,type,actor,payload,hlc_physical_ms,hlc_counter,content_hash) SELECT $1,origin_instance_uid,$2,project_name,issue_id,issue_uid,'issue.move_received',actor,$3,hlc_physical_ms,hlc_counter,content_hash FROM events WHERE uid=$4`, eventUID, project.ID, `{"request_uid":"01JJJJJJJJJJJJJJJJJJJJJJJJ"}`, event.UID)
			if i == 0 {
				require.NoError(t, err)
			} else {
				require.Error(t, err, "duplicate acceptance facts must not commit")
			}
		}
	})
	t.Run("moved mappings survive comment owner removal", func(t *testing.T) {
		comment, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "tester", Body: "Imported"})
		require.NoError(t, err)
		mapping, err := store.UpsertImportMapping(ctx, db.ImportMappingParams{ProjectID: project.ID, Source: "example-provider", ExternalID: "comment-1", ObjectType: "comment", IssueID: &issue.ID, CommentID: &comment.ID})
		require.NoError(t, err)
		_, err = sqlStore.ExecContext(ctx, `UPDATE import_mappings SET moved_issue_uid=$1,moved_to_authority_uid=$2,moved_to_project_uid=$3 WHERE id=$4`, issue.UID, store.InstanceUID(), project.UID, mapping.ID)
		require.NoError(t, err)
		_, err = sqlStore.ExecContext(ctx, `DELETE FROM comments WHERE id=$1`, comment.ID)
		require.NoError(t, err)
		var owner *int64
		var moved string
		require.NoError(t, sqlStore.QueryRowContext(ctx, `SELECT comment_id,moved_issue_uid FROM import_mappings WHERE id=$1`, mapping.ID).Scan(&owner, &moved))
		require.Nil(t, owner)
		require.Equal(t, issue.UID, moved)
		_, err = sqlStore.ExecContext(ctx, `INSERT INTO import_mappings(source,external_id,object_type,project_id) VALUES('example-provider','missing-owner','comment',$1)`, project.ID)
		require.Error(t, err, "ordinary mappings retain their owner requirements")
	})
}

// RunIssueMoveAuditPurge pins the permanent-reference contract: removing the
// current body must not remove the ordinary event that records its old location.
func RunIssueMoveAuditPurge(t *testing.T, store db.Storage) {
	t.Helper()
	ctx := t.Context()
	source, err := store.CreateProject(ctx, "source-project")
	require.NoError(t, err)
	destination, err := store.CreateProject(ctx, "destination-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "Moved task", Author: "tester"})
	require.NoError(t, err)
	moved, err := store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: destination.ID, IfMatchRev: issue.Revision, Actor: "tester"})
	require.NoError(t, err)
	require.NotNil(t, moved.Event)
	_, err = store.PurgeIssue(ctx, issue.ID, "tester", nil)
	require.NoError(t, err)
	var subjectID *int64
	var subjectUID, contentHash string
	q := store.(interface {
		QueryRowContext(context.Context, string, ...any) *sql.Row
	})
	require.NoError(t, q.QueryRowContext(ctx, `SELECT issue_id,issue_uid,content_hash FROM events WHERE uid=$1`, moved.Event.UID).Scan(&subjectID, &subjectUID, &contentHash))
	require.Nil(t, subjectID)
	require.Equal(t, issue.UID, subjectUID)
	require.Equal(t, moved.Event.ContentHash, contentHash)
}

func RunIssueMoveSourceProjectPurge(t *testing.T, store db.Storage) {
	t.Helper()
	ctx := t.Context()
	source, err := store.CreateProject(ctx, "source-project")
	require.NoError(t, err)
	destination, err := store.CreateProject(ctx, "destination-project")
	require.NoError(t, err)
	issue, original, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "Retained history", Author: "tester"})
	require.NoError(t, err)
	_, err = store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: destination.ID, IfMatchRev: issue.Revision, Actor: "tester"})
	require.NoError(t, err)
	_, _, err = store.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: source.ID, Actor: "tester"})
	require.NoError(t, err)
	_, err = store.PurgeProject(ctx, db.PurgeProjectParams{ProjectID: source.ID, Actor: "tester"})
	require.NoError(t, err)
	q := store.(interface {
		QueryRowContext(context.Context, string, ...any) *sql.Row
	})
	var localProject *int64
	var sourceProjectUID, hash string
	require.NoError(t, q.QueryRowContext(ctx, `SELECT project_id,source_project_uid,content_hash FROM events WHERE uid=$1`, original.UID).Scan(&localProject, &sourceProjectUID, &hash))
	require.Nil(t, localProject)
	require.Equal(t, source.UID, sourceProjectUID)
	require.Equal(t, original.ContentHash, hash)
	var retainedNameProject string
	require.NoError(t, q.QueryRowContext(ctx, `SELECT project_uid FROM project_aliases WHERE alias_kind='name' AND alias_identity=$1 AND authority_uid=$2`, source.Name, store.InstanceUID()).Scan(&retainedNameProject))
	require.Equal(t, source.UID, retainedNameProject)
	current, err := store.IssueByID(ctx, issue.ID)
	require.NoError(t, err)
	require.Equal(t, destination.ID, current.ProjectID)
}

func RunIssueMoveSourceBytesPurge(t *testing.T, store db.Storage) {
	t.Helper()
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "source-project")
	require.NoError(t, err)
	issue, event, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Accepted task", Author: "tester"})
	require.NoError(t, err)
	originalBytes, err := json.Marshal(db.RemoteEventFromStored(event))
	require.NoError(t, err)
	q := store.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
		QueryRowContext(context.Context, string, ...any) *sql.Row
	})
	_, err = q.ExecContext(ctx, `UPDATE events SET source_event_bytes=$1,source_project_uid=$2 WHERE uid=$3`, originalBytes, project.UID, event.UID)
	require.NoError(t, err)
	_, err = store.PurgeIssue(ctx, issue.ID, "tester", nil)
	require.NoError(t, err)
	var subjectID *int64
	var subjectUID, hash string
	var restoredBytes []byte
	require.NoError(t, q.QueryRowContext(ctx, `SELECT issue_id,issue_uid,content_hash,source_event_bytes FROM events WHERE uid=$1`, event.UID).Scan(&subjectID, &subjectUID, &hash, &restoredBytes))
	require.Nil(t, subjectID)
	require.Equal(t, issue.UID, subjectUID)
	require.Equal(t, event.ContentHash, hash)
	require.Equal(t, originalBytes, restoredBytes)
	_, _, err = store.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: project.ID, Actor: "tester"})
	require.NoError(t, err)
	_, err = store.PurgeProject(ctx, db.PurgeProjectParams{ProjectID: project.ID, Actor: "tester"})
	require.NoError(t, err)
	var projectID *int64
	var sourceUID string
	require.NoError(t, q.QueryRowContext(ctx, `SELECT project_id,source_project_uid,source_event_bytes FROM events WHERE uid=$1`, event.UID).Scan(&projectID, &sourceUID, &restoredBytes))
	require.Nil(t, projectID)
	require.Equal(t, project.UID, sourceUID)
	require.Equal(t, originalBytes, restoredBytes)
}

func RunIssueMoveProofRetirement(t *testing.T, store db.Storage) {
	t.Helper()
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "replica-project")
	require.NoError(t, err)
	issue, event, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Signed original task", Author: "tester"})
	require.NoError(t, err)
	public, private, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	pin := db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: "01AAAAAAAAAAAAAAAAAAAAAAAA", KeyID: db.RootPublicKeyID(public), PublicKey: public}
	require.NoError(t, store.PinRootAuthority(ctx, pin))
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 42, HubProjectUID: project.UID, Actor: "member", Enabled: true})
	require.NoError(t, err)
	receipt, err := db.SignRootReceipt(db.AttributionReceipt{Version: 1, ProjectUID: project.UID, AuthorityUID: pin.AuthorityUID, KeyID: pin.KeyID, EventUID: event.UID, ContentHash: event.ContentHash, AccountableActor: "member", SourceActor: event.Actor, IngressInstanceUID: store.InstanceUID(), AcceptedAt: time.Now().UTC(), ResetEpoch: 1, Sequence: 1}, private)
	require.NoError(t, err)
	require.NoError(t, db.VerifyRootReceipt(pin, receipt))
	require.NoError(t, store.ApplyUpstreamAttribution(ctx, pin, receipt))
	_, err = store.LeaveFederationReplica(ctx, project.ID)
	require.NoError(t, err, "teardown must preserve signed creation evidence without stranded state")
	retained, err := store.EntityAttribution(ctx, project.UID, "issue", issue.UID)
	require.NoError(t, err)
	require.Equal(t, receipt, retained)
	require.NoError(t, db.VerifyRootReceipt(pin, retained))
	_, err = store.AdoptProjectIntoFederation(ctx, db.AdoptProjectIntoFederationParams{ProjectID: project.ID, HubURL: "https://new-hub.example", HubProjectID: 84, HubProjectUID: "01BBBBBBBBBBBBBBBBBBBBBBBB", Actor: "member"})
	require.NoError(t, err, "retiring the original project UID preserves its creation proofs")
	retained, err = store.EntityAttribution(ctx, project.UID, "issue", issue.UID)
	require.NoError(t, err)
	require.Equal(t, receipt, retained)
	require.NoError(t, db.VerifyRootReceipt(pin, retained))
	var active int
	q := store.(interface {
		QueryRowContext(context.Context, string, ...any) *sql.Row
	})
	require.NoError(t, q.QueryRowContext(ctx, `SELECT active FROM federation_root_keys WHERE project_uid=$1 AND key_id=$2`, project.UID, pin.KeyID).Scan(&active))
	require.Zero(t, active, "retained proof keys no longer own the current replica")
	var exportedPin, exportedReceipt, exportedRef bool
	for record, exportErr := range store.ExportAttribution(ctx, db.ExportFilter{ProjectID: &project.ID, IncludeDeleted: true}) {
		require.NoError(t, exportErr)
		switch r := record.(type) {
		case *db.RootKeyPin:
			if r.ProjectUID == project.UID && r.KeyID == pin.KeyID {
				exportedPin = true
				require.True(t, r.Retired)
			}
		case *db.AttributionReceipt:
			if r.EventUID == event.UID {
				exportedReceipt = true
				require.Equal(t, receipt, *r)
			}
		case *db.EntityProvenance:
			if r.EntityUID == issue.UID && r.ProjectUID == project.UID {
				exportedRef = true
			}
		}
	}
	require.True(t, exportedPin, "current-owner exports include the original foreign proof key")
	require.True(t, exportedReceipt, "current-owner exports include the original signed receipt")
	require.True(t, exportedRef, "current-owner exports include the original creation reference")
	currentProject, err := store.ProjectByID(ctx, project.ID)
	require.NoError(t, err)
	for _, scope := range []struct {
		uids []string
		want bool
	}{
		{nil, true}, {[]string{}, false}, {[]string{project.UID}, false}, {[]string{currentProject.UID}, true},
	} {
		seen := false
		for record, exportErr := range store.ExportAttribution(db.WithAuthorizedProjects(ctx, scope.uids), db.ExportFilter{ProjectID: &project.ID, IncludeDeleted: true}) {
			require.NoError(t, exportErr)
			if r, ok := record.(*db.AttributionReceipt); ok && r.EventUID == event.UID {
				seen = true
			}
		}
		require.Equal(t, scope.want, seen, "foreign proof reads follow the current entity's authorized project")
	}

}

// Purged projects retain verification keys but cannot remain active signing roots.
func RunIssueMovePurgedRootRetirement(t *testing.T, store db.Storage) {
	t.Helper()
	ctx := t.Context()
	source, err := store.CreateProject(ctx, "source-root")
	require.NoError(t, err)
	destination, err := store.CreateProject(ctx, "destination-root")
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: source.ID, Role: db.FederationRoleHub, HubProjectID: source.ID, HubProjectUID: source.UID, Enabled: true})
	require.NoError(t, err)
	public, private, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	pin := db.RootKeyPin{ProjectUID: source.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}
	require.NoError(t, store.PinRootAuthority(ctx, pin))
	issue, _, err := store.CreateIssue(db.WithRootAttribution(ctx, db.RootAttributionSigner{AuthorityUID: pin.AuthorityUID, PrivateKey: private}, "tester"), db.CreateIssueParams{ProjectID: source.ID, Title: "Signed task", Author: "tester"})
	require.NoError(t, err)
	receipt, err := store.EntityAttribution(ctx, source.UID, "issue", issue.UID)
	require.NoError(t, err)
	require.NoError(t, db.VerifyRootReceipt(pin, receipt))
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: source.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 42, HubProjectUID: source.UID, Actor: "tester", Enabled: true})
	require.NoError(t, err)
	_, err = store.LeaveFederationReplica(ctx, source.ID)
	require.NoError(t, err)
	_, err = store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: destination.ID, IfMatchRev: issue.Revision, Actor: "tester"})
	require.NoError(t, err)
	_, _, err = store.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: source.ID, Actor: "tester"})
	require.NoError(t, err)
	_, err = store.PurgeProject(ctx, db.PurgeProjectParams{ProjectID: source.ID, Actor: "tester"})
	require.NoError(t, err)
	var found bool
	for record, exportErr := range store.ExportAttribution(ctx, db.ExportFilter{IncludeDeleted: true}) {
		require.NoError(t, exportErr)
		if exported, ok := record.(*db.RootKeyPin); ok && exported.ProjectUID == source.UID && exported.KeyID == pin.KeyID {
			found = true
			require.True(t, exported.Retired, "a purged root cannot participate in live signer selection")
		}
	}
	require.True(t, found, "original proof key remains exportable")
	retained, err := store.EntityAttribution(ctx, source.UID, "issue", issue.UID)
	require.NoError(t, err)
	require.Equal(t, receipt, retained)
	require.NoError(t, db.VerifyRootReceipt(pin, retained))
}

// An old name cannot silently switch identity; a conflict needs an actionable,
// typed error and must roll back the purge and signing-key retirement.
func RunIssueMoveRetainedNameConflict(t *testing.T, store db.Storage) {
	t.Helper()
	ctx := t.Context()
	source, err := store.CreateProject(ctx, "conflicting-source")
	require.NoError(t, err)
	destination, err := store.CreateProject(ctx, "reserved-owner")
	require.NoError(t, err)
	issue, event, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "Task", Author: "tester"})
	require.NoError(t, err)
	_, err = store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: destination.ID, IfMatchRev: issue.Revision, Actor: "tester"})
	require.NoError(t, err)
	q := store.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
		QueryRowContext(context.Context, string, ...any) *sql.Row
	})
	_, err = q.ExecContext(ctx, `INSERT INTO project_aliases(project_id,alias_identity,alias_kind,authority_uid,project_uid) VALUES(NULL,$1,'name',$2,$3)`, source.Name, store.InstanceUID(), destination.UID)
	require.NoError(t, err)
	_, _, err = store.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: source.ID, Actor: "tester"})
	require.NoError(t, err)
	_, err = store.PurgeProject(ctx, db.PurgeProjectParams{ProjectID: source.ID, Actor: "tester"})
	require.Error(t, err)
	var conflict interface{ RetainedName() string }
	require.True(t, errors.As(err, &conflict), "retained-name conflicts need a typed error")
	require.Equal(t, source.Name, conflict.RetainedName())
	require.Contains(t, err.Error(), "rename")
	var projectID int64
	require.NoError(t, q.QueryRowContext(ctx, `SELECT project_id FROM events WHERE uid=$1`, event.UID).Scan(&projectID))
	require.Equal(t, source.ID, projectID, "failed purge must roll back history detachment")
}

// Retained history follows the current authorized issue without rewriting its
// original identity or requiring a resurrected source project.
func RunIssueMoveRetainedHistoryRead(t *testing.T, store db.Storage) {
	t.Helper()
	ctx := t.Context()
	source, err := store.CreateProject(ctx, "history-source")
	require.NoError(t, err)
	destination, err := store.CreateProject(ctx, "history-destination")
	require.NoError(t, err)
	issue, original, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "Relocated task", Author: "tester"})
	require.NoError(t, err)
	_, err = store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: destination.ID, IfMatchRev: issue.Revision, Actor: "tester"})
	require.NoError(t, err)
	for _, purged := range []bool{false, true} {
		stage := "source-present"
		if purged {
			stage = "source-purged"
			_, _, err = store.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: source.ID, Actor: "tester"})
			require.NoError(t, err)
			_, err = store.PurgeProject(ctx, db.PurgeProjectParams{ProjectID: source.ID, Actor: "tester"})
			require.NoError(t, err)
		}
		t.Run(stage, func(t *testing.T) {
			for _, scope := range []struct {
				name    string
				allowed []string
				visible bool
			}{
				{"owner", nil, true}, {"current-project", []string{destination.UID}, true},
				{"original-project", []string{source.UID}, false}, {"no-projects", []string{}, false},
			} {
				t.Run(scope.name, func(t *testing.T) {
					readCtx := ctx
					if scope.allowed != nil {
						readCtx = db.WithAuthorizedProjects(ctx, scope.allowed)
					}
					events, readErr := store.EventsAfter(readCtx, db.EventsAfterParams{ProjectID: destination.ID, IssueUID: issue.UID, Limit: 100})
					require.NoError(t, readErr)
					window, windowErr := store.EventsInWindow(readCtx, db.EventsInWindowParams{ProjectID: destination.ID, Since: "2000-01-01T00:00:00.000Z", Until: "2099-01-01T00:00:00.000Z"})
					require.NoError(t, windowErr)
					for _, page := range [][]db.Event{events, window} {
						var found bool
						for _, event := range page {
							if event.UID != original.UID {
								continue
							}
							found = true
							require.Equal(t, original.ProjectUID, event.ProjectUID)
							require.Equal(t, original.ProjectName, event.ProjectName)
							require.Equal(t, original.ContentHash, event.ContentHash)
							require.Equal(t, original.Payload, event.Payload)
							if purged {
								require.Zero(t, event.ProjectID)
							}
						}
						require.Equal(t, scope.visible, found, "current owner history visibility")
					}
				})
			}
		})
	}
}
