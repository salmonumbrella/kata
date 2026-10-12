package pgstore

import (
	"context"
	"database/sql"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/embedding"
)

func (s *Store) transferHooks() db.MoveTreeNativeHooks {
	return db.MoveTreeNativeHooks{
		AllocateShortID: func(ctx context.Context, tx *sql.Tx, projectID int64, issueUID string) (string, error) {
			return s.resolveShortIDTx(ctx, tx, projectID, issueUID, "")
		},
		AppendEvent: func(ctx context.Context, tx *sql.Tx, projectID int64, projectUID, projectName, issueUID string, issueID int64, payload, actor string) (db.Event, error) {
			return s.insertEventTx(ctx, tx, eventInsert{ProjectID: projectID, ProjectUID: projectUID, ProjectName: projectName, IssueUID: &issueUID, IssueID: &issueID, Type: "issue.move_received", Actor: actor, Payload: payload})
		},
		RestoreArtifact: func(ctx context.Context, tx *sql.Tx, artifact embedding.EmbeddingArtifact) error {
			return importEmbeddingArtifact(ctx, tx, &db.EmbeddingArtifactExport{EmbeddingArtifact: artifact})
		},
		RestoreExternal: func(ctx context.Context, tx *sql.Tx, portion db.IssueTreePortion, projectUID, disposition string) error {
			for _, m := range portion.ExternalMappings {
				if err := pgReplayExternalFieldMapping(ctx, tx, &m, true); err != nil {
					return err
				}
			}
			for _, binding := range portion.ExternalBindings {
				binding.ProjectUID = projectUID
				if disposition == "detach" {
					binding.Enabled = false
					binding.PausedReason = "detached by issue move"
				}
				if err := pgReplayExternalRootBinding(ctx, tx, &binding, true); err != nil {
					return err
				}
			}
			for _, state := range portion.ExternalStates {
				if err := pgReplayExternalFieldState(ctx, tx, &state); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

func (s *Store) AcceptIssueTree(ctx context.Context, snapshot db.IssueTreeSnapshot, request db.MoveTreeRequest) (db.MoveTreeResult, error) {
	r, err := request.Normalize()
	if err != nil {
		return db.MoveTreeResult{}, err
	}
	if err := db.ValidateIssueTreeSelection(snapshot, r); err != nil {
		return db.MoveTreeResult{State: "conflict", RequestUID: r.RequestUID, RetryAction: "refresh the complete authorized selection before retrying"}, err
	}
	var result db.MoveTreeResult
	committing := false
	err = s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		committing = false
		if err := lockProjectAccessExclusive(ctx, tx); err != nil {
			return err
		}
		var err error
		result, err = db.AcceptIssueTreeSQL(ctx, tx, snapshot, r, s.instanceUID, s.transferHooks(), true)
		committing = err == nil
		return err
	})
	if err != nil {
		if committing {
			return db.MoveTreeResult{State: "outcome-unknown", RequestUID: r.RequestUID, UnknownRoots: []string{s.instanceUID}, RetryAction: "look up destination acceptance before retrying"}, err
		}
		return db.UncommittedIssueTreeAcceptance(result), err
	}
	return result, nil
}

func (s *Store) LookupIssueTreeAcceptance(ctx context.Context, request db.MoveTreeRequest) (db.MoveTreeResult, error) {
	r, err := request.Normalize()
	if err != nil {
		return db.MoveTreeResult{}, err
	}
	tx, err := s.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return db.MoveTreeResult{}, err
	}
	defer func() { _ = tx.Rollback() }()
	result, found, err := db.LookupIssueTreeAcceptanceSQL(ctx, tx, r, "", s.instanceUID, true)
	if err != nil {
		return result, err
	}
	if !found {
		return db.MoveTreeResult{State: "outcome-unknown", RequestUID: r.RequestUID, UnknownRoots: []string{r.DestinationAuthorityUID}, RetryAction: "resend the exact original snapshot with the same request UID"}, db.ErrNotFound
	}
	return result, nil
}

func (s *Store) LookupIssueTreeSourceReceipt(ctx context.Context, request db.MoveTreeRequest) (db.MoveTreeSourceReceipt, error) {
	r, err := request.Normalize()
	if err != nil {
		return db.MoveTreeSourceReceipt{}, err
	}
	tx, err := s.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return db.MoveTreeSourceReceipt{}, err
	}
	defer func() { _ = tx.Rollback() }()
	return db.LookupIssueTreeSourceReceiptSQL(ctx, tx, r, s.instanceUID)
}
