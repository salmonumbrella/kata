package daemon

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

// Transfer routes keep receiver validation and source finalization native. A
// supplied acceptance never authorizes finalization without live readback.
func registerIssueTreeTransferHandlers(h huma.API, cfg ServerConfig) {
	for _, action := range []string{"export", "accept", "lookup", "finalize"} {
		id := map[string]string{"export": "exportIssueTree", "accept": "acceptIssueTree", "lookup": "lookupIssueTreeAcceptance", "finalize": "finalizeIssueTree"}[action]
		huma.Register(h, huma.Operation{OperationID: id, Method: http.MethodPost, Path: "/api/v1/issue-moves/" + action}, func(ctx context.Context, in *api.IssueTreeTransferRequest) (*api.IssueTreeTransferResponse, error) {
			r, err := in.Body.Request.Normalize()
			if err != nil {
				return nil, api.NewError(400, "validation", err.Error(), "", nil)
			}
			if action == "accept" || action == "finalize" {
				actor, err := attributedActor(ctx, r.Actor)
				if err != nil {
					return nil, err
				}
				if actor != r.Actor {
					return nil, api.NewError(403, "issue_move_actor_mismatch", "move actor does not match authenticated identity", "", nil)
				}
			}
			projects := []string{r.SourceProjectUID}
			if action == "accept" || action == "lookup" {
				if r.DestinationAuthorityUID != cfg.DB.InstanceUID() {
					return nil, issueTreeAPIError(db.ErrMoveAuthorityMismatch)
				}
				projects = []string{r.DestinationProjectUID}
			} else {
				ctx = db.WithIssueTreeSourceSeries(db.WithIssueTreeSourceRoots(ctx, in.Body.SourceRoots), in.Body.SourceSeries)
				roots, err := db.IssueTreeSourceRoots(ctx, r, cfg.DB.InstanceUID())
				if err != nil {
					return nil, issueTreeAPIError(err)
				}
				seriesRoots, err := db.IssueTreeSourceSeries(ctx, r, cfg.DB.InstanceUID())
				if err != nil {
					return nil, issueTreeAPIError(err)
				}
				projects = nil
				for _, root := range roots {
					projects = append(projects, root.ProjectUID)
				}
				for _, root := range seriesRoots {
					projects = append(projects, root.ProjectUID)
				}
			}
			ctx, err = authorizeHostProjectScope(ctx, nil, projects, false)
			if err != nil {
				return nil, err
			}
			out := &api.IssueTreeTransferResponse{}
			switch action {
			case "export":
				roots, err := db.IssueTreeSourceRoots(ctx, r, cfg.DB.InstanceUID())
				if err != nil {
					return nil, issueTreeAPIError(err)
				}
				for _, locator := range roots {
					root, err := cfg.DB.IssueByUID(ctx, locator.IssueUID, db.IncludeDeletedYes)
					if err != nil {
						return nil, issueTreeAPIError(err)
					}
					if err = authorizeIssueScopedIssue(ctx, cfg.DB, root); err != nil {
						return nil, err
					}
				}
				snapshot, err := cfg.DB.ExportIssueTree(ctx, r)
				if err != nil {
					return nil, issueTreeAPIError(err)
				}
				out.Body.Snapshot = &snapshot
			case "accept":
				if in.Body.Snapshot == nil {
					return nil, api.NewError(400, "validation", "snapshot required", "", nil)
				}
				// A scoped grant cannot use an unrestricted destination credential to
				// create bodies outside its existing authorized membership.
				if issueScopeFromContext(ctx) != nil {
					for _, portion := range in.Body.Snapshot.Portions {
						for _, member := range portion.Members {
							issue, err := cfg.DB.IssueByUID(ctx, member.IssueUID, db.IncludeDeletedYes)
							if err != nil {
								return nil, projectAccessDenied()
							}
							if err = authorizeIssueScopedIssue(ctx, cfg.DB, issue); err != nil {
								return nil, err
							}
						}
					}
				}
				result, err := cfg.DB.AcceptIssueTree(ctx, *in.Body.Snapshot, r)
				if err != nil {
					return nil, issueTreeAPIError(err)
				}
				deliverIssueTreeResult(ctx, cfg, result)
				out.Body.Result = &result
			case "lookup":
				result, err := cfg.DB.LookupIssueTreeAcceptance(ctx, r)
				if err != nil {
					return nil, issueTreeAPIError(err)
				}
				if issueScopeFromContext(ctx) != nil {
					for _, applied := range result.Applied {
						issue, err := cfg.DB.IssueByUID(ctx, applied.IssueUID, db.IncludeDeletedYes)
						if err != nil {
							return nil, projectAccessDenied()
						}
						if err = authorizeIssueScopedIssue(ctx, cfg.DB, issue); err != nil {
							return nil, err
						}
					}
				}
				out.Body.Result = &result
			case "finalize":
				if in.Body.Preview == nil || in.Body.Accepted == nil {
					return nil, api.NewError(400, "validation", "preview and accepted evidence required", "", nil)
				}
				target, err := issueTreeTarget(ctx, cfg, r.DestinationAuthorityUID, r.Actor)
				if err != nil {
					return nil, err
				}
				ctx = db.WithMoveDestinationCommitVerifier(ctx, target.verify)
				result, err := cfg.DB.FinalizeIssueTree(ctx, r, *in.Body.Preview, *in.Body.Accepted)
				if err != nil {
					return nil, issueTreeAPIError(err)
				}
				deliverIssueTreeResult(ctx, cfg, result)
				out.Body.Result = &result
			}
			return out, nil
		})
	}
}

func deliverIssueTreeResult(ctx context.Context, cfg ServerConfig, result db.MoveTreeResult) {
	for i := range result.Events {
		deliverProjectMutation(cfg, &result.Events[i])
	}
	recordProjectAccessMutationRevision(ctx, result.ProjectAccessRevision)
}
