package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
)

type treePreviewSQL interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// IssueTreePreviewDigest hashes existing scoped rows inside the caller's native
// transaction. The digest is ephemeral request data, never a persisted manifest.
func IssueTreePreviewDigest(ctx context.Context, q treePreviewSQL, in MoveIssueTreeIn, memberIDs []int64, instanceUID string) (string, error) {
	h := sha256.New()
	write := func(value any) error {
		raw, err := json.Marshal(value)
		if err != nil {
			return err
		}
		_, err = h.Write(append(raw, '\n'))
		return err
	}
	if err := write(in.MoveSeries); err != nil {
		return "", err
	}
	if err := write(struct {
		Instance, Actor string
		IssueOnly       bool
	}{instanceUID, in.Actor, in.IssueOnly}); err != nil {
		return "", err
	}
	hashRows := func(tag, query string, args ...any) error {
		if err := write(tag); err != nil {
			return err
		}
		rows, err := q.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		columns, err := rows.Columns()
		if err != nil {
			return err
		}
		if err := write(columns); err != nil {
			return err
		}
		for rows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for i := range values {
				pointers[i] = &values[i]
			}
			if err := rows.Scan(pointers...); err != nil {
				return err
			}
			if err := write(values); err != nil {
				return err
			}
		}
		return rows.Err()
	}
	if err := hashRows("destination", `SELECT uid,name,deleted_at FROM projects WHERE id=$1`, in.ToProjectID); err != nil {
		return "", err
	}
	if err := hashRows("access", `SELECT value FROM meta WHERE key='project_access_revision'`); err != nil {
		return "", err
	}
	for _, id := range memberIDs {
		queries := []struct{ tag, sql string }{
			{"issue", `SELECT i.*,p.uid AS current_project_uid,p.name AS current_project_name FROM issues i JOIN projects p ON p.id=i.project_id WHERE i.id=$1`},
			{"series", `SELECT r.* FROM recurrences r JOIN issues i ON i.recurrence_id=r.id WHERE i.id=$1`},
			{"comments", `SELECT * FROM comments WHERE issue_id=$1 ORDER BY uid`},
			{"labels", `SELECT * FROM issue_labels WHERE issue_id=$1 ORDER BY label`},
			{"relations", `SELECT * FROM links WHERE from_issue_id=$1 OR to_issue_id=$1 ORDER BY id`},
			{"history", `SELECT uid,content_hash,source_project_uid FROM events WHERE issue_uid=(SELECT uid FROM issues WHERE id=$1) OR related_issue_uid=(SELECT uid FROM issues WHERE id=$1) ORDER BY uid`},
			{"claims", `SELECT * FROM issue_claims WHERE issue_id=$1 ORDER BY claim_uid`},
			{"pending_claims", `SELECT * FROM pending_claim_requests WHERE issue_id=$1 ORDER BY id`},
			{"provider_mappings", `SELECT * FROM import_mappings WHERE issue_id=$1 ORDER BY id`},
		}
		for _, query := range queries {
			if err := hashRows(query.tag, query.sql, id); err != nil {
				return "", fmt.Errorf("preview %s: %w", query.tag, err)
			}
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ErrMovePreviewStale requires a fresh preview before any selected owner moves.
var ErrMovePreviewStale = fmt.Errorf("move preview is stale; preview the complete tree again")
