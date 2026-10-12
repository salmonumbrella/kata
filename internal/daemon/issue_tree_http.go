package daemon

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"slices"
	"time"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/httpurl"
)

type issueTreeHTTP struct {
	origin, authority string
	client            *http.Client
}

// Only an explicitly configured target credential is consumed. Global daemon
// authentication is never a fallback. Credentials stay pinned to a root origin.
func issueTreeTarget(ctx context.Context, cfg ServerConfig, authority, actor string) (*issueTreeHTTP, error) {
	var target *issueTreeHTTP
	for _, entry := range cfg.FederationCatalog {
		if entry.Local || entry.URL == "" || entry.InstanceUID != authority {
			continue
		}
		base, err := httpurl.CanonicalHTTPBaseURL(entry.URL)
		if err != nil {
			return nil, err
		}
		origin, err := httpurl.CanonicalHTTPOrigin(entry.URL)
		if err != nil || base != origin {
			return nil, fmt.Errorf("move authority requires a canonical root HTTP origin")
		}
		if target != nil {
			return nil, fmt.Errorf("ambiguous configured move authority")
		}
		token := entry.Token
		if entry.TokenEnv != "" {
			if token != "" {
				return nil, fmt.Errorf("ambiguous target credential")
			}
			token = os.Getenv(entry.TokenEnv)
			if token == "" {
				return nil, fmt.Errorf("configured target credential is unavailable")
			}
		}
		client := &http.Client{Transport: http.DefaultTransport, Timeout: 120 * time.Second}
		policy := config.BearerPolicy{TrustPrivateNetwork: cfg.Auth.TrustPrivateNetwork, AllowInsecurePlaintext: entry.AllowInsecure}
		if err := policy.ConfigureClient(client, origin, token); err != nil {
			return nil, err
		}
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		target = &issueTreeHTTP{origin: origin, authority: authority, client: client}
	}
	if target == nil {
		return nil, api.NewError(409, "remote-selection-required", "no configured route for the requested authority", "configure an origin-pinned daemon target", nil)
	}
	var instance api.InstanceResponse
	if err := target.call(ctx, http.MethodGet, "/api/v1/instance", nil, &instance.Body); err != nil {
		return nil, err
	}
	if instance.Body.InstanceUID != authority {
		return nil, issueTreeAPIError(db.ErrMoveAuthorityMismatch)
	}
	policy := effectiveUIPolicy(ctx, cfg).Capabilities
	if !policy.Writable || !instance.Body.WebUICapabilities.Writable || instance.Body.WebUICapabilities.ActorPolicy != policy.ActorPolicy {
		return nil, api.NewError(403, "issue_move_policy_mismatch", "source and destination write policies do not permit this move", "", nil)
	}
	// An identity credential on the receiver must bind the same actual actor.
	if instance.Body.Auth.Actor != "" && instance.Body.Auth.Actor != actor {
		return nil, api.NewError(403, "issue_move_actor_mismatch", "destination credential belongs to another actor", "", nil)
	}
	return target, nil
}

type issueTreeHTTPError struct {
	status int
	body   string
}

func (e *issueTreeHTTPError) Error() string {
	return fmt.Sprintf("move authority response %d: %s", e.status, e.body)
}

func (target *issueTreeHTTP) call(ctx context.Context, method, path string, body, response any) error {
	var raw []byte
	var err error
	if body != nil {
		raw, err = json.Marshal(body)
		if err != nil {
			return err
		}
		if len(raw) > db.MaxIssueTreeSnapshotBytes {
			return fmt.Errorf("move request exceeds byte limit")
		}
	}
	request, err := http.NewRequestWithContext(ctx, method, target.origin+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	result, err := target.client.Do(request)
	if err != nil {
		return err
	}
	defer result.Body.Close()
	if result.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(result.Body, 4096))
		return &issueTreeHTTPError{status: result.StatusCode, body: string(raw)}
	}
	raw, err = io.ReadAll(io.LimitReader(result.Body, db.MaxIssueTreeSnapshotBytes+1))
	if err != nil {
		return err
	}
	if len(raw) > db.MaxIssueTreeSnapshotBytes {
		return fmt.Errorf("move response exceeds byte limit")
	}
	return json.Unmarshal(raw, response, json.RejectUnknownMembers(true))
}

