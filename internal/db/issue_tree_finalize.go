package db

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"
)

var ErrMoveCommitUnverified = errors.New("destination commit has not been independently verified")

type moveCommitVerifierKey struct{}

// MoveDestinationCommitVerifier checks the authenticated destination's actual
// committed facts. It runs before opening the source writer transaction.
type MoveDestinationCommitVerifier func(context.Context, MoveTreeRequest, MoveTreeResult) error

func WithMoveDestinationCommitVerifier(ctx context.Context, verify MoveDestinationCommitVerifier) context.Context {
	return context.WithValue(ctx, moveCommitVerifierKey{}, verify)
}
func VerifyMoveDestinationCommit(ctx context.Context, r MoveTreeRequest, result MoveTreeResult) error {
	verify, _ := ctx.Value(moveCommitVerifierKey{}).(MoveDestinationCommitVerifier)
	if verify == nil {
		return ErrMoveCommitUnverified
	}
	if result.RequestUID != r.RequestUID || len(result.Applied) == 0 || (result.State != "partial" && result.State != "completed") {
		return ErrMoveCommitUnverified
	}
	for _, a := range result.Applied {
		if a.AuthorityUID != r.DestinationAuthorityUID || a.ProjectUID != r.DestinationProjectUID || a.CommittedEventUID == "" || a.CommittedEventHash == "" {
			return ErrMoveCommitUnverified
		}
	}
	return verify(ctx, r, result)
}

// ValidateCommittedMoveEvidence compares exact authenticated readback with the
// proposed acceptance. No transport ACK or caller-supplied event is sufficient.
func ValidateCommittedMoveEvidence(proposed, actual MoveTreeResult) error {
	if proposed.RequestUID != actual.RequestUID || proposed.SnapshotDigest == "" || proposed.SnapshotDigest != actual.SnapshotDigest || len(proposed.Applied) != len(actual.Applied) {
		return ErrMoveCommitUnverified
	}
	for _, expected := range proposed.Applied {
		if !slices.Contains(actual.Applied, expected) {
			return ErrMoveCommitUnverified
		}
	}
	return nil
}

