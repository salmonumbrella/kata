package pgstore

import (
	"context"
	"database/sql"
	"go.kenn.io/kata/internal/db"
)

func (s *Store) FinalizeIssueTree(ctx context.Context, request db.MoveTreeRequest, preview db.MoveTreePreview, accepted db.MoveTreeResult) (db.MoveTreeResult, error) {
	r, err := request.Normalize()
	if err != nil {
		return db.MoveTreeResult{}, err
	}
	if err := db.VerifyMoveDestinationCommit(ctx, r, accepted); err != nil {
		return db.MoveTreeFailureResult(r, err), err
	}
	appendSource := func(ctx context.Context, tx *sql.Tx, projectID int64, projectUID, projectName, issueUID string, issueID int64, payload, actor string) (db.Event, error) {
		if issueID == 0 {
			return s.insertEventTx(ctx, tx, eventInsert{ProjectID: projectID, ProjectUID: projectUID, ProjectName: projectName, Type: "recurrence.moved", Actor: actor, Payload: payload})
		}
		return s.insertEventTx(ctx, tx, eventInsert{ProjectID: projectID, ProjectUID: projectUID, ProjectName: projectName, IssueID: &issueID, IssueUID: &issueUID, Type: "issue.moved", Actor: actor, Payload: payload})
	}
	var result db.MoveTreeResult
	committing := false
	err = s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		committing = false
		if err := lockProjectAccessExclusive(ctx, tx); err != nil {
			return err
		}
		scoped := *s
		scoped.exportQ = tx
		var err error
		result, err = db.FinalizeIssueTreeSQL(ctx, tx, &scoped, r, preview, accepted, appendSource, true)
		if err == nil {
			committing = true
		}
		return err
	})
	if err != nil && committing {
		return db.MoveFinalizationUnknown(r, err)
	}
	if err != nil {
		return db.UncommittedIssueTreeFinalization(result, accepted), err
	}
	return result, nil
}