func (target *issueTreeHTTP) lookup(ctx context.Context, r db.MoveTreeRequest) (db.MoveTreeResult, error) {
	in := &api.IssueTreeTransferRequest{}
	in.Body.Request = r
	out := &api.IssueTreeTransferResponse{}
	err := target.call(ctx, http.MethodPost, "/api/v1/issue-moves/lookup", in.Body, &out.Body)
	if err != nil {
		return db.MoveTreeResult{}, err
	}
	if out.Body.Result == nil {
		return db.MoveTreeResult{}, db.ErrMoveCommitUnverified
	}
	return *out.Body.Result, nil
}
func (target *issueTreeHTTP) verify(ctx context.Context, r db.MoveTreeRequest, proposed db.MoveTreeResult) error {
	actual, err := target.lookup(ctx, r)
	if err != nil {
		return err
	}
	return db.ValidateCommittedMoveEvidence(proposed, actual)
}

func coordinateIssueTreeHTTP(ctx context.Context, cfg ServerConfig, r db.MoveTreeRequest, dryRun bool) (*api.IssueTreeMoveResponse, error) {
	if r.SourceAuthorityUID != cfg.DB.InstanceUID() {
		return nil, issueTreeAPIError(db.ErrMoveAuthorityMismatch)
	}
	target, err := issueTreeTarget(ctx, cfg, r.DestinationAuthorityUID, r.Actor)
	if err != nil {
		var transportErr net.Error
		var responseErr *issueTreeHTTPError
		if !dryRun && (errors.As(err, &transportErr) || errors.As(err, &responseErr) && responseErr.status >= 500) {
			out := &api.IssueTreeMoveResponse{}
			out.Body.Result = &db.MoveTreeResult{State: "outcome-unknown", RequestUID: r.RequestUID, UnknownRoots: []string{r.DestinationAuthorityUID}, RetryAction: "look up destination acceptance using the same complete request before retrying"}
			return out, nil
		}
		return nil, err
	}
	if !dryRun {
		accepted, lookupErr := target.lookup(ctx, r)
		if lookupErr == nil {
			collection, err := recoverIssueTreeCollection(ctx, cfg, r, accepted)
			if err != nil {
				return nil, err
			}
			return finalizeIssueTreeCollection(ctx, cfg, r, target, collection, accepted), nil
		}
		var responseErr *issueTreeHTTPError
		if !errors.As(lookupErr, &responseErr) || responseErr.status != http.StatusNotFound {
			if errors.As(lookupErr, &responseErr) && responseErr.status == http.StatusConflict {
				return nil, issueTreeAPIError(db.ErrMoveConflict)
			}
			out := &api.IssueTreeMoveResponse{}
			out.Body.Result = &db.MoveTreeResult{State: "outcome-unknown", RequestUID: r.RequestUID, UnknownRoots: []string{r.DestinationAuthorityUID}, RetryAction: "look up destination acceptance using the same complete request before retrying"}
			return out, nil
		}
		_, receiptErr := cfg.DB.LookupIssueTreeSourceReceipt(ctx, r)
		if receiptErr == nil {
			out := &api.IssueTreeMoveResponse{}
			out.Body.Result = &db.MoveTreeResult{State: "outcome-unknown", RequestUID: r.RequestUID, UnknownRoots: []string{r.DestinationAuthorityUID}, RetryAction: "destination receipt unavailable for a finalized source; reconcile retained routes"}
			return out, nil
		}
		if !errors.Is(receiptErr, db.ErrNotFound) {
			return nil, issueTreeAPIError(receiptErr)
		}
	}
	root, err := cfg.DB.IssueByUID(ctx, r.IssueUID, db.IncludeDeletedYes)
	if err != nil {
		return nil, issueTreeAPIError(err)
	}
	if err = authorizeIssueScopedIssue(ctx, cfg.DB, root); err != nil {
		return nil, err
	}
	snapshot, err := cfg.DB.ExportIssueTree(ctx, r)
	if err != nil {
		return nil, issueTreeAPIError(err)
	}
	collection, err := collectIssueTreeHTTP(ctx, cfg, r, snapshot)
	if err != nil {
		return nil, err
	}
	snapshot = collection.snapshot
	preview := db.MoveTreePreview{Digest: snapshot.Digest, SnapshotDigest: snapshot.Digest, SourceRevisions: map[string]int64{}}
	for _, portion := range snapshot.Portions {
		db.AddIssueTreePreviewConsequences(&preview, r, portion)
		preview.Members = append(preview.Members, portion.Members...)
		preview.Relations = append(preview.Relations, portion.Links...)
		for _, member := range portion.Members {
			preview.SourceRevisions[member.IssueUID] = member.Revision
		}
	}
	out := &api.IssueTreeMoveResponse{}
	if dryRun {
		out.Body.Preview = &preview
		return out, nil
	}
	if r.PreviewDigest != "" && r.PreviewDigest != preview.Digest {
		return nil, issueTreeAPIError(db.ErrMovePreviewStale)
	}
	in := &api.IssueTreeTransferRequest{}
	in.Body.Request = r
	in.Body.Snapshot = &snapshot
	accepted := &api.IssueTreeTransferResponse{}
	if err = target.call(ctx, http.MethodPost, "/api/v1/issue-moves/accept", in.Body, &accepted.Body); err != nil {
		// A lost response cannot establish rollback. Independently resolve actual
		// destination facts before any source finalization or resend.
		actual, lookupErr := target.lookup(ctx, r)
		if lookupErr != nil {
			state := "outcome-unknown"
			var httpErr *issueTreeHTTPError
			if errors.As(err, &httpErr) && httpErr.status >= 400 && httpErr.status < 500 {
				state = "conflict"
			}
			result := db.MoveTreeResult{State: state, RequestUID: r.RequestUID, UnknownRoots: []string{r.DestinationAuthorityUID}, RetryAction: "look up destination acceptance using the same complete request before retrying"}
			out.Body.Result = &result
			return out, nil
		}
		accepted.Body.Result = &actual
	}
	if accepted.Body.Result == nil {
		return nil, issueTreeAPIError(db.ErrMoveCommitUnverified)
	}
	return finalizeIssueTreeCollection(ctx, cfg, r, target, collection, *accepted.Body.Result), nil
}

