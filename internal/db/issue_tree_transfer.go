package db

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"go.kenn.io/kata/internal/uid"
	"strings"
)

var ErrMoveAuthorityMismatch = fmt.Errorf("move endpoint authority does not match this store")
var ErrMoveConflict = fmt.Errorf("move conflicts with committed state; reconcile the copies explicitly")

// MoveRemoteSelectionError requires collecting the actual authorities before
// a local commit. It never permits silently dropping retained moved members.
type MoveRemoteSelectionError struct {
	Required       []MoveTreeMember
	RequiredSeries []MoveTreeSeriesRoot
}

func (e *MoveRemoteSelectionError) Error() string {
	return "remote-selection-required: collect every selected authority through the issue-moves coordinator"
}

// RequireLocalIssueTree inspects raw routing owners after closure authorization,
// before any local write. Retained projections never substitute for remote work.
func RequireLocalIssueTree(ctx context.Context, q MoveTreeSQL, ids []int64, moveSeries bool, instanceUID string) error {
	var requiredSeries []MoveTreeSeriesRoot
	var required []MoveTreeMember
	for _, id := range ids {
		var member MoveTreeMember
		var authority, project, short *string
		if err := q.QueryRowContext(ctx, `SELECT uid,moved_to_authority_uid,moved_to_project_uid,moved_to_short_id,location_generation FROM issues WHERE id=$1`, id).Scan(&member.IssueUID, &authority, &project, &short, &member.LocationGeneration); err != nil {
			return err
		}
		if moveSeries {
			var seriesUID, seriesAuthority, seriesProject *string
			err := q.QueryRowContext(ctx, `SELECT COALESCE(i.recurrence_uid,(SELECT uid FROM recurrences WHERE id=i.recurrence_id)),i.recurrence_authority_uid,i.recurrence_project_uid FROM issues i WHERE i.id=$1`, id).Scan(&seriesUID, &seriesAuthority, &seriesProject)
			if err != nil {
				return err
			}
			if seriesUID != nil && seriesAuthority != nil && *seriesAuthority != instanceUID && seriesProject != nil {
				root := MoveTreeSeriesRoot{RecurrenceUID: *seriesUID, AuthorityUID: *seriesAuthority, ProjectUID: *seriesProject}
				found := false
				for _, prior := range requiredSeries {
					if prior == root {
						found = true
					}
				}
				if !found {
					requiredSeries = append(requiredSeries, root)
				}
			}
		}
		if authority != nil {
			member.AuthorityUID = *authority
			member.ProjectUID = *project
			member.ShortID = *short
			required = append(required, member)
		}
	}
	if len(required) > 0 || len(requiredSeries) > 0 {
		return &MoveRemoteSelectionError{Required: required, RequiredSeries: requiredSeries}
	}
	return nil
}

func MoveTreeFailureResult(r MoveTreeRequest, err error) MoveTreeResult {
	result := moveConflictResult(r)
	if remote, ok := errors.AsType[*MoveRemoteSelectionError](err); ok {
		result.Conflicts = nil
		for _, m := range remote.Required {
			result.Conflicts = append(result.Conflicts, MoveTreeAction{IssueUID: m.IssueUID, AuthorityUID: m.AuthorityUID, Kind: "remote-selection-required", Message: remote.Error()})
		}
		for _, series := range remote.RequiredSeries {
			result.Conflicts = append(result.Conflicts, MoveTreeAction{AuthorityUID: series.AuthorityUID, Kind: "remote-series-owner-required", Message: "collect the actual scheduler owner before moving the full series"})
		}
		result.RetryAction = "retry through the issue-moves coordinator with access to every required authority"
	}
	return result
}

func ValidateMoveRetryFact(digest string, event Event) error {
	var fact struct {
		RequestDigest string `json:"request_digest"`
	}
	if err := json.Unmarshal([]byte(event.Payload), &fact); err != nil {
		return err
	}
	if fact.RequestDigest != digest {
		return ErrMoveConflict
	}
	return nil
}

