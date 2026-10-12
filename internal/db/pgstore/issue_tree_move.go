package pgstore

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"slices"

	"go.kenn.io/kata/internal/db"
)

// MoveIssueTreeLocal rehomes the authorized child closure in one native transaction.
func (s *Store) MoveIssueTreeLocal(ctx context.Context, input db.MoveIssueTreeIn) (db.MoveIssueTreeOut, error) {
	var output db.MoveIssueTreeOut
	err := s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		var attempt db.MoveIssueTreeOut
		if err := lockProjectAccessExclusive(ctx, tx); err != nil {
			return err
		}
		if retained, found, err := s.localMoveRetry(ctx, tx, input); err != nil {
			return err
		} else if found {
			output = retained
			return nil
		}
		rows, err := tx.QueryContext(ctx, `WITH RECURSIVE edges(from_kind,from_id,to_kind,to_id) AS (
 SELECT 'issue',to_issue_id,'issue',from_issue_id FROM links WHERE type='parent' AND NOT $3 AND from_issue_id IS NOT NULL AND to_issue_id IS NOT NULL
 UNION ALL SELECT 'issue',id,'series',recurrence_id FROM issues WHERE $4 AND recurrence_id IS NOT NULL
 UNION ALL SELECT 'series',recurrence_id,'issue',id FROM issues WHERE $4 AND recurrence_id IS NOT NULL
), selection(kind,id) AS (
 SELECT 'issue',id FROM issues WHERE id=$1 AND (project_id=$2 OR project_id=$5)
 UNION SELECT e.to_kind,e.to_id FROM selection s JOIN edges e ON e.from_kind=s.kind AND e.from_id=s.id
)
SELECT i.id,i.project_id,i.revision,p.uid FROM selection s JOIN issues i ON s.kind='issue' AND i.id=s.id JOIN projects p ON p.id=i.project_id ORDER BY i.id`, input.IssueID, input.FromProjectID, input.IssueOnly, input.MoveSeries, input.ToProjectID)
		if err != nil {
			return mapSQLError(err, nil)
		}
		type selectedIssue struct {
			id, projectID, revision int64
			projectUID              string
		}
		var selected []selectedIssue
		for rows.Next() {
			var issue selectedIssue
			if err := rows.Scan(&issue.id, &issue.projectID, &issue.revision, &issue.projectUID); err != nil {
				_ = rows.Close()
				return err
			}
			selected = append(selected, issue)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		if len(selected) == 0 {
			return db.ErrNotFound
		}
		destination, err := scanProject(tx.QueryRowContext(ctx, projectSelect+` WHERE id=$1 AND deleted_at IS NULL FOR SHARE`, input.ToProjectID))
		if err != nil {
			return err
		}
		admitted, restricted := db.AuthorizedProjects(ctx)
		if restricted && !slices.Contains(admitted, destination.UID) {
			return db.ErrNotFound
		}
		if err := db.CheckProjectAccessTransaction(ctx, tx, destination.UID); err != nil {
			return err
		}
		for _, issue := range selected {
			if restricted && !slices.Contains(admitted, issue.projectUID) {
				return db.ErrNotFound
			}
			if err := db.CheckProjectAccessTransaction(ctx, tx, issue.projectUID); err != nil {
				return err
			}
		}
		memberIDs := make([]int64, len(selected))
		for i, member := range selected {
			memberIDs[i] = member.id
		}
		if err := db.RequireLocalIssueTree(ctx, tx, memberIDs, input.MoveSeries, s.instanceUID); err != nil {
			return err
		}
		var selectedSeries []db.LocalMoveSeries
		if input.MoveSeries {
			selectedSeries, err = db.SelectedLocalMoveSeries(ctx, tx, memberIDs)
			if err != nil {
				return err
			}
			for _, series := range selectedSeries {
				if restricted && !slices.Contains(admitted, series.FromProjectUID) {
					return db.ErrNotFound
				}
				if err := db.CheckProjectAccessTransaction(ctx, tx, series.FromProjectUID); err != nil {
					return err
				}
				if err := ensureFederatedMoveAllowedTx(ctx, tx, series.FromProjectID, input.ToProjectID); err != nil {
					return err
				}
				if err := ensureProjectWritableTx(ctx, tx, series.FromProjectID); err != nil {
					return err
				}
			}
		}
		digest, err := db.IssueTreePreviewDigest(ctx, tx, input, memberIDs, s.instanceUID)
		if err != nil {
			return err
		}
		if input.PreviewDigest != "" && input.PreviewDigest != digest {
			return db.ErrMovePreviewStale
		}
		attempt.PreviewDigest = digest
		for _, issue := range selected {
			if issue.projectID == input.ToProjectID {
				retained, _, err := lockedIssueTx(ctx, tx, issue.id, true)
				if err != nil {
					return err
				}
				attempt.Issues = append(attempt.Issues, retained)
				continue
			}
			member := input.MoveIssueProjectIn
			member.IssueID, member.FromProjectID, member.IfMatchRev = issue.id, issue.projectID, issue.revision
			if issue.id == input.IssueID {
				member.IfMatchRev = input.IfMatchRev
			}
			moved, err := s.moveIssueProjectTx(ctx, tx, member, true, true, true)
			if err != nil {
				return err
			}
			attempt.Issues = append(attempt.Issues, moved.Issue)
			if !input.DryRun {
				attempt.Events = append(attempt.Events, *moved.Event)
			}
			if issue.id == input.IssueID {
				attempt.Root = moved
			}
			attempt.Root.ProjectAccessRevision = moved.ProjectAccessRevision
		}
		for _, series := range selectedSeries {
			if series.FromProjectID == input.ToProjectID {
				continue
			}
			if err := db.RehomeLocalMoveSeries(ctx, tx, series, input.ToProjectID); err != nil {
				return err
			}
			payload, err := json.Marshal(map[string]any{"recurrence_uid": series.UID, "from_project_uid": series.FromProjectUID, "to_project_uid": destination.UID})
			if err != nil {
				return err
			}
			event, err := s.insertEventTx(ctx, tx, eventInsert{ProjectID: series.FromProjectID, ProjectUID: series.FromProjectUID, Type: "recurrence.moved", Actor: input.Actor, Payload: string(payload)})
			if err != nil {
				return err
			}
			if !input.DryRun {
				attempt.Events = append(attempt.Events, event)
			}
		}
		output = attempt
		if input.DryRun {
			return errMovePreview
		}
		return nil
	})
	if errors.Is(err, errMovePreview) {
		return output, nil
	}
	if err != nil {
		return db.MoveIssueTreeOut{}, err
	}
	return output, nil
}