// Recovery reconstructs only authorized selectors from ordinary committed
// per-issue facts; it never reads retained source bodies or persists a journal.
func recoverIssueTreeCollection(ctx context.Context, cfg ServerConfig, r db.MoveTreeRequest, accepted db.MoveTreeResult) (issueTreeCollection, error) {
	collection := issueTreeCollection{snapshot: db.IssueTreeSnapshot{Digest: accepted.SnapshotDigest}, sources: map[string]db.IssueTreeSnapshot{}, roots: map[string][]db.MoveTreeMember{}, series: map[string][]db.MoveTreeSeriesRoot{}, targets: map[string]*issueTreeHTTP{}}
	for _, applied := range accepted.Applied {
		authority := applied.FromAuthorityUID
		prior := collection.sources[authority].Digest
		if applied.SourceSnapshotDigest == "" || prior != "" && prior != applied.SourceSnapshotDigest {
			return collection, issueTreeAPIError(db.ErrMoveConflict)
		}
		collection.sources[authority] = db.IssueTreeSnapshot{Digest: applied.SourceSnapshotDigest}
		collection.roots[authority] = append(collection.roots[authority], db.MoveTreeMember{IssueUID: applied.IssueUID, AuthorityUID: authority, ProjectUID: applied.FromProjectUID})
		if applied.SourceSeriesUID != "" {
			owner, digest := applied.SourceSeriesAuthorityUID, applied.SourceSeriesSnapshotDigest
			if owner == "" {
				owner, digest = authority, applied.SourceSnapshotDigest
			}
			prior := collection.sources[owner].Digest
			if digest == "" || prior != "" && prior != digest {
				return collection, issueTreeAPIError(db.ErrMoveConflict)
			}
			collection.sources[owner] = db.IssueTreeSnapshot{Digest: digest}
			root := db.MoveTreeSeriesRoot{RecurrenceUID: applied.SourceSeriesUID, AuthorityUID: owner, ProjectUID: applied.SourceSeriesProjectUID}
			if !slices.Contains(collection.series[owner], root) {
				collection.series[owner] = append(collection.series[owner], root)
			}
		}
	}
	return collection, nil
}