func ValidateLocalMoveRetryIssue(event Event, issue Issue) error {
	var fact struct {
		Revision   int64  `json:"revision"`
		ProjectUID string `json:"to_project_uid"`
		ShortID    string `json:"to_short_id"`
	}
	if err := json.Unmarshal([]byte(event.Payload), &fact); err != nil {
		return err
	}
	if issue.Revision != fact.Revision || issue.ShortID != fact.ShortID || issue.ProjectID != event.ProjectID {
		return ErrMoveConflict
	}
	return nil
}

// MoveTreeRequest uses immutable endpoint identities. RequestUID is generated
// by the caller once and reused after disconnection.
type MoveTreeRequest struct {
	SourceAuthorityUID      string `json:"source_authority_uid"`
	SourceProjectUID        string `json:"source_project_uid"`
	IssueUID                string `json:"issue_uid"`
	DestinationAuthorityUID string `json:"destination_authority_uid"`
	DestinationProjectUID   string `json:"destination_project_uid"`
	Selection               string `json:"selection,omitempty"`
	MoveSeries              bool   `json:"move_series,omitempty"`
	ProviderDisposition     string `json:"provider_disposition,omitempty"`
	PreviewDigest           string `json:"preview_digest,omitempty"`
	RequestUID              string `json:"request_uid"`
	ActorAuthorityUID       string `json:"actor_authority_uid"`
	Actor                   string `json:"actor"`
}

func (r MoveTreeRequest) Normalize() (MoveTreeRequest, error) {
	for _, field := range []struct{ name, value string }{{"source_authority_uid", r.SourceAuthorityUID}, {"source_project_uid", r.SourceProjectUID}, {"issue_uid", r.IssueUID}, {"destination_authority_uid", r.DestinationAuthorityUID}, {"destination_project_uid", r.DestinationProjectUID}, {"request_uid", r.RequestUID}, {"actor_authority_uid", r.ActorAuthorityUID}} {
		if !uid.Valid(field.value) {
			return r, fmt.Errorf("%s must be a ULID", field.name)
		}
	}
	if r.Selection == "" {
		r.Selection = "subtree"
	}
	if r.Selection != "subtree" && r.Selection != "issue-only" {
		return r, fmt.Errorf("selection must be subtree or issue-only")
	}
	if r.ProviderDisposition == "" {
		r.ProviderDisposition = "preserve"
	}
	if r.ProviderDisposition != "preserve" && r.ProviderDisposition != "detach" {
		return r, fmt.Errorf("provider_disposition must be preserve or detach")
	}
	if strings.TrimSpace(r.Actor) == "" {
		return r, fmt.Errorf("actor must be non-empty")
	}
	return r, nil
}

