package daemon

import (
	"context"
	"errors"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
)

// setupProject preserves init's authorization checks while delegating the
// stable identity and workspace attachment decision to one native writer.
func setupProject(ctx context.Context, store db.Storage, req *api.InitProjectRequest, name string, alias config.AliasInfo, actor string) (*api.ProjectResolveBody, projectInitMutation, bool, error) {
	params := db.SetupProjectParams{Name: name, ProjectUID: req.Body.ProjectUID, AuthorityUID: req.Body.AuthorityUID, AliasIdentity: alias.Identity, AliasKind: alias.Kind, Actor: actor}
	if err := db.ValidateProjectSetup(params); err != nil {
		return nil, projectInitMutation{}, false, api.NewError(400, "validation", err.Error(), "", nil)
	}
	for _, lookup := range []func() (db.Project, error){
		func() (db.Project, error) { return store.ProjectByUID(ctx, params.ProjectUID) },
		func() (db.Project, error) { return store.ProjectByNameIncludingArchived(ctx, name) },
	} {
		project, err := lookup()
		if err == nil {
			if err := authorizeIssueScopedProject(ctx, project); err != nil {
				return nil, projectInitMutation{}, false, err
			}
		} else if !errors.Is(err, db.ErrNotFound) {
			return nil, projectInitMutation{}, false, internalAPIError(err)
		}
	}
	if alias.Identity != "" {
		existing, err := store.AliasByIdentity(ctx, alias.Identity)
		if err == nil {
			project, err := store.ProjectByID(ctx, existing.ProjectID)
			if err != nil {
				return nil, projectInitMutation{}, false, internalAPIError(err)
			}
			if err := authorizeIssueScopedProject(ctx, project); err != nil {
				return nil, projectInitMutation{}, false, err
			}
		} else if !errors.Is(err, db.ErrNotFound) {
			return nil, projectInitMutation{}, false, internalAPIError(err)
		}
	}
	result, err := store.SetupProject(ctx, params)
	if err != nil {
		switch {
		case errors.Is(err, db.ErrProjectNameRetained):
			return nil, projectInitMutation{}, false, api.NewError(409, "project_name_retained", err.Error(), "choose a name outside the retained project namespace", nil)
		case errors.Is(err, db.ErrProjectSetupAuthority):
			return nil, projectInitMutation{}, false, api.NewError(409, "project_authority_conflict", err.Error(), "resolve the destination daemon before retrying", nil)
		case errors.Is(err, db.ErrProjectSetupConflict):
			return nil, projectInitMutation{}, false, api.NewError(409, "project_uid_conflict", err.Error(), "choose the existing project UID or a different name", nil)
		case errors.Is(err, db.ErrAliasExists):
			return nil, projectInitMutation{}, false, api.NewError(409, "project_alias_conflict", err.Error(), "choose an alias belonging to this project", nil)
		case errors.Is(err, db.ErrProjectAlreadyArchived):
			return nil, projectInitMutation{}, false, api.NewError(409, "project_archived", err.Error(), "restore the project before retrying", nil)
		default:
			return nil, projectInitMutation{}, false, internalAPIError(err)
		}
	}
	if result.Event != nil {
		recordProjectAccessCatalogMutation(ctx, result.Project.UID)
	}
	return &api.ProjectResolveBody{Project: dbProjectToOut(result.Project), Alias: result.Alias}, projectInitMutation{Event: result.Event, ProjectID: result.Project.ID}, result.Event != nil, nil
}
