package daemon

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

func registerIssueTreeMoveHandlers(humaAPI huma.API, cfg ServerConfig) {
	registerIssueTreeTransferHandlers(humaAPI, cfg)
	huma.Register(humaAPI, huma.Operation{OperationID: "moveIssueTree", Method: http.MethodPost, Path: "/api/v1/issue-moves"}, func(ctx context.Context, in *api.IssueTreeMoveRequest) (*api.IssueTreeMoveResponse, error) {
		r := in.Body.Request
		actor, err := attributedActor(ctx, r.Actor)
		if err != nil {
			return nil, err
		}
		r.Actor = actor
		r, err = r.Normalize()
		if err != nil {
			return nil, api.NewError(400, "validation", err.Error(), "", nil)
		}
		projects := []string{r.SourceProjectUID}
		if r.DestinationAuthorityUID == cfg.DB.InstanceUID() {
			projects = append(projects, r.DestinationProjectUID)
		}
		ctx, err = authorizeHostProjectScope(ctx, nil, projects, false)
		if err != nil {
			return nil, err
		}
		if r.DestinationAuthorityUID != cfg.DB.InstanceUID() {
			return coordinateIssueTreeHTTP(ctx, cfg, r, in.Body.DryRun)
		}
		root, err := cfg.DB.IssueByUID(ctx, r.IssueUID, db.IncludeDeletedYes)
		if err != nil {
			return nil, issueTreeAPIError(err)
		}
		if err := authorizeIssueScopedIssue(ctx, cfg.DB, root); err != nil {
			return nil, err
		}
		out := &api.IssueTreeMoveResponse{}
		if in.Body.DryRun {
			preview, err := cfg.DB.PreviewIssueTree(ctx, r)
			if err != nil {
				return nil, issueTreeAPIError(err)
			}
			out.Body.Preview = &preview
			return out, nil
		}
		result, err := cfg.DB.MoveIssueTree(ctx, r)
		if err != nil {
			return nil, issueTreeAPIError(err)
		}
		for i := range result.Events {
			deliverProjectMutation(cfg, &result.Events[i])
		}
		recordProjectAccessMutationRevision(ctx, result.ProjectAccessRevision)
		out.Body.Result = &result
		return out, nil
	})
}

func issueTreeAPIError(err error) error {
	if remote, ok := errors.AsType[*db.MoveRemoteSelectionError](err); ok {
		return api.NewError(409, "remote-selection-required", remote.Error(), "collect all authorized portions through the issue-moves coordinator", map[string]any{"required": remote.Required, "required_series": remote.RequiredSeries})
	}
	if errors.Is(err, db.ErrNotFound) {
		return api.NewError(404, "issue_move_unavailable", "selected work is unavailable", "", nil)
	}
	if errors.Is(err, db.ErrMoveConflict) || errors.Is(err, db.ErrMovePreviewStale) {
		return api.NewError(409, "issue_move_conflict", err.Error(), "refresh the complete selection or reconcile the retained copies", nil)
	}
	if errors.Is(err, db.ErrMoveAuthorityMismatch) {
		return api.NewError(409, "issue_move_authority_mismatch", err.Error(), "resolve the configured source and destination authorities", nil)
	}
	if apiErr := federationReadOnlyError(err); apiErr != nil {
		return apiErr
	}
	return internalAPIError(err)
}