func finalizeIssueTreeCollection(ctx context.Context, cfg ServerConfig, r db.MoveTreeRequest, target *issueTreeHTTP, collection issueTreeCollection, accepted db.MoveTreeResult) *api.IssueTreeMoveResponse {
	snapshot := collection.snapshot
	result := accepted
	result.Events = nil
	result.Finalized = nil
	result.Conflicts = nil
	result.UnknownRoots = nil
	authorities := []string{}
	for authority := range collection.sources {
		authorities = append(authorities, authority)
	}
	slices.Sort(authorities)
	for _, authority := range authorities {
		sourcePreview := db.MoveTreePreview{Digest: snapshot.Digest, SnapshotDigest: collection.sources[authority].Digest}
		var finalized db.MoveTreeResult
		var finalizeErr error
		if authority == cfg.DB.InstanceUID() {
			local := db.WithIssueTreeSourceSeries(db.WithIssueTreeSourceRoots(db.WithMoveDestinationCommitVerifier(ctx, target.verify), collection.roots[authority]), collection.series[authority])
			finalized, finalizeErr = cfg.DB.FinalizeIssueTree(local, r, sourcePreview, accepted)
			if finalizeErr == nil {
				deliverIssueTreeResult(ctx, cfg, finalized)
			}
		} else {
			in := &api.IssueTreeTransferRequest{}
			in.Body.Request = r
			in.Body.SourceRoots = collection.roots[authority]
			in.Body.SourceSeries = collection.series[authority]
			in.Body.Preview = &sourcePreview
			in.Body.Accepted = &accepted
			response := &api.IssueTreeTransferResponse{}
			sourceTarget := collection.targets[authority]
			if sourceTarget == nil {
				sourceTarget, finalizeErr = issueTreeTarget(ctx, cfg, authority, r.Actor)
			}
			if finalizeErr == nil {
				finalizeErr = sourceTarget.call(ctx, http.MethodPost, "/api/v1/issue-moves/finalize", in.Body, &response.Body)
			}
			if response.Body.Result != nil {
				finalized = *response.Body.Result
			}
		}
		if finalizeErr == nil {
			for _, issueUID := range finalized.Finalized {
				if !slices.Contains(result.Finalized, issueUID) {
					result.Finalized = append(result.Finalized, issueUID)
				}
			}
		}
		if finalizeErr != nil {
			var httpErr *issueTreeHTTPError
			if errors.Is(finalizeErr, db.ErrMoveConflict) || errors.As(finalizeErr, &httpErr) && httpErr.status == 409 {
				result.Conflicts = append(result.Conflicts, db.MoveTreeAction{AuthorityUID: authority, Kind: "changed-content", Message: "source changed after destination acceptance; reconcile the writable copies"})
			} else if authority == cfg.DB.InstanceUID() && finalized.State == "partial" {
				result.Conflicts = append(result.Conflicts, db.MoveTreeAction{AuthorityUID: authority, Kind: "source-finalization-failed", Message: "source finalization rolled back; retry against the committed destination receipt"})
			} else {
				result.UnknownRoots = append(result.UnknownRoots, authority)
			}
		}
	}
	result.State = "partial"
	result.RetryAction = "verify destination and finalize remaining source authorities with the same request and source previews"
	if len(result.UnknownRoots) > 0 {
		result.State = "outcome-unknown"
	}
	if len(result.Finalized) == len(result.Applied) && len(result.UnknownRoots) == 0 && len(result.Conflicts) == 0 {
		result.State = "completed"
		result.RetryAction = ""
	}
	out := &api.IssueTreeMoveResponse{}
	out.Body.Result = &result
	return out
}
