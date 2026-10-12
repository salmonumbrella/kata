package pgstore

import (
	"context"
	"database/sql"
	"errors"
	"go.kenn.io/kata/internal/db"
)

func (s *Store) localMoveInput(ctx context.Context, request db.MoveTreeRequest) (db.MoveIssueTreeIn, error) {
	r, err := request.Normalize()
	if err != nil {
		return db.MoveIssueTreeIn{}, err
	}
	if r.SourceAuthorityUID != s.InstanceUID() || r.DestinationAuthorityUID != s.InstanceUID() {
		return db.MoveIssueTreeIn{}, db.ErrMoveAuthorityMismatch
	}
	var issueID, projectID, revision int64
	if err := s.QueryRowContext(ctx, `SELECT i.id,i.project_id,i.revision FROM issues i JOIN projects p ON p.id=i.project_id WHERE i.uid=$1 AND p.uid IN ($2,$3) AND i.moved_to_authority_uid IS NULL`, r.IssueUID, r.SourceProjectUID, r.DestinationProjectUID).Scan(&issueID, &projectID, &revision); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return db.MoveIssueTreeIn{}, db.ErrNotFound
		}
		return db.MoveIssueTreeIn{}, err
	}
	source, err := s.ProjectByUID(ctx, r.SourceProjectUID)
	if err != nil {
		return db.MoveIssueTreeIn{}, err
	}
	destination, err := s.ProjectByUID(ctx, r.DestinationProjectUID)
	if err != nil {
		return db.MoveIssueTreeIn{}, err
	}
	digest, err := db.MoveRequestDigest(r)
	if err != nil {
		return db.MoveIssueTreeIn{}, err
	}
	return db.MoveIssueTreeIn{MoveIssueProjectIn: db.MoveIssueProjectIn{IssueID: issueID, FromProjectID: source.ID, ToProjectID: destination.ID, IfMatchRev: revision, Actor: r.Actor, RequestUID: r.RequestUID, RequestDigest: digest, ActorAuthorityUID: r.ActorAuthorityUID, ProviderDisposition: r.ProviderDisposition}, IssueOnly: r.Selection == "issue-only", MoveSeries: r.MoveSeries, PreviewDigest: r.PreviewDigest}, nil
}

func (s *Store) PreviewIssueTree(ctx context.Context, request db.MoveTreeRequest) (db.MoveTreePreview, error) {
	input, err := s.localMoveInput(ctx, request)
	if err != nil {
		return db.MoveTreePreview{}, err
	}
	input.DryRun = true
	input.PreviewDigest = ""
	out, err := s.MoveIssueTreeLocal(ctx, input)
	if err != nil {
		return db.MoveTreePreview{}, err
	}
	preview := db.MoveTreePreview{Digest: out.PreviewDigest, SnapshotDigest: out.PreviewDigest, SourceRevisions: map[string]int64{}}
	for _, issue := range out.Issues {
		project, err := s.ProjectByID(ctx, issue.ProjectID)
		if err != nil {
			return db.MoveTreePreview{}, err
		}
		preview.Members = append(preview.Members, db.MoveTreeMember{IssueUID: issue.UID, AuthorityUID: request.SourceAuthorityUID, ProjectUID: project.UID, ShortID: issue.ShortID, Revision: issue.Revision})
		preview.SourceRevisions[issue.UID] = issue.Revision
	}
	if err := db.PopulateLocalIssueTreePreview(ctx, s, request, &preview); err != nil {
		return db.MoveTreePreview{}, err
	}
	return preview, nil
}

func (s *Store) MoveIssueTree(ctx context.Context, request db.MoveTreeRequest) (db.MoveTreeResult, error) {
	r, err := request.Normalize()
	if err != nil {
		return db.MoveTreeResult{}, err
	}
	input, err := s.localMoveInput(ctx, r)
	if err != nil {
		return db.MoveTreeResult{}, err
	}
	out, err := s.MoveIssueTreeLocal(ctx, input)
	if err != nil {
		return db.MoveTreeFailureResult(r, err), err
	}
	return db.MoveTreeLocalResult(r, out)
}

func (s *Store) localMoveRetry(ctx context.Context, tx *sql.Tx, in db.MoveIssueTreeIn) (db.MoveIssueTreeOut, bool, error) {
	var out db.MoveIssueTreeOut
	if in.RequestUID == "" || in.DryRun {
		return out, false, nil
	}
	rows, err := tx.QueryContext(ctx, eventSelect+` WHERE e.project_id=$1 AND e.type='issue.moved' AND e.payload::jsonb->>'request_uid'=$2 ORDER BY e.id`, in.ToProjectID, in.RequestUID)
	if err != nil {
		return out, false, err
	}
	for rows.Next() {
		event, err := scanEvent(rows)
		if err != nil {
			_ = rows.Close()
			return out, false, err
		}
		out.Events = append(out.Events, event)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return out, false, err
	}
	for _, event := range out.Events {
		if err := db.ValidateMoveRetryFact(in.RequestDigest, event); err != nil {
			return db.MoveIssueTreeOut{}, false, err
		}
		if event.IssueID == nil {
			return db.MoveIssueTreeOut{}, false, db.ErrMoveConflict
		}
		issue, _, err := lockedIssueTx(ctx, tx, *event.IssueID, true)
		if err != nil {
			return db.MoveIssueTreeOut{}, false, err
		}
		if err := db.ValidateLocalMoveRetryIssue(event, issue); err != nil {
			return db.MoveIssueTreeOut{}, false, err
		}
		out.Issues = append(out.Issues, issue)
	}
	out.Replayed = len(out.Events) > 0
	return out, out.Replayed, nil
}

func (s *Store) ExportIssueTree(ctx context.Context, request db.MoveTreeRequest) (db.IssueTreeSnapshot, error) {
	r, err := request.Normalize()
	if err != nil {
		return db.IssueTreeSnapshot{}, err
	}
	tx, err := s.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return db.IssueTreeSnapshot{}, err
	}
	defer func() { _ = tx.Rollback() }()
	snapshotStore := *s
	snapshotStore.exportQ = tx
	return db.BuildIssueTreeSnapshot(ctx, tx, &snapshotStore, r, true)
}