// MoveRequestDigest identifies exact retry options, including the supplied preview.
func MoveRequestDigest(r MoveTreeRequest) (string, error) {
	normalized, err := r.Normalize()
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(normalized)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

type MoveTreeMember struct {
	IssueUID           string `json:"issue_uid"`
	AuthorityUID       string `json:"authority_uid"`
	ProjectUID         string `json:"project_uid"`
	ShortID            string `json:"short_id"`
	Revision           int64  `json:"revision"`
	LocationGeneration int64  `json:"location_generation"`
}

type MoveTreePreview struct {
	Digest            string           `json:"digest"`
	SnapshotDigest    string           `json:"snapshot_digest"`
	SourceRevisions   map[string]int64 `json:"source_revisions"`
	Members           []MoveTreeMember `json:"members"`
	Relations         []LinkExport     `json:"relations"`
	ProviderActions   []MoveTreeAction `json:"provider_actions"`
	RecurrenceActions []MoveTreeAction `json:"recurrence_actions"`
	Blockers          []MoveTreeAction `json:"blockers"`
}
type MoveTreeAction struct {
	IssueUID     string `json:"issue_uid,omitempty"`
	AuthorityUID string `json:"authority_uid,omitempty"`
	Kind         string `json:"kind"`
	Message      string `json:"message"`
}
type MoveTreeApplied struct {
	SourceSnapshotDigest       string `json:"source_snapshot_digest,omitempty"`
	SourceSeriesUID            string `json:"source_series_uid,omitempty"`
	SourceSeriesProjectUID     string `json:"source_series_project_uid,omitempty"`
	SourceSeriesAuthorityUID   string `json:"source_series_authority_uid,omitempty"`
	SourceSeriesSnapshotDigest string `json:"source_series_snapshot_digest,omitempty"`
	IssueUID                   string `json:"issue_uid"`
	FromAuthorityUID           string `json:"from_authority_uid"`
	FromProjectUID             string `json:"from_project_uid"`
	FromShortID                string `json:"from_short_id"`
	AuthorityUID               string `json:"authority_uid"`
	ProjectUID                 string `json:"project_uid"`
	ShortID                    string `json:"short_id"`
	Revision                   int64  `json:"revision"`
	LocationGeneration         int64  `json:"location_generation"`
	CommittedEventUID          string `json:"committed_event_uid,omitempty"`
	CommittedEventHash         string `json:"committed_event_hash,omitempty"`
}
type MoveTreeResult struct {
	SnapshotDigest        string            `json:"snapshot_digest,omitempty"`
	ProjectAccessRevision int64             `json:"-"`
	State                 string            `json:"state"`
	RequestUID            string            `json:"request_uid"`
	Applied               []MoveTreeApplied `json:"applied"`
	Finalized             []string          `json:"finalized"`
	Conflicts             []MoveTreeAction  `json:"conflicts"`
	UnknownRoots          []string          `json:"unknown_roots"`
	RetryAction           string            `json:"retry_action,omitempty"`
	Events                []Event           `json:"-"`
}

// Required wire collections stay arrays even when there are no members or
// committed results. The daemon also uses the legacy JSON encoder.
func (p MoveTreePreview) MarshalJSON() ([]byte, error) {
	type wire MoveTreePreview
	return json.Marshal(wire(p), json.FormatNilSliceAsNull(false), json.FormatNilMapAsNull(false))
}

func (r MoveTreeResult) MarshalJSON() ([]byte, error) {
	type wire MoveTreeResult
	return json.Marshal(wire(r), json.FormatNilSliceAsNull(false))
}

// MoveTreeLocalResult converts native transaction evidence without treating a
// transport acknowledgement as a committed event.
func MoveTreeLocalResult(r MoveTreeRequest, out MoveIssueTreeOut) (MoveTreeResult, error) {
	result := MoveTreeResult{State: "completed", RequestUID: r.RequestUID, Events: out.Events, ProjectAccessRevision: out.Root.ProjectAccessRevision}
	if out.Replayed {
		result.Events = nil
	}
	for _, issue := range out.Issues {
		applied := MoveTreeApplied{IssueUID: issue.UID, AuthorityUID: r.DestinationAuthorityUID, ProjectUID: r.DestinationProjectUID, ShortID: issue.ShortID, Revision: issue.Revision}
		for _, event := range out.Events {
			if event.IssueUID == nil || *event.IssueUID != issue.UID {
				continue
			}
			var fact struct {
				FromAuthorityUID   string `json:"from_authority_uid"`
				FromProjectUID     string `json:"from_project_uid"`
				FromShortID        string `json:"from_short_id"`
				LocationGeneration int64  `json:"location_generation"`
			}
			if err := json.Unmarshal([]byte(event.Payload), &fact); err != nil {
				return MoveTreeResult{}, err
			}
			applied.FromAuthorityUID, applied.FromProjectUID, applied.FromShortID = fact.FromAuthorityUID, fact.FromProjectUID, fact.FromShortID
			applied.LocationGeneration = fact.LocationGeneration
			applied.CommittedEventUID, applied.CommittedEventHash = event.UID, event.ContentHash
		}
		if applied.CommittedEventUID == "" {
			continue
		}
		result.Applied = append(result.Applied, applied)
		result.Finalized = append(result.Finalized, issue.UID)
	}
	return result, nil
}

// MoveTreeSourceReceipt resolves a finalized source without returning its body.
// The event is selected by the retained row's exact move event UID.
type MoveTreeSourceReceipt struct {
	RequestUID     string         `json:"request_uid"`
	SnapshotDigest string         `json:"snapshot_digest"`
	Route          MoveTreeMember `json:"route"`
}

type issueTreeSourceRootsKey struct{}

// WithIssueTreeSourceRoots selects actual local portions of one canonical move
// request. Selectors are transient and are reauthorized inside native locks.
func WithIssueTreeSourceRoots(ctx context.Context, roots []MoveTreeMember) context.Context {
	return context.WithValue(ctx, issueTreeSourceRootsKey{}, append([]MoveTreeMember(nil), roots...))
}
func IssueTreeSourceRoots(ctx context.Context, r MoveTreeRequest, instanceUID string) ([]MoveTreeMember, error) {
	roots, _ := ctx.Value(issueTreeSourceRootsKey{}).([]MoveTreeMember)
	if len(roots) == 0 {
		series, err := IssueTreeSourceSeries(ctx, r, instanceUID)
		if err != nil {
			return nil, err
		}
		if len(series) > 0 {
			return nil, nil
		}
		roots = []MoveTreeMember{{IssueUID: r.IssueUID, AuthorityUID: r.SourceAuthorityUID, ProjectUID: r.SourceProjectUID}}
	}
	for _, root := range roots {
		if root.AuthorityUID != instanceUID || !uid.Valid(root.IssueUID) || !uid.Valid(root.ProjectUID) {
			return nil, ErrMoveAuthorityMismatch
		}
	}
	return roots, nil
}

// UncommittedIssueTreeAcceptance removes provisional transaction output. No
// event/reference allocated in a rolled-back writer is acceptance evidence.
func UncommittedIssueTreeAcceptance(result MoveTreeResult) MoveTreeResult {
	result.Events = nil
	result.Applied = nil
	result.Finalized = nil
	result.State = "conflict"
	return result
}
func UncommittedIssueTreeFinalization(result, accepted MoveTreeResult) MoveTreeResult {
	result.Events = nil
	result.Finalized = nil
	result.RequestUID = accepted.RequestUID
	result.SnapshotDigest = accepted.SnapshotDigest
	result.Applied = accepted.Applied
	if result.State == "" || result.State == "completed" {
		result.State = "partial"
	}
	return result
}

// MoveTreeSeriesRoot locates the actual scheduler owner without an occurrence
// body or parent edge. It is only a transient authorized selection.
type MoveTreeSeriesRoot struct {
	RecurrenceUID string `json:"recurrence_uid"`
	AuthorityUID  string `json:"authority_uid"`
	ProjectUID    string `json:"project_uid"`
}
type issueTreeSourceSeriesKey struct{}

func WithIssueTreeSourceSeries(ctx context.Context, roots []MoveTreeSeriesRoot) context.Context {
	return context.WithValue(ctx, issueTreeSourceSeriesKey{}, append([]MoveTreeSeriesRoot(nil), roots...))
}
func IssueTreeSourceSeries(ctx context.Context, r MoveTreeRequest, instanceUID string) ([]MoveTreeSeriesRoot, error) {
	roots, _ := ctx.Value(issueTreeSourceSeriesKey{}).([]MoveTreeSeriesRoot)
	for _, root := range roots {
		if !r.MoveSeries || root.AuthorityUID != instanceUID || !uid.Valid(root.RecurrenceUID) || !uid.Valid(root.ProjectUID) {
			return nil, ErrMoveAuthorityMismatch
		}
	}
	return roots, nil
}