func FinalizeIssueTreeSQL(ctx context.Context, tx *sql.Tx, store Storage, r MoveTreeRequest, preview MoveTreePreview, accepted MoveTreeResult, appendEvent func(context.Context, *sql.Tx, int64, string, string, string, int64, string, string) (Event, error), postgres bool) (MoveTreeResult, error) {
	result := accepted
	result.Events = nil
	result.Finalized = nil
	roots, err := IssueTreeSourceRoots(ctx, r, store.InstanceUID())
	if err != nil {
		return result, err
	}
	projects := []string{}
	for _, root := range roots {
		if !slices.Contains(projects, root.ProjectUID) {
			projects = append(projects, root.ProjectUID)
		}
	}
	seriesRoots, err := IssueTreeSourceSeries(ctx, r, store.InstanceUID())
	if err != nil {
		return result, err
	}
	for _, root := range seriesRoots {
		if !slices.Contains(projects, root.ProjectUID) {
			projects = append(projects, root.ProjectUID)
		}
	}
	args := []any{r.RequestUID}
	placeholders := []string{}
	for _, project := range projects {
		args = append(args, project)
		placeholders = append(placeholders, fmt.Sprintf("$%d", len(args)))
	}
	// An exact retry is found through ordinary issue-specific source move facts,
	// before requesting a live body that normal admission deliberately excludes.
	expression := "json_extract(e.payload,'$.request_uid')"
	if postgres {
		expression = "e.payload::jsonb->>'request_uid'"
	}
	rows, err := tx.QueryContext(ctx, `SELECT e.issue_uid,e.payload FROM events e WHERE e.type='issue.moved' AND `+expression+`=$1 AND e.project_id IN (SELECT id FROM projects WHERE uid IN (`+strings.Join(placeholders, ",")+`)) ORDER BY e.id`, args...)
	if err != nil {
		return result, err
	}
	type retained struct{ uid, payload string }
	var prior []retained
	for rows.Next() {
		var v retained
		if err := rows.Scan(&v.uid, &v.payload); err != nil {
			_ = rows.Close()
			return result, err
		}
		prior = append(prior, v)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return result, err
	}
	seriesFinalized, err := lookupFinalizedMoveSeries(ctx, tx, store.InstanceUID(), r, preview, seriesRoots)
	if err != nil {
		return MoveTreeFailureResult(r, err), err
	}
	if len(prior) > 0 || len(roots) == 0 && seriesFinalized {
		if len(seriesRoots) > 0 && !seriesFinalized {
			return MoveTreeFailureResult(r, ErrMoveConflict), ErrMoveConflict
		}
		for _, project := range projects {
			if err := CheckProjectAccessTransaction(ctx, tx, project); err != nil {
				return result, err
			}
		}
		for _, v := range prior {
			var fact struct {
				SnapshotDigest string `json:"snapshot_digest"`
				EventUID       string `json:"received_event_uid"`
				EventHash      string `json:"received_event_hash"`
			}
			if err := json.Unmarshal([]byte(v.payload), &fact); err != nil {
				return result, err
			}
			if fact.SnapshotDigest != preview.SnapshotDigest {
				return MoveTreeFailureResult(r, ErrMoveConflict), ErrMoveConflict
			}
			matched := false
			for _, applied := range accepted.Applied {
				if applied.IssueUID == v.uid && applied.CommittedEventUID == fact.EventUID && applied.CommittedEventHash == fact.EventHash {
					matched = true
				}
			}
			if !matched {
				return MoveTreeFailureResult(r, ErrMoveConflict), ErrMoveConflict
			}
			for _, project := range projects {
				if err := CheckProjectAccessTransaction(ctx, tx, project); err != nil {
					return result, err
				}
			}
			result.Finalized = append(result.Finalized, v.uid)
		}
		if len(result.Finalized) == len(result.Applied) {
			result.State = "completed"
			result.RetryAction = ""
		} else {
			result.State = "partial"
		}
		return result, nil
	}
	snapshot, err := BuildIssueTreeSnapshot(ctx, tx, store, r, postgres)
	if err != nil {
		return result, err
	}
	acceptedDigest := preview.Digest
	if acceptedDigest == "" {
		acceptedDigest = preview.SnapshotDigest
	}
	if preview.SnapshotDigest == "" || snapshot.Digest != preview.SnapshotDigest || accepted.SnapshotDigest != acceptedDigest {
		return MoveTreeFailureResult(r, ErrMoveConflict), ErrMoveConflict
	}
	requestDigest, err := MoveRequestDigest(r)
	if err != nil {
		return result, err
	}
	for _, portion := range snapshot.Portions {
		for _, member := range portion.Members {
			var applied *MoveTreeApplied
			for i := range accepted.Applied {
				a := &accepted.Applied[i]
				if a.IssueUID == member.IssueUID && a.FromAuthorityUID == store.InstanceUID() && a.FromProjectUID == member.ProjectUID && a.FromShortID == member.ShortID {
					applied = a
					break
				}
			}
			if applied == nil || applied.LocationGeneration != member.LocationGeneration+1 {
				return MoveTreeFailureResult(r, ErrMoveConflict), ErrMoveConflict
			}
			var issueID, projectID int64
			var projectName string
			if err := tx.QueryRowContext(ctx, `SELECT i.id,i.project_id,p.name FROM issues i JOIN projects p ON p.id=i.project_id WHERE i.uid=$1 AND i.moved_to_authority_uid IS NULL`, member.IssueUID).Scan(&issueID, &projectID, &projectName); err != nil {
				return result, err
			}
			fact := map[string]any{"request_uid": r.RequestUID, "request_digest": requestDigest, "snapshot_digest": snapshot.Digest, "accepted_snapshot_digest": accepted.SnapshotDigest, "issue_uid": member.IssueUID, "from_authority_uid": store.InstanceUID(), "from_project_uid": member.ProjectUID, "from_short_id": member.ShortID, "to_authority_uid": r.DestinationAuthorityUID, "to_project_uid": r.DestinationProjectUID, "to_short_id": applied.ShortID, "location_generation": applied.LocationGeneration, "received_event_uid": applied.CommittedEventUID, "received_event_hash": applied.CommittedEventHash, "actor_authority_uid": r.ActorAuthorityUID, "provider_disposition": r.ProviderDisposition}
			payload, err := json.Marshal(fact, json.Deterministic(true))
			if err != nil {
				return result, err
			}
			event, err := appendEvent(ctx, tx, projectID, member.ProjectUID, projectName, member.IssueUID, issueID, string(payload), r.Actor)
			if err != nil {
				return result, err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE issues SET moved_to_authority_uid=$1,moved_to_project_uid=$2,moved_to_short_id=$3,moved_event_uid=$4,location_generation=$5,revision=revision+1 WHERE id=$6`, r.DestinationAuthorityUID, r.DestinationProjectUID, applied.ShortID, event.UID, applied.LocationGeneration, issueID); err != nil {
				return result, err
			}
			// Keep actual external identity owners for source polling suppression.
			if _, err := tx.ExecContext(ctx, `UPDATE import_mappings SET moved_issue_uid=$1,moved_to_authority_uid=$2,moved_to_project_uid=$3 WHERE issue_id=$4 OR comment_id IN (SELECT id FROM comments WHERE issue_id=$4) OR link_id IN (SELECT id FROM links WHERE from_issue_id=$4 OR to_issue_id=$4)`, member.IssueUID, r.DestinationAuthorityUID, r.DestinationProjectUID, issueID); err != nil {
				return result, err
			}
			result.Events = append(result.Events, event)
			result.Finalized = append(result.Finalized, member.IssueUID)
		}
	}
	for _, portion := range snapshot.Portions {
		for _, series := range portion.Recurrences {
			var sourceProjectUID, sourceProjectName string
			if err := tx.QueryRowContext(ctx, `SELECT uid,name FROM projects WHERE id=$1`, series.ProjectID).Scan(&sourceProjectUID, &sourceProjectName); err != nil {
				return result, err
			}
			payload, err := json.Marshal(map[string]any{"request_uid": r.RequestUID, "request_digest": requestDigest, "snapshot_digest": snapshot.Digest, "recurrence_uid": series.UID, "from_authority_uid": store.InstanceUID(), "from_project_uid": sourceProjectUID, "to_authority_uid": r.DestinationAuthorityUID, "to_project_uid": r.DestinationProjectUID, "actor_authority_uid": r.ActorAuthorityUID}, json.Deterministic(true))
			if err != nil {
				return result, err
			}
			event, err := appendEvent(ctx, tx, series.ProjectID, sourceProjectUID, sourceProjectName, "", 0, string(payload), r.Actor)
			if err != nil {
				return result, err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE recurrences SET moved_to_authority_uid=$1,moved_to_project_uid=$2,moved_event_uid=$3,location_generation=location_generation+1,revision=revision+1 WHERE uid=$4`, r.DestinationAuthorityUID, r.DestinationProjectUID, event.UID, series.UID); err != nil {
				return result, err
			}
			result.Events = append(result.Events, event)
		}
	}
	if len(result.Finalized) == len(result.Applied) {
		result.State = "completed"
		result.RetryAction = ""
	} else {
		result.State = "partial"
		result.RetryAction = "finalize the remaining source authorities with this request UID"
	}
	return result, nil
}

