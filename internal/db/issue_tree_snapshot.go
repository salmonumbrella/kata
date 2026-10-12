package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"iter"
	"slices"
	"strings"

	"go.kenn.io/kata/internal/embedding"
)

const MaxIssueTreeSnapshotBytes = 256 << 20

type MoveTreeProject struct {
	ID           int64  `json:"id"`
	UID          string `json:"uid"`
	AuthorityUID string `json:"authority_uid"`
	Name         string `json:"name"`
}

// IssueTreePortion is transient selected owner data from one actual authority.
// It contains no daemon settings, credentials, enrollment or transport state.
type IssueTreePortion struct {
	AuthorityUID         string                        `json:"authority_uid"`
	SourceSnapshotDigest string                        `json:"source_snapshot_digest,omitempty"`
	Projects             []MoveTreeProject             `json:"projects"`
	Members              []MoveTreeMember              `json:"members"`
	Issues               []IssueExport                 `json:"issues"`
	Comments             []CommentExport               `json:"comments"`
	Labels               []IssueLabelExport            `json:"labels"`
	Links                []LinkExport                  `json:"links"`
	Events               []EventExport                 `json:"events"`
	Recurrences          []RecurrenceExport            `json:"recurrences"`
	Tombstones           []PurgeLogExport              `json:"tombstones"`
	Mappings             []ImportMappingExport         `json:"mappings"`
	ExternalBindings     []ExternalRootBindingExport   `json:"external_bindings"`
	ExternalMappings     []ExternalFieldMappingExport  `json:"external_mappings"`
	ExternalStates       []ExternalFieldStateExport    `json:"external_states"`
	Claims               []IssueClaimExport            `json:"claims"`
	PendingClaims        []PendingClaimRequestExport   `json:"pending_claims"`
	CreationProofs       []EntityProvenance            `json:"creation_proofs"`
	Receipts             []AttributionReceipt          `json:"receipts"`
	PublicKeys           []RootKeyPin                  `json:"public_keys"`
	Artifacts            []embedding.EmbeddingArtifact `json:"artifacts"`
}

type IssueTreeSnapshot struct {
	Version    int                `json:"version"`
	RequestUID string             `json:"request_uid"`
	Digest     string             `json:"digest"`
	Portions   []IssueTreePortion `json:"portions"`
	// Required contains body-free child locators to collect before acceptance.
	Required       []MoveTreeMember     `json:"required"`
	RequiredSeries []MoveTreeSeriesRoot `json:"required_series,omitempty"`
}

