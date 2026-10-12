package sqlitestore

import (
	"context"
	"encoding/json/v2"
	"errors"
	"slices"

	"go.kenn.io/kata/internal/db"
)

// MoveIssueTreeLocal applies an authorized descendant selection in one transaction.
// It uses existing rows and locks; no durable membership state is introduced.
func (d *Store) MoveIssueTreeLocal(ctx context.Context, in db.MoveIssueTreeIn) (db.MoveIssueTreeOut, error) {
	return retryWrite1(ctx, d, func() (db.MoveIssueTreeOut, error) {
		var out db.MoveIssueTreeOut
		tx, err := d.BeginTx(ctx, nil)
		if err != nil {
			return out, err
		}
		defer func() { _ = tx.Rollback() }()
		if err := lockProjectAccess(ctx, tx); err != nil {
			return out, err
		}
		if retained, found, err := d.localMoveRetry(ctx, tx, in); err != nil {
			return out, err
		} else if found {
			return retained, tx.Commit()
		}
		rows, err := tx.QueryContext(ctx, `WITH RECURSIVE edges(from_kind,from_id,to_kind,to_id) AS (
 SELECT 'issue',to_issue_id,'issue',from_issue_id FROM links WHERE type='parent' AND NOT $3 AND from_issue_id IS NOT NULL AND to_issue_id IS NOT NULL
 UNION ALL SELECT 'issue',id,'series',recurrence_id FROM issues WHERE $4 AND recurrence_id IS NOT NULL
 UNION ALL SELECT 'series',recurrence_id,'issue',id FROM issues WHERE $4 AND recurrence_id IS NOT NULL
), selection(kind,id) AS (
 SELECT 'issue',id FROM issues WHERE id=$1 AND (project_id=$2 OR project_id=$5)
 UNION SELECT e.to_kind,e.to_id FROM selection s JOIN edges e ON e.from_kind=s.kind AND e.from_id=s.id
)
SELECT i.id,i.project_id,i.revision,p.uid FROM selection s JOIN issues i ON s.kind='issue' AND i.id=s.id JOIN projects p ON p.id=i.project_id ORDER BY i.id`, in.IssueID, in.FromProjectID, in.IssueOnly, in.MoveSeries, in.ToProjectID)
		if err != nil {
			return out, err
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
				return out, err
			}
			selected = append(selected, issue)
		}
		err = errors.Join(rows.Err(), rows.Close())
		if err != nil {
			return out, err
		}
		if len(selected) == 0 {
			return out, db.ErrNotFound
		}
		var destinationUID string
		if err := tx.QueryRowContext(ctx, `SELECT uid FROM projects WHERE id=? AND deleted_at IS NULL`, in.ToProjectID).Scan(&destinationUID); err != nil {
			return out, err
		}
		admitted, restricted := db.AuthorizedProjects(ctx)
		if restricted && !slices.Contains(admitted, destinationUID) {
			return out, db.ErrNotFound
		}
		if err := db.CheckProjectAccessTransaction(ctx, tx, destinationUID); err != nil {
			return out, err
		}
		for _, issue := range selected {
			if restricted && !slices.Contains(admitted, issue.projectUID) {
				return db.MoveIssueTreeOut{}, db.ErrNotFound
			}
			if err := db.CheckProjectAccessTransaction(ctx, tx, issue.projectUID); err != nil {
				return db.MoveIssueTreeOut{}, err
			}
		}
		memberIDs := make([]int64, len(selected))
		for i, member := range selected {
			memberIDs[i] = member.id
		}
		if err := db.RequireLocalIssueTree(ctx, tx, memberIDs, in.MoveSeries, d.instanceUID); err != nil {
			return db.MoveIssueTreeOut{}, err
		}
		var selectedSeries []db.LocalMoveSeries
		if in.MoveSeries {
			selectedSeries, err = db.SelectedLocalMoveSeries(ctx, tx, memberIDs)
			if err != nil {
				return db.MoveIssueTreeOut{}, err
			}
			for _, series := range selectedSeries {
				if restricted && !slices.Contains(admitted, series.FromProjectUID) {
					return db.MoveIssueTreeOut{}, db.ErrNotFound
				}
				if err := db.CheckProjectAccessTransaction(ctx, tx, series.FromProjectUID); err != nil {
					return db.MoveIssueTreeOut{}, err
				}
				if err := ensureFederatedMoveAllowedTx(ctx, tx, series.FromProjectID, in.ToProjectID); err != nil {
					return db.MoveIssueTreeOut{}, err
				}
				if err := ensureProjectWritableTx(ctx, tx, series.FromProjectID); err != nil {
					return db.MoveIssueTreeOut{}, err
				}
			}
		}
		digest, err := db.IssueTreePreviewDigest(ctx, tx, in, memberIDs, d.instanceUID)
		if err != nil {
			return db.MoveIssueTreeOut{}, err
		}
		if in.PreviewDigest != "" && in.PreviewDigest != digest {
			return db.MoveIssueTreeOut{}, db.ErrMovePreviewStale
		}
		out.PreviewDigest = digest
		for _, issue := range selected {
			if issue.projectID == in.ToProjectID {
				retained, err := issueByIDTx(ctx, tx, issue.id)
				if err != nil {
					return db.MoveIssueTreeOut{}, err
				}
				out.Issues = append(out.Issues, retained)
				continue
			}
			member := in.MoveIssueProjectIn
			member.IssueID, member.FromProjectID, member.IfMatchRev = issue.id, issue.projectID, issue.revision
			if issue.id == in.IssueID {
				member.IfMatchRev = in.IfMatchRev
			}
			moved, err := d.moveIssueProjectTx(ctx, tx, member, true, true, true)
			if err != nil {
				return db.MoveIssueTreeOut{}, err
			}
			out.Issues = append(out.Issues, moved.Issue)
			if !in.DryRun {
				out.Events = append(out.Events, *moved.Event)
			}
			if issue.id == in.IssueID {
				out.Root = moved
			}
			out.Root.ProjectAccessRevision = moved.ProjectAccessRevision
		}
		for _, series := range selectedSeries {
			if series.FromProjectID == in.ToProjectID {
				continue
			}
			if err := db.RehomeLocalMoveSeries(ctx, tx, series, in.ToProjectID); err != nil {
				return db.MoveIssueTreeOut{}, err
			}
			payload, err := json.Marshal(map[string]any{"recurrence_uid": series.UID, "from_project_uid": series.FromProjectUID, "to_project_uid": destinationUID})
			if err != nil {
				return db.MoveIssueTreeOut{}, err
			}
			event, err := d.insertEventTx(ctx, tx, eventInsert{ProjectID: series.FromProjectID, ProjectUID: series.FromProjectUID, Type: "recurrence.moved", Actor: in.Actor, Payload: string(payload)})
			if err != nil {
				return db.MoveIssueTreeOut{}, err
			}
			if !in.DryRun {
				out.Events = append(out.Events, event)
			}
		}
		if in.DryRun {
			return out, tx.Rollback()
		}
		return out, tx.Commit()
	})
}
