package db

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"

	"go.kenn.io/kata/internal/embedding"
	"go.kenn.io/kata/internal/uid"
)

type MoveTreeNativeHooks struct {
	AllocateShortID func(context.Context, *sql.Tx, int64, string) (string, error)
	AppendEvent     func(context.Context, *sql.Tx, int64, string, string, string, int64, string, string) (Event, error)
	RestoreExternal func(context.Context, *sql.Tx, IssueTreePortion, string, string) error
	RestoreArtifact func(context.Context, *sql.Tx, embedding.EmbeddingArtifact) error
}

func moveConflictResult(r MoveTreeRequest) MoveTreeResult {
	return MoveTreeResult{State: "conflict", RequestUID: r.RequestUID, RetryAction: "reconcile the source and destination copies before retrying", Conflicts: []MoveTreeAction{{Kind: "changed-content", Message: ErrMoveConflict.Error()}}}
}

// ValidateIssueTreeSelection verifies exact owner membership before a native
// writer sees input. Numeric IDs have meaning only inside their source portion.
func ValidateIssueTreeSelection(snapshot IssueTreeSnapshot, r MoveTreeRequest) error {
	if err := ValidateIssueTreeSnapshotDigest(snapshot); err != nil {
		return err
	}
	if snapshot.RequestUID != r.RequestUID {
		return ErrMoveConflict
	}
	if len(snapshot.Required) > 0 || len(snapshot.RequiredSeries) > 0 {
		return &MoveRemoteSelectionError{Required: snapshot.Required, RequiredSeries: snapshot.RequiredSeries}
	}
	allUIDs := map[string]bool{}
	for _, portion := range snapshot.Portions {
		if !uid.Valid(portion.AuthorityUID) {
			return ErrMoveAuthorityMismatch
		}
		issueIDs := map[int64]string{}
		comments := map[int64]string{}
		links := map[int64]bool{}
		members := map[string]MoveTreeMember{}
		projects := map[int64]MoveTreeProject{}
		for _, p := range portion.Projects {
			if !uid.Valid(p.UID) || p.AuthorityUID != portion.AuthorityUID {
				return ErrMoveAuthorityMismatch
			}
			projects[p.ID] = p
		}
		for _, m := range portion.Members {
			if m.AuthorityUID != portion.AuthorityUID || members[m.IssueUID].IssueUID != "" {
				return ErrMoveConflict
			}
			members[m.IssueUID] = m
		}
		for _, i := range portion.Issues {
			p, ok := projects[i.ProjectID]
			if !ok || allUIDs[i.UID] || members[i.UID].ProjectUID != p.UID || members[i.UID].Revision != i.Revision || i.MovedToAuthorityUID != nil {
				return ErrMoveConflict
			}
			if err := validateImportRecord(&i); err != nil {
				return err
			}
			issueIDs[i.ID] = i.UID
			allUIDs[i.UID] = true
		}
		if len(members) != len(issueIDs) {
			return ErrMoveConflict
		}
		for _, c := range portion.Comments {
			if issueIDs[c.IssueID] == "" || comments[c.ID] != "" {
				return ErrMoveConflict
			}
			if err := validateImportRecord(&c); err != nil {
				return err
			}
			comments[c.ID] = c.UID
		}
		for _, l := range portion.Links {
			if members[l.FromIssueUID].IssueUID == "" && members[l.ToIssueUID].IssueUID == "" {
				return ErrMoveConflict
			}
			if err := validateImportRecord(&l); err != nil {
				return err
			}
			links[l.ID] = true
		}
		for _, l := range portion.Labels {
			if issueIDs[l.IssueID] == "" {
				return ErrMoveConflict
			}
			if err := validateImportRecord(&l); err != nil {
				return err
			}
		}
		for _, m := range portion.Mappings {
			if m.IssueID != nil && issueIDs[*m.IssueID] == "" || m.CommentID != nil && comments[*m.CommentID] == "" || m.LinkID != nil && !links[*m.LinkID] {
				return ErrMoveConflict
			}
			if err := validateImportRecord(&m); err != nil {
				return err
			}
		}
		for _, e := range portion.Events {
			selected := e.IssueUID != nil && members[*e.IssueUID].IssueUID != ""
			if strings.HasPrefix(e.Type, "recurrence.") {
				var subject struct {
					UID string `json:"recurrence_uid"`
				}
				if err := json.Unmarshal(e.Payload, &subject); err != nil {
					return err
				}
				for _, series := range portion.Recurrences {
					selected = selected || series.UID == subject.UID
				}
			}
			if !selected {
				return ErrMoveConflict
			}
			if err := ValidateMoveSourceEvent(e); err != nil {
				return err
			}
		}
		for _, a := range portion.Artifacts {
			if members[a.IssueUID].IssueUID == "" {
				return ErrMoveConflict
			}
			if err := embedding.ValidateArtifact(a, "", ""); err != nil {
				return err
			}
		}
		for _, proof := range portion.CreationProofs {
			selected := members[proof.EntityUID].IssueUID != ""
			if proof.Kind == "comment" {
				for _, c := range portion.Comments {
					selected = selected || c.UID == proof.EntityUID
				}
			}
			if !selected {
				return ErrMoveConflict
			}
			found := false
			for _, receipt := range portion.Receipts {
				if receipt.ProjectUID != proof.ProjectUID || receipt.EventUID != proof.EventUID {
					continue
				}
				for _, pin := range portion.PublicKeys {
					if pin.ProjectUID == receipt.ProjectUID && pin.KeyID == receipt.KeyID {
						if err := VerifyRootReceipt(pin, receipt); err != nil {
							return err
						}
						found = true
					}
				}
			}
			if !found {
				return fmt.Errorf("selected creation proof missing its verified receipt or public key")
			}
		}
	}
	if !allUIDs[r.IssueUID] {
		return ErrMoveConflict
	}
	return nil
}