func IssueTreeSnapshotDigest(snapshot IssueTreeSnapshot) (string, error) {
	snapshot.Digest = ""
	raw, err := json.Marshal(snapshot, json.Deterministic(true))
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func EncodeIssueTreeSnapshot(snapshot IssueTreeSnapshot) ([]byte, error) {
	if err := ValidateIssueTreeSnapshotDigest(snapshot); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(snapshot, json.Deterministic(true))
	if err != nil {
		return nil, err
	}
	if len(raw) > MaxIssueTreeSnapshotBytes {
		return nil, fmt.Errorf("issue tree exceeds snapshot byte limit")
	}
	return raw, nil
}

func DecodeIssueTreeSnapshot(reader io.Reader) (IssueTreeSnapshot, error) {
	raw, err := io.ReadAll(io.LimitReader(reader, MaxIssueTreeSnapshotBytes+1))
	if err != nil {
		return IssueTreeSnapshot{}, err
	}
	if len(raw) > MaxIssueTreeSnapshotBytes {
		return IssueTreeSnapshot{}, fmt.Errorf("issue tree exceeds snapshot byte limit")
	}
	var snapshot IssueTreeSnapshot
	if err := json.Unmarshal(raw, &snapshot, json.RejectUnknownMembers(true)); err != nil {
		return IssueTreeSnapshot{}, err
	}
	if err := ValidateIssueTreeSnapshotDigest(snapshot); err != nil {
		return IssueTreeSnapshot{}, err
	}
	return snapshot, nil
}

func ValidateIssueTreeSnapshotDigest(snapshot IssueTreeSnapshot) error {
	if snapshot.Version != 1 || len(snapshot.Portions) == 0 {
		return fmt.Errorf("invalid issue tree snapshot version or empty portions")
	}
	digest, err := IssueTreeSnapshotDigest(snapshot)
	if err != nil {
		return err
	}
	if snapshot.Digest != digest {
		return fmt.Errorf("issue tree snapshot digest mismatch")
	}
	return nil
}

type MoveTreeSQL interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

// SelectIssueTree resolves retained local rows rather than normal live readers.
// Authorization is checked for the complete local closure before returning data.
func SelectIssueTree(ctx context.Context, q MoveTreeSQL, r MoveTreeRequest, instanceUID string) ([]MoveTreeMember, []MoveTreeMember, error) {
	roots, err := IssueTreeSourceRoots(ctx, r, instanceUID)
	if err != nil {
		return nil, nil, err
	}
	seriesRoots, err := IssueTreeSourceSeries(ctx, r, instanceUID)
	if err != nil {
		return nil, nil, err
	}
	var rootIDs []int64
	for _, root := range roots {
		if admitted, restricted := AuthorizedProjects(ctx); restricted && !slices.Contains(admitted, root.ProjectUID) {
			return nil, nil, ErrNotFound
		}
		if err := CheckProjectAccessTransaction(ctx, q, root.ProjectUID); err != nil {
			return nil, nil, err
		}
		var rootID int64
		err := q.QueryRowContext(ctx, `SELECT i.id FROM issues i JOIN projects p ON p.id=i.project_id WHERE i.uid=$1 AND p.uid=$2 AND p.deleted_at IS NULL AND i.moved_to_authority_uid IS NULL`, root.IssueUID, root.ProjectUID).Scan(&rootID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, ErrNotFound
		}
		if err != nil {
			return nil, nil, err
		}
		if !slices.Contains(rootIDs, rootID) {
			rootIDs = append(rootIDs, rootID)
		}
	}
	if len(rootIDs) == 0 {
		rootIDs = []int64{0}
	}
	args := []any{rootIDs[0], r.Selection == "issue-only", r.MoveSeries, instanceUID}
	placeholders := []string{"$1"}
	for _, id := range rootIDs[1:] {
		args = append(args, id)
		placeholders = append(placeholders, fmt.Sprintf("$%d", len(args)))
	}
	seriesSeed := ""
	for _, root := range seriesRoots {
		if admitted, restricted := AuthorizedProjects(ctx); restricted && !slices.Contains(admitted, root.ProjectUID) {
			return nil, nil, ErrNotFound
		}
		if err := CheckProjectAccessTransaction(ctx, q, root.ProjectUID); err != nil {
			return nil, nil, err
		}
		var id int64
		err := q.QueryRowContext(ctx, `SELECT r.id FROM recurrences r JOIN projects p ON p.id=r.project_id WHERE r.uid=$1 AND p.uid=$2 AND p.deleted_at IS NULL AND r.moved_to_authority_uid IS NULL`, root.RecurrenceUID, root.ProjectUID).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, ErrNotFound
		}
		if err != nil {
			return nil, nil, err
		}
		args = append(args, root.RecurrenceUID)
		seriesSeed += fmt.Sprintf(" UNION SELECT 'series',uid FROM recurrences WHERE uid=$%d", len(args))
	}
	rows, err := q.QueryContext(ctx, `WITH RECURSIVE edges(from_kind,from_uid,to_kind,to_uid) AS (
 SELECT 'issue',to_issue_uid,'issue',from_issue_uid FROM links WHERE type='parent' AND NOT $2 AND from_issue_uid IS NOT NULL AND to_issue_uid IS NOT NULL
 UNION ALL SELECT 'issue',uid,'series',COALESCE(recurrence_uid,(SELECT uid FROM recurrences WHERE id=issues.recurrence_id)) FROM issues WHERE $3 AND (recurrence_id IS NOT NULL OR recurrence_uid IS NOT NULL)
 UNION ALL SELECT 'series',COALESCE(recurrence_uid,(SELECT uid FROM recurrences WHERE id=issues.recurrence_id)),'issue',uid FROM issues WHERE $3 AND (recurrence_id IS NOT NULL OR recurrence_uid IS NOT NULL)
), selection(kind,uid) AS (
 SELECT 'issue',uid FROM issues WHERE id IN (`+strings.Join(placeholders, ",")+`)`+seriesSeed+`
 UNION SELECT e.to_kind,e.to_uid FROM selection s JOIN edges e ON e.from_kind=s.kind AND e.from_uid=s.uid
)
SELECT s.uid,$4,COALESCE(p.uid,''),COALESCE(i.short_id,''),COALESCE(i.revision,0),COALESCE(i.location_generation,0),i.moved_to_authority_uid,p.deleted_at FROM selection s LEFT JOIN issues i ON s.kind='issue' AND i.uid=s.uid LEFT JOIN projects p ON p.id=i.project_id WHERE s.kind='issue' ORDER BY s.uid`, args...)
	if err != nil {
		return nil, nil, err
	}
	var members, missing []MoveTreeMember
	for rows.Next() {
		var m MoveTreeMember
		var moved, deleted *string
		if err := rows.Scan(&m.IssueUID, &m.AuthorityUID, &m.ProjectUID, &m.ShortID, &m.Revision, &m.LocationGeneration, &moved, &deleted); err != nil {
			_ = rows.Close()
			return nil, nil, err
		}
		if m.ProjectUID == "" || moved != nil {
			missing = append(missing, m)
		} else {
			if deleted != nil {
				_ = rows.Close()
				return nil, nil, ErrNotFound
			}
			members = append(members, m)
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, nil, err
	}
	admitted, restricted := AuthorizedProjects(ctx)
	for _, m := range members {
		if restricted && !slices.Contains(admitted, m.ProjectUID) {
			return nil, nil, ErrNotFound
		}
		if err := CheckProjectAccessTransaction(ctx, q, m.ProjectUID); err != nil {
			return nil, nil, err
		}
	}
	for i := range missing {
		m := &missing[i]
		err := q.QueryRowContext(ctx, `SELECT moved_to_authority_uid,moved_to_project_uid FROM issues WHERE uid=$1 AND moved_to_authority_uid IS NOT NULL`, m.IssueUID).Scan(&m.AuthorityUID, &m.ProjectUID)
		if errors.Is(err, sql.ErrNoRows) {
			err = q.QueryRowContext(ctx, `SELECT from_authority_uid,from_project_uid FROM links WHERE type='parent' AND from_issue_uid=$1 ORDER BY id LIMIT 1`, m.IssueUID).Scan(&m.AuthorityUID, &m.ProjectUID)
		}
		if err != nil {
			return nil, nil, fmt.Errorf("selected child location unavailable")
		}
	}
	return members, missing, nil
}

func collectTreeRows[T any](seq iter.Seq2[T, error], keep func(T) bool) ([]T, error) {
	var out []T
	for record, err := range seq {
		if err != nil {
			return nil, err
		}
		if keep(record) {
			out = append(out, record)
		}
	}
	return out, nil
}

// BuildIssueTreeSnapshot consumes only native transaction-bound owner readers.
func BuildIssueTreeSnapshot(ctx context.Context, q MoveTreeSQL, store Storage, r MoveTreeRequest, postgres bool) (IssueTreeSnapshot, error) {
	members, missing, err := SelectIssueTree(ctx, q, r, store.InstanceUID())
	if err != nil {
		return IssueTreeSnapshot{}, err
	}
	portion := IssueTreePortion{AuthorityUID: store.InstanceUID(), Members: members}
	ids := map[int64]bool{}
	uids := map[string]bool{}
	projectIDs := map[int64]bool{}
	for _, m := range members {
		uids[m.IssueUID] = true
	}
	seriesRoots, err := IssueTreeSourceSeries(ctx, r, store.InstanceUID())
	if err != nil {
		return IssueTreeSnapshot{}, err
	}
	// Export iterators intentionally retain audit rows. Filter before assembling
	// the wire snapshot; project configuration is never included.
	projectMembers := append([]MoveTreeMember(nil), members...)
	for _, seriesRoot := range seriesRoots {
		projectMembers = append(projectMembers, MoveTreeMember{ProjectUID: seriesRoot.ProjectUID})
	}
	for _, m := range projectMembers {
		var p MoveTreeProject
		if err := q.QueryRowContext(ctx, `SELECT id,uid,name FROM projects WHERE uid=$1`, m.ProjectUID).Scan(&p.ID, &p.UID, &p.Name); err != nil {
			return IssueTreeSnapshot{}, err
		}
		if !projectIDs[p.ID] {
			p.AuthorityUID = store.InstanceUID()
			portion.Projects = append(portion.Projects, p)
			projectIDs[p.ID] = true
		}
	}
	for _, p := range portion.Projects {
		filter := ExportFilter{ProjectID: &p.ID, IncludeDeleted: true}
		issues, err := collectTreeRows(store.ExportIssues(ctx, filter), func(i IssueExport) bool { return uids[i.UID] })
		if err != nil {
			return IssueTreeSnapshot{}, err
		}
		portion.Issues = append(portion.Issues, issues...)
		for _, i := range issues {
			ids[i.ID] = true
		}
	}
	commentIDs := map[int64]bool{}
	commentUIDs := map[string]bool{}
	linkIDs := map[int64]bool{}
	seriesUIDs := map[string]bool{}
	for _, root := range seriesRoots {
		seriesUIDs[root.RecurrenceUID] = true
	}
	bindingUIDs := map[string]bool{}
	for _, i := range portion.Issues {
		if r.MoveSeries && i.RecurrenceUID != nil {
			seriesUIDs[*i.RecurrenceUID] = true
		}
	}
	for _, p := range portion.Projects {
		f := ExportFilter{ProjectID: &p.ID, IncludeDeleted: true}
		comments, err := collectTreeRows(store.ExportComments(ctx, f), func(c CommentExport) bool { return ids[c.IssueID] })
		if err != nil {
			return IssueTreeSnapshot{}, err
		}
		portion.Comments = append(portion.Comments, comments...)
		for _, c := range comments {
			commentIDs[c.ID] = true
			commentUIDs[c.UID] = true
		}
		labels, err := collectTreeRows(store.ExportIssueLabels(ctx, f), func(l IssueLabelExport) bool { return ids[l.IssueID] })
		if err != nil {
			return IssueTreeSnapshot{}, err
		}
		portion.Labels = append(portion.Labels, labels...)
		links, err := collectTreeRows(store.ExportLinks(ctx, f), func(l LinkExport) bool { return (uids[l.FromIssueUID] || uids[l.ToIssueUID]) && !linkIDs[l.ID] })
		if err != nil {
			return IssueTreeSnapshot{}, err
		}
		portion.Links = append(portion.Links, links...)
		for _, l := range links {
			linkIDs[l.ID] = true
		}
		recurrences, err := collectTreeRows(store.ExportRecurrences(ctx, f), func(v RecurrenceExport) bool { return seriesUIDs[v.UID] })
		if err != nil {
			return IssueTreeSnapshot{}, err
		}
		portion.Recurrences = append(portion.Recurrences, recurrences...)
		tombstones, err := collectTreeRows(store.ExportPurgeLog(ctx, f), func(v PurgeLogExport) bool { return v.RecurrenceUID != nil && seriesUIDs[*v.RecurrenceUID] })
		if err != nil {
			return IssueTreeSnapshot{}, err
		}
		portion.Tombstones = append(portion.Tombstones, tombstones...)
		mappings, err := collectTreeRows(store.ExportImportMappings(ctx, f), func(v ImportMappingExport) bool {
			return v.IssueID != nil && ids[*v.IssueID] || v.CommentID != nil && commentIDs[*v.CommentID] || v.LinkID != nil && linkIDs[*v.LinkID]
		})
		if err != nil {
			return IssueTreeSnapshot{}, err
		}
		portion.Mappings = append(portion.Mappings, mappings...)
		bindings, err := collectTreeRows(store.ExportExternalRootBindings(ctx, f), func(v ExternalRootBindingExport) bool { return uids[v.IssueUID] })
		if err != nil {
			return IssueTreeSnapshot{}, err
		}
		portion.ExternalBindings = append(portion.ExternalBindings, bindings...)
		for _, b := range bindings {
			bindingUIDs[b.UID] = true
		}
		states, err := collectTreeRows(store.ExportExternalFieldStates(ctx, f), func(v ExternalFieldStateExport) bool { return bindingUIDs[v.BindingUID] })
		if err != nil {
			return IssueTreeSnapshot{}, err
		}
		portion.ExternalStates = append(portion.ExternalStates, states...)
		claims, err := collectTreeRows(store.ExportIssueClaims(ctx, f), func(v IssueClaimExport) bool { return uids[v.IssueUID] })
		if err != nil {
			return IssueTreeSnapshot{}, err
		}
		portion.Claims = append(portion.Claims, claims...)
		pending, err := collectTreeRows(store.ExportPendingClaimRequests(ctx, f), func(v PendingClaimRequestExport) bool { return uids[v.IssueUID] })
		if err != nil {
			return IssueTreeSnapshot{}, err
		}
		portion.PendingClaims = append(portion.PendingClaims, pending...)
	}
	for v, err := range store.ExportExternalFieldMappings(ctx, ExportFilter{IncludeDeleted: true}) {
		if err != nil {
			return IssueTreeSnapshot{}, err
		}
		for _, state := range portion.ExternalStates {
			if state.MappingConnectorInstance == v.ConnectorInstance && state.MappingKataField == v.KataField && state.MappingExternalFieldID == v.ExternalFieldID && state.MappingSchemaRevision == v.SchemaRevision && state.MappingCreatedAt.Equal(v.CreatedAt) {
				portion.ExternalMappings = append(portion.ExternalMappings, v)
				break
			}
		}
	}
	// Creation proofs are issue-specific. Keys and receipts are selected only
	// through those proofs; unrelated source histories never enter the snapshot.
	var receipts []AttributionReceipt
	var keys []RootKeyPin
	for record, err := range store.ExportAttribution(ctx, ExportFilter{IncludeDeleted: true}) {
		if err != nil {
			return IssueTreeSnapshot{}, err
		}
		switch v := record.(type) {
		case *EntityProvenance:
			if v.Kind == "issue" && uids[v.EntityUID] || v.Kind == "comment" && commentUIDs[v.EntityUID] {
				portion.CreationProofs = append(portion.CreationProofs, *v)
			}
		case *AttributionReceipt:
			receipts = append(receipts, *v)
		case *RootKeyPin:
			keys = append(keys, *v)
		}
	}
	for _, receipt := range receipts {
		for _, proof := range portion.CreationProofs {
			if receipt.ProjectUID == proof.ProjectUID && receipt.EventUID == proof.EventUID {
				portion.Receipts = append(portion.Receipts, receipt)
				break
			}
		}
	}
	for _, key := range keys {
		for _, receipt := range portion.Receipts {
			if key.ProjectUID == receipt.ProjectUID && key.KeyID == receipt.KeyID {
				portion.PublicKeys = append(portion.PublicKeys, key)
				break
			}
		}
	}
	if exporter, ok := store.(EmbeddingArtifactExporter); ok {
		selectedUIDs := make([]string, 0, len(uids))
		for _, m := range members {
			selectedUIDs = append(selectedUIDs, m.IssueUID)
		}
		for record, err := range exporter.ExportEmbeddingArtifacts(ctx, ExportFilter{IncludeDeleted: true}, selectedUIDs...) {
			if err != nil {
				return IssueTreeSnapshot{}, err
			}
			v, ok := record.(*EmbeddingArtifactExport)
			if !ok {
				return IssueTreeSnapshot{}, ErrFederationIngestValidation
			}
			portion.Artifacts = append(portion.Artifacts, v.EmbeddingArtifact)
		}
	}
	// Read original event envelopes directly: generic backup exporters may scrub
	// remote peers or recompute historical hashes, which transfers must not do.
	eventUIDs := map[string]bool{}
	appendHistory := func(events []EventExport) {
		for _, event := range events {
			if !eventUIDs[event.UID] {
				portion.Events = append(portion.Events, event)
				eventUIDs[event.UID] = true
			}
		}
	}
	for _, m := range members {
		events, err := readMoveSnapshotHistory(ctx, q, `e.issue_uid=$1`, m.IssueUID)
		if err != nil {
			return IssueTreeSnapshot{}, err
		}
		appendHistory(events)
	}
	recurrenceExpression := `json_extract(e.payload,'$.recurrence_uid')`
	if postgres {
		recurrenceExpression = `e.payload::jsonb->>'recurrence_uid'`
	}
	for _, series := range portion.Recurrences {
		events, err := readMoveSnapshotHistory(ctx, q, `e.type LIKE 'recurrence.%' AND `+recurrenceExpression+`=$1`, series.UID)
		if err != nil {
			return IssueTreeSnapshot{}, err
		}
		appendHistory(events)
	}

	requiredSeries := []MoveTreeSeriesRoot{}
	if r.MoveSeries {
		for _, issue := range portion.Issues {
			if issue.RecurrenceUID != nil && issue.RecurrenceAuthorityUID != nil && *issue.RecurrenceAuthorityUID != store.InstanceUID() && issue.RecurrenceProjectUID != nil {
				root := MoveTreeSeriesRoot{RecurrenceUID: *issue.RecurrenceUID, AuthorityUID: *issue.RecurrenceAuthorityUID, ProjectUID: *issue.RecurrenceProjectUID}
				if !slices.Contains(requiredSeries, root) {
					requiredSeries = append(requiredSeries, root)
				}
			}
		}
	}
	snapshot := IssueTreeSnapshot{Version: 1, RequestUID: r.RequestUID, Portions: []IssueTreePortion{portion}, Required: missing, RequiredSeries: requiredSeries}
	snapshot.Digest, err = IssueTreeSnapshotDigest(snapshot)
	return snapshot, err
}

func ValidateMoveSourceEvent(event EventExport) error {
	var original EventHashInput
	if err := json.Unmarshal(event.SourceEventBytes, &original, json.RejectUnknownMembers(true)); err != nil {
		return fmt.Errorf("original event envelope: %w", err)
	}
	hash, err := EventContentHash(original)
	if err != nil {
		return err
	}
	if hash != event.ContentHash || original.UID != event.UID || original.OriginInstanceUID != event.OriginInstanceUID || !sameMoveOptionalUID(original.IssueUID, event.IssueUID) {
		return fmt.Errorf("original event identity or hash mismatch")
	}
	return nil
}

func sameMoveOptionalUID(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func readMoveSnapshotHistory(ctx context.Context, q MoveTreeSQL, predicate string, args ...any) ([]EventExport, error) {
	rows, err := q.QueryContext(ctx, `SELECT e.id,e.uid,e.origin_instance_uid,COALESCE(e.project_id,0),COALESCE(e.source_project_uid,p.uid),e.project_name,e.issue_id,e.issue_uid,e.related_issue_id,e.related_issue_uid,e.type,e.actor,e.payload,e.hlc_physical_ms,e.hlc_counter,e.content_hash,CAST(e.created_at AS TEXT),e.source_project_uid,e.source_event_bytes FROM events e LEFT JOIN projects p ON p.id=e.project_id WHERE `+predicate+` ORDER BY e.uid`, args...)
	if err != nil {
		return nil, err
	}
	var events []EventExport
	for rows.Next() {
		var e EventExport
		var payload string
		if err := rows.Scan(&e.ID, &e.UID, &e.OriginInstanceUID, &e.ProjectID, &e.ProjectUID, &e.ProjectName, &e.IssueID, &e.IssueUID, &e.RelatedIssueID, &e.RelatedIssueUID, &e.Type, &e.Actor, &payload, &e.HLCPhysicalMS, &e.HLCCounter, &e.ContentHash, &e.CreatedAt, &e.SourceProjectUID, &e.SourceEventBytes); err != nil {
			_ = rows.Close()
			return nil, err
		}
		e.Payload = jsontext.Value(payload)
		original := EventHashInput{UID: e.UID, OriginInstanceUID: e.OriginInstanceUID, ProjectUID: e.ProjectUID, IssueUID: e.IssueUID, RelatedIssueUID: e.RelatedIssueUID, Type: e.Type, Actor: e.Actor, HLCPhysicalMS: e.HLCPhysicalMS, HLCCounter: e.HLCCounter, CreatedAt: e.CreatedAt, Payload: e.Payload}
		if len(e.SourceEventBytes) == 0 {
			raw, err := json.Marshal(original, json.Deterministic(true))
			if err != nil {
				_ = rows.Close()
				return nil, err
			}
			e.SourceEventBytes = raw
		}
		if err := ValidateMoveSourceEvent(e); err != nil {
			_ = rows.Close()
			return nil, err
		}
		e.SourceProjectUID = new(e.ProjectUID)
		events = append(events, e)
	}
	return events, errors.Join(rows.Err(), rows.Close())
}