// Scheduler-only portions have no issue.moved row. Resolve their ordinary
// recurrence fact under the same native locks without exposing a retained body.
func lookupFinalizedMoveSeries(ctx context.Context, q MoveTreeSQL, instance string, r MoveTreeRequest, preview MoveTreePreview, roots []MoveTreeSeriesRoot) (bool, error) {
	if len(roots) == 0 {
		return false, nil
	}
	digest, err := MoveRequestDigest(r)
	if err != nil {
		return false, err
	}
	for _, root := range roots {
		if err := CheckProjectAccessTransaction(ctx, q, root.ProjectUID); err != nil {
			return false, err
		}
		var event, authority, project *string
		var payload sql.NullString
		if err := q.QueryRowContext(ctx, `SELECT r.moved_event_uid,r.moved_to_authority_uid,r.moved_to_project_uid,e.payload FROM recurrences r JOIN projects p ON p.id=r.project_id LEFT JOIN events e ON e.uid=r.moved_event_uid AND e.type='recurrence.moved' WHERE r.uid=$1 AND p.uid=$2`, root.RecurrenceUID, root.ProjectUID).Scan(&event, &authority, &project, &payload); err != nil {
			return false, err
		}
		if event == nil {
			return false, nil
		}
		if !payload.Valid || authority == nil || project == nil || *authority != r.DestinationAuthorityUID || *project != r.DestinationProjectUID {
			return false, ErrMoveConflict
		}
		var fact struct {
			RequestUID       string `json:"request_uid"`
			RequestDigest    string `json:"request_digest"`
			SnapshotDigest   string `json:"snapshot_digest"`
			RecurrenceUID    string `json:"recurrence_uid"`
			FromAuthorityUID string `json:"from_authority_uid"`
			FromProjectUID   string `json:"from_project_uid"`
			ToAuthorityUID   string `json:"to_authority_uid"`
			ToProjectUID     string `json:"to_project_uid"`
		}
		if err := json.Unmarshal([]byte(payload.String), &fact); err != nil {
			return false, err
		}
		if fact.RequestUID != r.RequestUID || fact.RequestDigest != digest || fact.SnapshotDigest != preview.SnapshotDigest || fact.RecurrenceUID != root.RecurrenceUID || fact.FromAuthorityUID != instance || fact.FromProjectUID != root.ProjectUID || fact.ToAuthorityUID != r.DestinationAuthorityUID || fact.ToProjectUID != r.DestinationProjectUID {
			return false, ErrMoveConflict
		}
	}
	return true, nil
}

func MoveFinalizationUnknown(r MoveTreeRequest, err error) (MoveTreeResult, error) {
	return MoveTreeResult{State: "outcome-unknown", RequestUID: r.RequestUID, UnknownRoots: []string{r.SourceAuthorityUID}, RetryAction: fmt.Sprintf("look up source move facts for request %s before retrying", r.RequestUID)}, err
}
