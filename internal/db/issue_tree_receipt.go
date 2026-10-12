package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// IssueTreeOwnerDigest includes every mutable selected owner; comment and
// relationship writes do not have to advance issues.revision to invalidate it.
func IssueTreeOwnerDigest(ctx context.Context, q MoveTreeSQL, issueUID, requestUID string, postgres bool) (string, error) {
	hash := sha256.New()
	write := func(v any) error {
		raw, err := json.Marshal(v, json.Deterministic(true))
		if err != nil {
			return err
		}
		_, err = hash.Write(append(raw, '\n'))
		return err
	}
	requestExpression := "json_extract(payload,'$.request_uid')"
	if postgres {
		requestExpression = "payload::jsonb->>'request_uid'"
	}
	queries := []struct{ name, sql string }{
		{"issue", `SELECT * FROM issues WHERE uid=$1`},
		{"comments", `SELECT c.* FROM comments c JOIN issues i ON i.id=c.issue_id WHERE i.uid=$1 ORDER BY c.uid`},
		{"labels", `SELECT l.* FROM issue_labels l JOIN issues i ON i.id=l.issue_id WHERE i.uid=$1 ORDER BY l.label`},
		{"relations", `SELECT * FROM links WHERE from_issue_uid=$1 OR to_issue_uid=$1 ORDER BY id`},
		{"history", `SELECT uid,content_hash,source_project_uid,source_event_bytes FROM events WHERE (issue_uid=$1 OR related_issue_uid=$1) AND COALESCE(` + requestExpression + `, '')<>$2 ORDER BY uid`},
		{"series", `SELECT r.* FROM recurrences r JOIN issues i ON i.recurrence_id=r.id WHERE i.uid=$1`},
		{"claims", `SELECT * FROM issue_claims WHERE issue_uid=$1 ORDER BY claim_uid`},
		{"pending_claims", `SELECT * FROM pending_claim_requests WHERE issue_uid=$1 ORDER BY request_uid`},
		{"mappings", `SELECT m.* FROM import_mappings m WHERE m.issue_id=(SELECT id FROM issues WHERE uid=$1) OR m.comment_id IN (SELECT c.id FROM comments c JOIN issues i ON i.id=c.issue_id WHERE i.uid=$1) OR m.link_id IN (SELECT id FROM links WHERE from_issue_uid=$1 OR to_issue_uid=$1) ORDER BY m.id`},
		{"external_bindings", `SELECT b.* FROM external_root_bindings b JOIN issues i ON i.id=b.issue_id WHERE i.uid=$1 ORDER BY b.uid`},
		{"external_states", `SELECT s.* FROM external_field_states s JOIN external_root_bindings b ON b.id=s.binding_id JOIN issues i ON i.id=b.issue_id WHERE i.uid=$1 ORDER BY s.binding_id,s.mapping_id`},
		{"external_fields", `SELECT m.* FROM external_field_mappings m JOIN external_field_states s ON s.mapping_id=m.id JOIN external_root_bindings b ON b.id=s.binding_id JOIN issues i ON i.id=b.issue_id WHERE i.uid=$1 ORDER BY m.id`},
		{"artifacts", `SELECT * FROM federation_embedding_artifacts WHERE issue_uid=$1 AND staging_expires_at IS NULL ORDER BY project_uid,digest`},
	}
	for _, query := range queries {
		if err := write(query.name); err != nil {
			return "", err
		}
		args := []any{issueUID}
		if strings.Contains(query.sql, "$2") {
			args = append(args, requestUID)
		}
		rows, err := q.QueryContext(ctx, query.sql, args...)
		if err != nil {
			return "", fmt.Errorf("move owner digest %s: %w", query.name, err)
		}
		columns, err := rows.Columns()
		if err != nil {
			_ = rows.Close()
			return "", err
		}
		if err := write(columns); err != nil {
			_ = rows.Close()
			return "", err
		}
		for rows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for i := range values {
				pointers[i] = &values[i]
			}
			if err := rows.Scan(pointers...); err != nil {
				_ = rows.Close()
				return "", err
			}
			if err := write(values); err != nil {
				_ = rows.Close()
				return "", err
			}
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// LookupIssueTreeAcceptanceSQL uses indexed ordinary per-issue facts and checks
// current complete owners before returning exact retry evidence.
func LookupIssueTreeAcceptanceSQL(ctx context.Context, q MoveTreeSQL, r MoveTreeRequest, snapshotDigest, instanceUID string, postgres bool) (MoveTreeResult, bool, error) {
	result := MoveTreeResult{State: "partial", RequestUID: r.RequestUID, RetryAction: "verify this destination commit and finalize every source authority"}
	p, err := AuthorizeMoveDestination(ctx, q, r, instanceUID)
	if err != nil {
		return result, false, err
	}
	expression := "json_extract(e.payload,'$.request_uid')"
	if postgres {
		expression = "e.payload::jsonb->>'request_uid'"
	}
	rows, err := q.QueryContext(ctx, `SELECT e.uid,e.content_hash,e.issue_uid,e.payload FROM events e WHERE e.project_id=$1 AND e.type='issue.move_received' AND `+expression+`=$2 ORDER BY e.id LIMIT 1000001`, p.ID, r.RequestUID)
	if err != nil {
		return result, false, err
	}
	type retainedFact struct{ eventUID, eventHash, issueUID, payload string }
	var facts []retainedFact
	for rows.Next() {
		var fact retainedFact
		if err := rows.Scan(&fact.eventUID, &fact.eventHash, &fact.issueUID, &fact.payload); err != nil {
			_ = rows.Close()
			return result, false, err
		}
		facts = append(facts, fact)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return result, false, err
	}
	if len(facts) == 0 {
		return result, false, nil
	}
	if len(facts) > 1000000 {
		return result, false, fmt.Errorf("acceptance exceeds lookup member limit")
	}
	digest, err := MoveRequestDigest(r)
	if err != nil {
		return result, false, err
	}
	for _, stored := range facts {
		var fact struct {
			RequestDigest              string `json:"request_digest"`
			SnapshotDigest             string `json:"snapshot_digest"`
			ReceivedIssueDigest        string `json:"received_issue_digest"`
			SourceSnapshotDigest       string `json:"source_snapshot_digest"`
			SourceSeriesUID            string `json:"source_series_uid"`
			SourceSeriesProjectUID     string `json:"source_series_project_uid"`
			SourceSeriesAuthorityUID   string `json:"source_series_authority_uid"`
			SourceSeriesSnapshotDigest string `json:"source_series_snapshot_digest"`
			FromAuthorityUID           string `json:"from_authority_uid"`
			FromProjectUID             string `json:"from_project_uid"`
			FromShortID                string `json:"from_short_id"`
			ShortID                    string `json:"to_short_id"`
			Revision                   int64  `json:"revision"`
			LocationGeneration         int64  `json:"location_generation"`
		}
		if err := json.Unmarshal([]byte(stored.payload), &fact); err != nil {
			return result, false, err
		}
		if result.SnapshotDigest != "" && result.SnapshotDigest != fact.SnapshotDigest {
			return moveConflictResult(r), false, ErrMoveConflict
		}
		result.SnapshotDigest = fact.SnapshotDigest
		if fact.RequestDigest != digest || snapshotDigest != "" && fact.SnapshotDigest != snapshotDigest {
			return moveConflictResult(r), false, ErrMoveConflict
		}
		current, err := IssueTreeOwnerDigest(ctx, q, stored.issueUID, r.RequestUID, postgres)
		if err != nil {
			return result, false, err
		}
		if current != fact.ReceivedIssueDigest {
			return moveConflictResult(r), false, ErrMoveConflict
		}
		if fact.SourceSnapshotDigest == "" {
			fact.SourceSnapshotDigest = fact.SnapshotDigest
		}
		result.Applied = append(result.Applied, MoveTreeApplied{SourceSnapshotDigest: fact.SourceSnapshotDigest, SourceSeriesUID: fact.SourceSeriesUID, SourceSeriesProjectUID: fact.SourceSeriesProjectUID, SourceSeriesAuthorityUID: fact.SourceSeriesAuthorityUID, SourceSeriesSnapshotDigest: fact.SourceSeriesSnapshotDigest, IssueUID: stored.issueUID, FromAuthorityUID: fact.FromAuthorityUID, FromProjectUID: fact.FromProjectUID, FromShortID: fact.FromShortID, AuthorityUID: instanceUID, ProjectUID: p.UID, ShortID: fact.ShortID, Revision: fact.Revision, LocationGeneration: fact.LocationGeneration, CommittedEventUID: stored.eventUID, CommittedEventHash: stored.eventHash})
	}
	return result, true, nil
}

// LookupIssueTreeSourceReceiptSQL uses UID-indexed routing and event identity;
// it never exposes the finalized issue projection or scans unrelated history.
func LookupIssueTreeSourceReceiptSQL(ctx context.Context, q MoveTreeSQL, r MoveTreeRequest, instanceUID string) (MoveTreeSourceReceipt, error) {
	if r.SourceAuthorityUID != instanceUID {
		return MoveTreeSourceReceipt{}, ErrMoveAuthorityMismatch
	}
	if admitted, restricted := AuthorizedProjects(ctx); restricted && !slices.Contains(admitted, r.SourceProjectUID) {
		return MoveTreeSourceReceipt{}, ErrNotFound
	}
	if err := CheckProjectAccessTransaction(ctx, q, r.SourceProjectUID); err != nil {
		return MoveTreeSourceReceipt{}, err
	}
	var receipt MoveTreeSourceReceipt
	var payload string
	err := q.QueryRowContext(ctx, `SELECT i.uid,i.moved_to_authority_uid,i.moved_to_project_uid,i.moved_to_short_id,i.location_generation,e.payload FROM issues i JOIN projects p ON p.id=i.project_id JOIN events e ON e.uid=i.moved_event_uid WHERE i.uid=$1 AND p.uid=$2 AND p.deleted_at IS NULL AND i.moved_to_authority_uid IS NOT NULL AND e.type='issue.moved'`, r.IssueUID, r.SourceProjectUID).Scan(&receipt.Route.IssueUID, &receipt.Route.AuthorityUID, &receipt.Route.ProjectUID, &receipt.Route.ShortID, &receipt.Route.LocationGeneration, &payload)
	if errors.Is(err, sql.ErrNoRows) {
		return receipt, ErrNotFound
	}
	if err != nil {
		return receipt, err
	}
	var fact struct {
		RequestUID     string `json:"request_uid"`
		RequestDigest  string `json:"request_digest"`
		SnapshotDigest string `json:"snapshot_digest"`
	}
	if err = json.Unmarshal([]byte(payload), &fact); err != nil {
		return receipt, err
	}
	digest, err := MoveRequestDigest(r)
	if err != nil {
		return receipt, err
	}
	if fact.RequestUID != r.RequestUID || fact.RequestDigest != digest || fact.SnapshotDigest == "" || receipt.Route.AuthorityUID != r.DestinationAuthorityUID || receipt.Route.ProjectUID != r.DestinationProjectUID {
		return receipt, ErrMoveConflict
	}
	receipt.RequestUID = r.RequestUID
	receipt.SnapshotDigest = fact.SnapshotDigest
	return receipt, nil
}