func AuthorizeMoveDestination(ctx context.Context, q MoveTreeSQL, r MoveTreeRequest, instanceUID string) (MoveTreeProject, error) {
	if r.DestinationAuthorityUID != instanceUID {
		return MoveTreeProject{}, ErrMoveAuthorityMismatch
	}
	admitted, restricted := AuthorizedProjects(ctx)
	if restricted && !slices.Contains(admitted, r.DestinationProjectUID) {
		return MoveTreeProject{}, ErrNotFound
	}
	if err := CheckProjectAccessTransaction(ctx, q, r.DestinationProjectUID); err != nil {
		return MoveTreeProject{}, err
	}
	p := MoveTreeProject{AuthorityUID: instanceUID}
	if err := q.QueryRowContext(ctx, `SELECT id,uid,name FROM projects WHERE uid=$1 AND deleted_at IS NULL`, r.DestinationProjectUID).Scan(&p.ID, &p.UID, &p.Name); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return p, ErrNotFound
		}
		return p, err
	}
	var bindingCount int
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM federation_bindings WHERE project_id=$1`, p.ID).Scan(&bindingCount); err != nil {
		return p, err
	}
	if bindingCount > 0 {
		return p, ErrFederatedMoveUnsupported
	}
	return p, nil
}

// insertMoveRow binds every value and only receives fixed table/column lists
// from this file. Backend-generated numeric IDs never cross authority boundaries.
func insertMoveRow(ctx context.Context, q MoveTreeSQL, table, columns string, values ...any) (int64, error) {
	fields := strings.Split(columns, ",")
	if len(fields) != len(values) {
		return 0, fmt.Errorf("move owner field count mismatch")
	}
	placeholders := make([]string, len(values))
	for i := range values {
		placeholders[i] = fmt.Sprintf("$%d", i+1)
	}
	var id int64
	err := q.QueryRowContext(ctx, "INSERT INTO "+table+"("+columns+") VALUES("+strings.Join(placeholders, ",")+") RETURNING id", values...).Scan(&id)
	return id, err
}

func moveOwnerID(ctx context.Context, q MoveTreeSQL, table, column, value string) (*int64, error) {
	var id int64
	err := q.QueryRowContext(ctx, "SELECT id FROM "+table+" WHERE "+column+"=$1", value).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &id, nil
}

func AcceptIssueTreeSQL(ctx context.Context, tx *sql.Tx, snapshot IssueTreeSnapshot, r MoveTreeRequest, instanceUID string, hooks MoveTreeNativeHooks, postgres bool) (MoveTreeResult, error) {
	p, err := AuthorizeMoveDestination(ctx, tx, r, instanceUID)
	if err != nil {
		return MoveTreeResult{}, err
	}
	requestDigest, err := MoveRequestDigest(r)
	if err != nil {
		return MoveTreeResult{}, err
	}
	if result, found, err := LookupIssueTreeAcceptanceSQL(ctx, tx, r, snapshot.Digest, instanceUID, postgres); err != nil {
		return moveConflictResult(r), err
	} else if found {
		return result, nil
	}
	result := MoveTreeResult{State: "partial", RequestUID: r.RequestUID, SnapshotDigest: snapshot.Digest, RetryAction: "verify this destination commit and finalize every source authority"}
	for _, portion := range snapshot.Portions {
		for _, series := range portion.Recurrences {
			if !r.MoveSeries || series.MovedToAuthorityUID != nil {
				return moveConflictResult(r), ErrMoveConflict
			}
			if err := validateImportRecord(&series); err != nil {
				return result, err
			}
			existing, err := moveOwnerID(ctx, tx, "recurrences", "uid", series.UID)
			if err != nil {
				return result, err
			}
			if existing != nil {
				if err := validateReturnedRecurrence(ctx, tx, snapshot, portion, series, *existing); err != nil {
					return moveConflictResult(r), err
				}
			}
			if _, err := writeReceivedOwner(ctx, tx, "recurrences", existing, "uid,project_id,rrule,dtstart,timezone,template_title,template_body,template_owner,template_priority,template_labels,template_metadata,next_occurrence_key,last_materialized_uid,author,revision,created_at,updated_at,deleted_at,location_generation", series.UID, p.ID, series.RRule, series.DTStart, series.Timezone, series.TemplateTitle, series.TemplateBody, series.TemplateOwner, series.TemplatePriority, string(series.TemplateLabels), string(series.TemplateMetadata), series.NextOccurrenceKey, series.LastMaterializedUID, series.Author, series.Revision+1, series.CreatedAt, series.UpdatedAt, series.DeletedAt, series.LocationGeneration+1); err != nil {
				return result, err
			}
		}
	}
	issueIDs := map[string]int64{}
	returnedIDs := map[int64]bool{}
	shortIDs := map[string]string{}
	for _, portion := range snapshot.Portions {
		for _, i := range portion.Issues {
			existing, err := moveOwnerID(ctx, tx, "issues", "uid", i.UID)
			if err != nil {
				return result, err
			}
			if existing != nil {
				if err := validateReturnedIssue(ctx, tx, portion, i, *existing); err != nil {
					return moveConflictResult(r), err
				}
				returnedIDs[*existing] = true
			}
			short, err := hooks.AllocateShortID(ctx, tx, p.ID, i.UID)
			if err != nil {
				return result, err
			}
			shortIDs[i.UID] = short
			var recurrenceID *int64
			if r.MoveSeries && i.RecurrenceUID != nil {
				recurrenceID, err = moveOwnerID(ctx, tx, "recurrences", "uid", *i.RecurrenceUID)
				if err != nil {
					return result, err
				}
			}
			recurrenceAuthority, recurrenceProject := i.RecurrenceAuthorityUID, i.RecurrenceProjectUID
			if i.RecurrenceUID != nil && recurrenceAuthority == nil {
				recurrenceAuthority = new(portion.AuthorityUID)
				recurrenceProject = new(portionProjectUID(portion, i.ProjectID))
			}
			if r.MoveSeries && recurrenceID != nil {
				recurrenceAuthority = new(instanceUID)
				recurrenceProject = new(p.UID)
			}
			ownerAuthority := i.OwnerAuthorityUID
			if ownerAuthority == nil {
				ownerAuthority = new(portion.AuthorityUID)
			}
			id, err := writeReceivedIssue(ctx, tx, existing, "uid,project_id,short_id,title,body,status,closed_reason,owner,assignment_expires_on,priority,author,created_at,updated_at,closed_at,deleted_at,metadata,revision,content_revision,recurrence_id,recurrence_uid,recurrence_authority_uid,recurrence_project_uid,occurrence_key,owner_authority_uid,location_generation", i.UID, p.ID, short, i.Title, i.Body, i.Status, i.ClosedReason, i.Owner, i.AssignmentExpiresOn, i.Priority, i.Author, i.CreatedAt, i.UpdatedAt, i.ClosedAt, i.DeletedAt, string(i.Metadata), i.Revision+1, i.ContentRevision, recurrenceID, i.RecurrenceUID, recurrenceAuthority, recurrenceProject, i.OccurrenceKey, ownerAuthority, i.LocationGeneration+1)
			if err != nil {
				return result, err
			}
			issueIDs[i.UID] = id
		}
	}
	for _, portion := range snapshot.Portions {
		localIDs := map[int64]int64{}
		commentIDs := map[int64]int64{}
		linkIDs := map[int64]int64{}
		for _, i := range portion.Issues {
			localIDs[i.ID] = issueIDs[i.UID]
		}
		for _, c := range portion.Comments {
			existing, err := moveOwnerID(ctx, tx, "comments", "uid", c.UID)
			if err != nil {
				return result, err
			}
			if existing != nil {
				var owner int64
				var author, createdAt string
				if err := tx.QueryRowContext(ctx, `SELECT issue_id,author,CAST(created_at AS TEXT) FROM comments WHERE id=$1`, *existing).Scan(&owner, &author, &createdAt); err != nil {
					return result, err
				}
				if owner != localIDs[c.IssueID] || author != c.Author || createdAt != c.CreatedAt {
					return moveConflictResult(r), ErrMoveConflict
				}
				if _, err := tx.ExecContext(ctx, `UPDATE comments SET body=$1,teammate=$2,reply_to_uid=$3,reply_kind=$4,edited_at=$5 WHERE id=$6`, c.Body, moveOptionalString(c.Teammate), moveOptionalString(c.ReplyToUID), moveOptionalString(c.ReplyKind), moveOptionalString(c.EditedAt), *existing); err != nil {
					return result, err
				}
				commentIDs[c.ID] = *existing
				continue
			}
			id, err := insertMoveRow(ctx, tx, "comments", "uid,issue_id,author,body,created_at,teammate,reply_to_uid,reply_kind,edited_at", c.UID, localIDs[c.IssueID], c.Author, c.Body, c.CreatedAt, moveOptionalString(c.Teammate), moveOptionalString(c.ReplyToUID), moveOptionalString(c.ReplyKind), moveOptionalString(c.EditedAt))
			if err != nil {
				return result, err
			}
			commentIDs[c.ID] = id
		}

		// Label membership is mutable. Replace the retained projection only
		// after its issue's original identity and prior commit were verified.
		for _, id := range localIDs {
			if returnedIDs[id] {
				if _, err := tx.ExecContext(ctx, `DELETE FROM issue_labels WHERE issue_id=$1`, id); err != nil {
					return result, err
				}
			}
		}
		for _, l := range portion.Labels {
			if _, err := tx.ExecContext(ctx, `INSERT INTO issue_labels(issue_id,label,author,created_at) VALUES($1,$2,$3,$4)`, localIDs[l.IssueID], l.Label, l.Author, l.CreatedAt); err != nil {
				return result, err
			}
		}
		for _, l := range portion.Links {
			from, err := moveOwnerID(ctx, tx, "issues", "uid", l.FromIssueUID)
			if err != nil {
				return result, err
			}
			to, err := moveOwnerID(ctx, tx, "issues", "uid", l.ToIssueUID)
			if err != nil {
				return result, err
			}
			fromAuthority, fromProject, toAuthority, toProject := l.FromAuthorityUID, l.FromProjectUID, l.ToAuthorityUID, l.ToProjectUID
			if issueIDs[l.FromIssueUID] != 0 {
				fromAuthority = new(instanceUID)
				fromProject = new(p.UID)
			}
			if issueIDs[l.ToIssueUID] != 0 {
				toAuthority = new(instanceUID)
				toProject = new(p.UID)
			}
			id, err := writeReceivedLink(ctx, tx, l, from, to, fromAuthority, fromProject, toAuthority, toProject)
			if err != nil {
				return result, err
			}
			linkIDs[l.ID] = id
		}
		for _, e := range portion.Events {
			if err := retainMoveEvent(ctx, tx, e, p.ID, issueIDs); err != nil {
				return result, err
			}
		}
		for _, m := range portion.Mappings {
			if r.ProviderDisposition == "detach" {
				continue
			}
			normalized, err := NormalizeIssueStatusExport(m)
			if err != nil {
				return result, err
			}
			m = normalized
			var issue, comment, link *int64
			if m.IssueID != nil {
				issue = new(localIDs[*m.IssueID])
			}
			if m.CommentID != nil {
				comment = new(commentIDs[*m.CommentID])
			}
			if m.LinkID != nil {
				link = new(linkIDs[*m.LinkID])
			}
			if err := writeReceivedMapping(ctx, tx, portion, m, p.ID, issue, comment, link, returnedIDs); err != nil {
				return result, err
			}
		}
		for _, c := range portion.Claims {
			id := issueIDs[c.IssueUID]
			if _, err := insertMoveRow(ctx, tx, "issue_claims", "claim_uid,project_id,issue_id,issue_uid,holder,holder_instance_uid,client_kind,purpose,claim_kind,acquired_at,expires_at,released_at,release_reason,revision,updated_at", c.ClaimUID, p.ID, id, c.IssueUID, c.Holder, c.HolderInstanceUID, c.ClientKind, c.Purpose, c.ClaimKind, c.AcquiredAt, c.ExpiresAt, c.ReleasedAt, c.ReleaseReason, c.Revision, c.UpdatedAt); err != nil {
				return result, err
			}
		}
		for _, c := range portion.PendingClaims {
			id := issueIDs[c.IssueUID]
			if _, err := insertMoveRow(ctx, tx, "pending_claim_requests", "request_uid,project_id,issue_id,issue_uid,holder,holder_instance_uid,client_kind,claim_kind,ttl_seconds,purpose,requested_at,last_attempt_at,last_error,rejected_at,resolved_at", c.RequestUID, p.ID, id, c.IssueUID, c.Holder, c.HolderInstanceUID, c.ClientKind, c.ClaimKind, c.TTLSeconds, c.Purpose, c.RequestedAt, c.LastAttemptAt, c.LastError, c.RejectedAt, c.ResolvedAt); err != nil {
				return result, err
			}
		}
		for _, tombstone := range portion.Tombstones {
			if err := validateImportRecord(&tombstone); err != nil {
				return result, err
			}
			if existing, err := moveOwnerID(ctx, tx, "purge_log", "uid", tombstone.UID); err != nil {
				return result, err
			} else if existing != nil {
				if err := validateRetainedTombstone(ctx, tx, tombstone, *existing); err != nil {
					return moveConflictResult(r), err
				}
				if _, err := tx.ExecContext(ctx, `UPDATE purge_log SET project_id=$1 WHERE id=$2`, p.ID, *existing); err != nil {
					return result, err
				}
				continue
			}
			if _, err := insertMoveRow(ctx, tx, "purge_log", "uid,origin_instance_uid,project_id,purged_issue_id,issue_uid,project_uid,project_name,short_id,issue_title,issue_author,comment_count,link_count,label_count,event_count,events_deleted_min_id,events_deleted_max_id,purge_reset_after_event_id,actor,reason,purged_at,recurrence_uid,occurrence_key", tombstone.UID, tombstone.OriginInstanceUID, p.ID, tombstone.PurgedIssueID, tombstone.IssueUID, tombstone.ProjectUID, tombstone.ProjectName, tombstone.ShortID, tombstone.IssueTitle, tombstone.IssueAuthor, tombstone.CommentCount, tombstone.LinkCount, tombstone.LabelCount, tombstone.EventCount, tombstone.EventsDeletedMinID, tombstone.EventsDeletedMaxID, tombstone.PurgeResetAfterEventID, tombstone.Actor, tombstone.Reason, tombstone.PurgedAt, tombstone.RecurrenceUID, tombstone.OccurrenceKey); err != nil {
				return result, err
			}
		}
		if err := retainMoveProvenance(ctx, tx, portion); err != nil {
			return result, err
		}
		if hooks.RestoreExternal != nil {
			if err := hooks.RestoreExternal(ctx, tx, portion, p.UID, r.ProviderDisposition); err != nil {
				return result, err
			}
		}
		for _, a := range portion.Artifacts {
			rebound, err := embedding.RebindArtifactProject(a, p.UID)
			if err != nil {
				return result, err
			}
			if hooks.RestoreArtifact == nil {
				return result, fmt.Errorf("destination cannot retain complete embedding artifacts")
			}
			if err := hooks.RestoreArtifact(ctx, tx, rebound); err != nil {
				return result, err
			}
		}
	}
	// Ordinary per-issue scalar audit events are the only retry records.
	for _, portion := range snapshot.Portions {
		for _, i := range portion.Issues {
			ownerDigest, err := IssueTreeOwnerDigest(ctx, tx, i.UID, r.RequestUID, postgres)
			if err != nil {
				return result, err
			}
			sourceDigest := portion.SourceSnapshotDigest
			if sourceDigest == "" {
				sourceDigest = snapshot.Digest
			}
			sourceSeriesUID, sourceSeriesProjectUID, sourceSeriesAuthorityUID, sourceSeriesSnapshotDigest := "", "", "", ""
			for _, owner := range snapshot.Portions {
				for _, series := range owner.Recurrences {
					if i.RecurrenceUID != nil && *i.RecurrenceUID == series.UID {
						sourceSeriesUID, sourceSeriesProjectUID, sourceSeriesAuthorityUID = series.UID, portionProjectUID(owner, series.ProjectID), owner.AuthorityUID
						sourceSeriesSnapshotDigest = owner.SourceSnapshotDigest
						if sourceSeriesSnapshotDigest == "" {
							sourceSeriesSnapshotDigest = snapshot.Digest
						}
					}
				}
			}
			fact := map[string]any{"request_uid": r.RequestUID, "request_digest": requestDigest, "snapshot_digest": snapshot.Digest, "received_issue_digest": ownerDigest, "source_snapshot_digest": sourceDigest, "source_series_uid": sourceSeriesUID, "source_series_project_uid": sourceSeriesProjectUID, "source_series_authority_uid": sourceSeriesAuthorityUID, "source_series_snapshot_digest": sourceSeriesSnapshotDigest, "issue_uid": i.UID, "from_authority_uid": portion.AuthorityUID, "from_project_uid": portionProjectUID(portion, i.ProjectID), "from_short_id": i.ShortID, "to_authority_uid": instanceUID, "to_project_uid": p.UID, "to_short_id": shortIDs[i.UID], "revision": i.Revision + 1, "location_generation": i.LocationGeneration + 1, "actor_authority_uid": r.ActorAuthorityUID, "provider_disposition": r.ProviderDisposition}
			payload, err := json.Marshal(fact, json.Deterministic(true))
			if err != nil {
				return result, err
			}
			event, err := hooks.AppendEvent(ctx, tx, p.ID, p.UID, p.Name, i.UID, issueIDs[i.UID], string(payload), r.Actor)
			if err != nil {
				return result, err
			}
			result.Events = append(result.Events, event)
			result.Applied = append(result.Applied, MoveTreeApplied{SourceSnapshotDigest: sourceDigest, SourceSeriesUID: sourceSeriesUID, SourceSeriesProjectUID: sourceSeriesProjectUID, SourceSeriesAuthorityUID: sourceSeriesAuthorityUID, SourceSeriesSnapshotDigest: sourceSeriesSnapshotDigest, IssueUID: i.UID, FromAuthorityUID: portion.AuthorityUID, FromProjectUID: portionProjectUID(portion, i.ProjectID), FromShortID: i.ShortID, AuthorityUID: instanceUID, ProjectUID: p.UID, ShortID: shortIDs[i.UID], Revision: i.Revision + 1, LocationGeneration: i.LocationGeneration + 1, CommittedEventUID: event.UID, CommittedEventHash: event.ContentHash})
		}
	}
	return result, nil
}

func portionProjectUID(portion IssueTreePortion, id int64) string {
	for _, p := range portion.Projects {
		if p.ID == id {
			return p.UID
		}
	}
	return ""
}

func retainMoveEvent(ctx context.Context, q MoveTreeSQL, e EventExport, projectID int64, issueIDs map[string]int64) error {
	var hash string
	var original []byte
	err := q.QueryRowContext(ctx, `SELECT content_hash,source_event_bytes FROM events WHERE uid=$1`, e.UID).Scan(&hash, &original)
	if err == nil {
		if hash != e.ContentHash {
			return ErrMoveConflict
		}
		if len(original) > 0 && !slices.Equal(original, e.SourceEventBytes) {
			return ErrMoveConflict
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var issueID, relatedID *int64
	if e.IssueUID != nil && issueIDs[*e.IssueUID] != 0 {
		issueID = new(issueIDs[*e.IssueUID])
	}
	if e.RelatedIssueUID != nil {
		relatedID, err = moveOwnerID(ctx, q, "issues", "uid", *e.RelatedIssueUID)
		if err != nil {
			return err
		}
	}
	_, err = insertMoveRow(ctx, q, "events", "uid,origin_instance_uid,project_id,project_name,issue_id,issue_uid,related_issue_id,related_issue_uid,type,actor,payload,hlc_physical_ms,hlc_counter,content_hash,created_at,source_project_uid,source_event_bytes", e.UID, e.OriginInstanceUID, projectID, e.ProjectName, issueID, e.IssueUID, relatedID, e.RelatedIssueUID, e.Type, e.Actor, string(e.Payload), e.HLCPhysicalMS, e.HLCCounter, e.ContentHash, e.CreatedAt, e.SourceProjectUID, e.SourceEventBytes)
	return err
}

func retainMoveProvenance(ctx context.Context, q MoveTreeSQL, p IssueTreePortion) error {
	for _, pin := range p.PublicKeys {
		encoded := base64.StdEncoding.EncodeToString(pin.PublicKey)
		var prior string
		err := q.QueryRowContext(ctx, `SELECT public_key FROM federation_root_keys WHERE project_uid=$1 AND key_id=$2`, pin.ProjectUID, pin.KeyID).Scan(&prior)
		if err == nil {
			if prior != encoded {
				return ErrMoveConflict
			}
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		active := 0
		if !pin.Retired {
			active = 1
		}
		if _, err := q.ExecContext(ctx, `INSERT INTO federation_root_keys(project_uid,authority_uid,key_id,public_key,active) VALUES($1,$2,$3,$4,$5)`, pin.ProjectUID, pin.AuthorityUID, pin.KeyID, encoded, active); err != nil {
			return err
		}
	}
	for _, receipt := range p.Receipts {
		raw, err := json.Marshal(receipt, json.Deterministic(true))
		if err != nil {
			return err
		}
		var prior string
		err = q.QueryRowContext(ctx, `SELECT receipt FROM federation_event_provenance WHERE project_uid=$1 AND event_uid=$2`, receipt.ProjectUID, receipt.EventUID).Scan(&prior)
		if err == nil {
			var existing AttributionReceipt
			if err := json.Unmarshal([]byte(prior), &existing); err != nil {
				return err
			}
			existingRaw, err := json.Marshal(existing, json.Deterministic(true))
			if err != nil {
				return err
			}
			if !slices.Equal(existingRaw, raw) {
				return ErrMoveConflict
			}
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if _, err := q.ExecContext(ctx, `INSERT INTO federation_event_provenance(project_uid,event_uid,content_hash,reset_epoch,sequence,key_id,receipt) VALUES($1,$2,$3,$4,$5,$6,$7)`, receipt.ProjectUID, receipt.EventUID, receipt.ContentHash, receipt.ResetEpoch, receipt.Sequence, receipt.KeyID, string(raw)); err != nil {
			return err
		}
	}
	for _, proof := range p.CreationProofs {
		var prior string
		err := q.QueryRowContext(ctx, `SELECT event_uid FROM federation_entity_provenance WHERE project_uid=$1 AND kind=$2 AND entity_uid=$3`, proof.ProjectUID, proof.Kind, proof.EntityUID).Scan(&prior)
		if err == nil {
			if prior != proof.EventUID {
				return ErrMoveConflict
			}
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if _, err := q.ExecContext(ctx, `INSERT INTO federation_entity_provenance(project_uid,kind,entity_uid,event_uid) VALUES($1,$2,$3,$4)`, proof.ProjectUID, proof.Kind, proof.EntityUID, proof.EventUID); err != nil {
			return err
		}
	}
	return nil
}

func moveOptionalString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func writeReceivedMapping(ctx context.Context, q MoveTreeSQL, portion IssueTreePortion, m ImportMappingExport, project int64, issue, comment, link *int64, returned map[int64]bool) error {
	var id int64
	var oldIssue, oldComment, oldLink *int64
	var owner, authority, sourceProject *string
	err := q.QueryRowContext(ctx, `SELECT id,issue_id,comment_id,link_id,moved_issue_uid,moved_to_authority_uid,moved_to_project_uid FROM import_mappings WHERE source=$1 AND external_id=$2 AND object_type=$3 AND project_id=$4`, m.Source, m.ExternalID, m.ObjectType, project).Scan(&id, &oldIssue, &oldComment, &oldLink, &owner, &authority, &sourceProject)
	if errors.Is(err, sql.ErrNoRows) {
		_, err = insertMoveRow(ctx, q, "import_mappings", "source,external_id,object_type,project_id,issue_id,comment_id,link_id,label,source_updated_at,imported_at,observed_status,observed_status_at,pending_event_uid,remote_locator", m.Source, m.ExternalID, m.ObjectType, project, issue, comment, link, m.Label, m.SourceUpdatedAt, m.ImportedAt, m.ObservedStatus, m.ObservedStatusAt, m.PendingEventUID, m.RemoteLocator)
		return err
	}
	if err != nil {
		return err
	}
	sameID := func(a, b *int64) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }
	if owner == nil || authority == nil || sourceProject == nil || *authority != portion.AuthorityUID || !sameID(oldIssue, issue) || !sameID(oldComment, comment) || !sameID(oldLink, link) {
		return ErrMoveConflict
	}
	verified := false
	for _, incoming := range portion.Issues {
		if incoming.UID != *owner || portionProjectUID(portion, incoming.ProjectID) != *sourceProject {
			continue
		}
		var localID int64
		if err := q.QueryRowContext(ctx, `SELECT id FROM issues WHERE uid=$1`, incoming.UID).Scan(&localID); err != nil {
			return err
		}
		verified = returned[localID]
	}
	if !verified {
		return ErrMoveConflict
	}
	_, err = q.ExecContext(ctx, `UPDATE import_mappings SET label=$1,source_updated_at=$2,imported_at=$3,observed_status=$4,observed_status_at=$5,pending_event_uid=$6,remote_locator=$7,moved_issue_uid=NULL,moved_to_authority_uid=NULL,moved_to_project_uid=NULL WHERE id=$8`, m.Label, m.SourceUpdatedAt, m.ImportedAt, m.ObservedStatus, m.ObservedStatusAt, m.PendingEventUID, m.RemoteLocator, id)
	return err
}

func validateRetainedTombstone(ctx context.Context, q MoveTreeSQL, incoming PurgeLogExport, id int64) error {
	var retained PurgeLogExport
	if err := q.QueryRowContext(ctx, `SELECT id,uid,origin_instance_uid,project_id,purged_issue_id,issue_uid,project_uid,project_name,short_id,issue_title,issue_author,comment_count,link_count,label_count,event_count,events_deleted_min_id,events_deleted_max_id,purge_reset_after_event_id,actor,reason,CAST(purged_at AS TEXT),recurrence_uid,occurrence_key FROM purge_log WHERE id=$1`, id).Scan(&retained.ID, &retained.UID, &retained.OriginInstanceUID, &retained.ProjectID, &retained.PurgedIssueID, &retained.IssueUID, &retained.ProjectUID, &retained.ProjectName, &retained.ShortID, &retained.IssueTitle, &retained.IssueAuthor, &retained.CommentCount, &retained.LinkCount, &retained.LabelCount, &retained.EventCount, &retained.EventsDeletedMinID, &retained.EventsDeletedMaxID, &retained.PurgeResetAfterEventID, &retained.Actor, &retained.Reason, &retained.PurgedAt, &retained.RecurrenceUID, &retained.OccurrenceKey); err != nil {
		return err
	}
	// Only physical location and backend row identity change. Original purge
	// identity, historical coordinates, attribution and occurrence stay exact.
	incoming.ID, incoming.ProjectID = retained.ID, retained.ProjectID
	a, err := json.Marshal(incoming, json.Deterministic(true))
	if err != nil {
		return err
	}
	b, err := json.Marshal(retained, json.Deterministic(true))
	if err != nil {
		return err
	}
	if string(a) != string(b) {
		return ErrMoveConflict
	}
	return nil
}

func validateReturnedIssue(ctx context.Context, q MoveTreeSQL, p IssueTreePortion, incoming IssueExport, id int64) error {
	var movedEventUID *string
	var generation int64
	var author, created string
	if err := q.QueryRowContext(ctx, `SELECT moved_event_uid,location_generation,author,CAST(created_at AS TEXT) FROM issues WHERE id=$1`, id).Scan(&movedEventUID, &generation, &author, &created); err != nil {
		return err
	}
	if movedEventUID == nil || generation > incoming.LocationGeneration || author != incoming.Author || created != incoming.CreatedAt {
		return ErrMoveConflict
	}
	var payload string
	if err := q.QueryRowContext(ctx, `SELECT payload FROM events WHERE uid=$1 AND type='issue.moved'`, *movedEventUID).Scan(&payload); err != nil {
		return err
	}
	var fact struct {
		ReceivedUID  string `json:"received_event_uid"`
		ReceivedHash string `json:"received_event_hash"`
	}
	if err := json.Unmarshal([]byte(payload), &fact); err != nil {
		return err
	}
	for _, event := range p.Events {
		if event.UID == fact.ReceivedUID && event.ContentHash == fact.ReceivedHash && event.IssueUID != nil && *event.IssueUID == incoming.UID {
			return nil
		}
	}
	return ErrMoveConflict
}

func writeReceivedIssue(ctx context.Context, q MoveTreeSQL, existing *int64, columns string, values ...any) (int64, error) {
	return writeReceivedOwner(ctx, q, "issues", existing, columns, values...)
}

func writeReceivedOwner(ctx context.Context, q MoveTreeSQL, table string, existing *int64, columns string, values ...any) (int64, error) {
	if existing == nil {
		return insertMoveRow(ctx, q, table, columns, values...)
	}
	fields := strings.Split(columns, ",")
	if len(fields) != len(values) {
		return 0, fmt.Errorf("received issue field count mismatch")
	}
	assignments := make([]string, len(fields))
	for i, field := range fields {
		assignments[i] = fmt.Sprintf("%s=$%d", field, i+1)
	}
	values = append(values, *existing)
	clear := ",moved_to_authority_uid=NULL,moved_to_project_uid=NULL,moved_event_uid=NULL"
	if table == "issues" {
		clear += ",moved_to_short_id=NULL"
	}
	_, err := q.ExecContext(ctx, "UPDATE "+table+" SET "+strings.Join(assignments, ",")+clear+" WHERE id="+fmt.Sprintf("$%d", len(values)), values...)
	return *existing, err
}

func validateReturnedRecurrence(ctx context.Context, q MoveTreeSQL, snapshot IssueTreeSnapshot, portion IssueTreePortion, incoming RecurrenceExport, id int64) error {
	var movedEvent, authority, project *string
	var generation int64
	var author, created string
	if err := q.QueryRowContext(ctx, `SELECT moved_event_uid,moved_to_authority_uid,moved_to_project_uid,location_generation,author,CAST(created_at AS TEXT) FROM recurrences WHERE id=$1`, id).Scan(&movedEvent, &authority, &project, &generation, &author, &created); err != nil {
		return err
	}
	if movedEvent == nil || authority == nil || project == nil || *authority != portion.AuthorityUID || *project != portionProjectUID(portion, incoming.ProjectID) || generation > incoming.LocationGeneration || author != incoming.Author || created != incoming.CreatedAt {
		return ErrMoveConflict
	}
	var payload string
	if err := q.QueryRowContext(ctx, `SELECT payload FROM events WHERE uid=$1 AND type='recurrence.moved'`, *movedEvent).Scan(&payload); err != nil {
		return err
	}
	type moveFact struct {
		RequestUID    string `json:"request_uid"`
		RequestDigest string `json:"request_digest"`
		SeriesUID     string `json:"source_series_uid"`
	}
	var retained moveFact
	if err := json.Unmarshal([]byte(payload), &retained); err != nil {
		return err
	}
	for _, p := range snapshot.Portions {
		members := map[string]bool{}
		for _, i := range p.Issues {
			if i.RecurrenceUID != nil && *i.RecurrenceUID == incoming.UID {
				members[i.UID] = true
			}
		}
		for _, e := range p.Events {
			if e.Type != "issue.move_received" || e.IssueUID == nil {
				continue
			}
			var received moveFact
			if err := json.Unmarshal(e.Payload, &received); err != nil {
				return err
			}
			if retained.RequestUID != "" && retained.RequestDigest != "" && received.RequestUID == retained.RequestUID && received.RequestDigest == retained.RequestDigest && (received.SeriesUID == incoming.UID || members[*e.IssueUID]) {
				return nil
			}
		}
	}
	return ErrMoveConflict
}

func writeReceivedLink(ctx context.Context, q MoveTreeSQL, l LinkExport, from, to *int64, fromAuthority, fromProject, toAuthority, toProject *string) (int64, error) {
	var id int64
	var author, created string
	err := q.QueryRowContext(ctx, `SELECT id,author,CAST(created_at AS TEXT) FROM links WHERE from_issue_uid=$1 AND to_issue_uid=$2 AND type=$3 AND COALESCE(from_comment_uid,'')=COALESCE($4,'') AND COALESCE(to_comment_uid,'')=COALESCE($5,'') AND COALESCE(reply_kind,'')=COALESCE($6,'')`, l.FromIssueUID, l.ToIssueUID, l.Type, l.FromCommentUID, l.ToCommentUID, l.ReplyKind).Scan(&id, &author, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return insertMoveRow(ctx, q, "links", "from_issue_id,to_issue_id,from_issue_uid,to_issue_uid,type,author,created_at,from_authority_uid,from_project_uid,to_authority_uid,to_project_uid,from_comment_uid,to_comment_uid,reply_kind", from, to, l.FromIssueUID, l.ToIssueUID, l.Type, l.Author, l.CreatedAt, fromAuthority, fromProject, toAuthority, toProject, l.FromCommentUID, l.ToCommentUID, l.ReplyKind)
	}
	if err != nil {
		return 0, err
	}
	if author != l.Author || created != l.CreatedAt {
		return 0, ErrMoveConflict
	}
	_, err = q.ExecContext(ctx, `UPDATE links SET from_issue_id=$1,to_issue_id=$2,from_authority_uid=$3,from_project_uid=$4,to_authority_uid=$5,to_project_uid=$6 WHERE id=$7`, from, to, fromAuthority, fromProject, toAuthority, toProject, id)
	return id, err
}
