package main

import (
	"context"
	"fmt"

	"go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/uid"
	"go.kenn.io/kata/pkg/client/generated"
)

// Captured source identity is saved before workspace or alias resolution changes.
// This is setup input, not a server move receipt or a recovery decision.
type initSetupSource struct {
	Ref          string         `json:"ref"`
	IssueUID     string         `json:"issue_uid"`
	ProjectUID   string         `json:"project_uid"`
	AuthorityUID string         `json:"authority_uid"`
	Route        initSetupRoute `json:"route"`
}
type initSetupPreviousBinding struct {
	Name       string `json:"name"`
	ProjectUID string `json:"project_uid,omitempty"`
}

func initSetupRouteForContext(ctx context.Context, baseURL string) initSetupRoute {
	route := initSetupRoute{URL: baseURL}
	if selected, ok := ctx.Value(resolvedDaemonContextKey{}).(client.ResolvedDaemon); ok && selected.BaseURL == baseURL {
		route.Daemon = selected.Name
	}
	return route
}

func initSetupSourceClient(ctx context.Context, currentURL, name string) (initSetupClient, initSetupRoute, error) {
	if name == "" {
		c, err := newInitSetupClient(ctx, currentURL)
		return c, initSetupRouteForContext(ctx, currentURL), err
	}
	selected, err := client.PrepareResolvedNamed(ctx, name)
	if err != nil {
		return initSetupClient{}, initSetupRoute{}, cliDaemonTargetError(err)
	}
	// A secondary source must use its own catalog credential. The ordinary
	// CLI's global token override belongs to its selected destination.
	cat, err := config.ReadDaemonCatalogAndAuthPolicy()
	if err != nil {
		return initSetupClient{}, initSetupRoute{}, err
	}
	entry := catalogByName(cat, name)
	if entry == nil {
		return initSetupClient{}, initSetupRoute{}, fmt.Errorf("source daemon %q is no longer configured", name)
	}
	selected.Token, _, err = config.ResolveSecret(entry.Token, "", entry.TokenEnv)
	if err != nil {
		return initSetupClient{}, initSetupRoute{}, err
	}
	if entry.Local && selected.Token == "" {
		home, homeErr := config.KataHome()
		if selected.LocalProfile != nil {
			home, homeErr = selected.LocalProfile.Home, nil
		}
		if homeErr != nil {
			return initSetupClient{}, initSetupRoute{}, homeErr
		}
		local, err := config.ReadDaemonConfigForHome(home)
		if err != nil {
			return initSetupClient{}, initSetupRoute{}, err
		}
		selected.Token = local.Auth.Token
		if selected.Token == "" {
			selected.Token, err = config.ReadPersistedAuthToken(home)
			if err != nil {
				return initSetupClient{}, initSetupRoute{}, err
			}
		}
	}
	sourceCtx := context.WithValue(ctx, resolvedDaemonContextKey{}, selected)
	c, err := newInitSetupClient(sourceCtx, selected.BaseURL)
	return c, initSetupRoute{URL: selected.BaseURL, Daemon: name}, err
}

func captureInitKickoff(ctx context.Context, baseURL, startPath string, opts callInitOpts) (*initSetupSource, error) {
	if opts.Kickoff == "" {
		return nil, nil
	}
	source, route, err := initSetupSourceClient(ctx, baseURL, opts.SourceDaemon)
	if err != nil {
		return nil, err
	}
	authority, err := source.instanceUID(ctx)
	if err != nil {
		return nil, err
	}
	var issueUID, projectUID string
	if uid.Valid(opts.Kickoff) {
		response, callErr := source.api.ShowIssueByUIDWithResponse(ctx, &generated.ShowIssueByUIDRequestOptions{PathParams: &generated.ShowIssueByUIDPath{UID: opts.Kickoff}})
		if response == nil {
			return nil, externalCLITransportError(response, callErr)
		}
		if err := externalCLIResponseError(response.StatusCode, response.Body, callErr); err != nil {
			return nil, err
		}
		if response.JSON200 == nil {
			return nil, fmt.Errorf("source daemon returned no kickoff issue")
		}
		issueUID = response.JSON200.Issue.UID
		project, callErr := source.api.ShowProjectWithResponse(ctx, &generated.ShowProjectRequestOptions{PathParams: &generated.ShowProjectPath{ProjectID: response.JSON200.Issue.ProjectID}})
		if project == nil {
			return nil, externalCLITransportError(project, callErr)
		}
		if err := externalCLIResponseError(project.StatusCode, project.Body, callErr); err != nil {
			return nil, err
		}
		if project.JSON200 == nil {
			return nil, fmt.Errorf("source daemon returned no kickoff project")
		}
		projectUID = project.JSON200.Project.UID
	} else {
		parsed, err := ResolveRef(opts.Kickoff, workspaceProjectName(startPath))
		if err != nil {
			return nil, &cliError{Message: err.Error(), Kind: kindValidation, ExitCode: ExitValidation}
		}
		project, err := source.projectByName(ctx, parsed.ProjectName)
		if err != nil {
			return nil, err
		}
		response, callErr := source.api.ShowIssueWithResponse(ctx, &generated.ShowIssueRequestOptions{PathParams: &generated.ShowIssuePath{ProjectID: project.ID, Ref: parsed.RefForAPI}})
		if response == nil {
			return nil, externalCLITransportError(response, callErr)
		}
		if err := externalCLIResponseError(response.StatusCode, response.Body, callErr); err != nil {
			return nil, err
		}
		if response.JSON200 == nil {
			return nil, fmt.Errorf("source daemon returned no kickoff issue")
		}
		issueUID = response.JSON200.Issue.UID
		projectUID = project.UID
	}
	if !uid.Valid(issueUID) || !uid.Valid(projectUID) {
		return nil, fmt.Errorf("source daemon returned invalid kickoff identity")
	}
	return &initSetupSource{Ref: opts.Kickoff, IssueUID: issueUID, ProjectUID: projectUID, AuthorityUID: authority, Route: route}, nil
}

func validateInitKickoffSource(ctx context.Context, baseURL string, p initSetupProgress, opts callInitOpts) error {
	if p.Source == nil {
		return nil
	}
	if opts.Kickoff != "" && opts.Kickoff != p.Source.Ref && opts.Kickoff != p.Source.IssueUID {
		return initSetupInputConflict()
	}
	if opts.SourceDaemon != "" && opts.SourceDaemon != p.Source.Route.Daemon {
		return initSetupInputConflict()
	}
	source, route, err := initSetupSourceClient(ctx, baseURL, p.Source.Route.Daemon)
	if err != nil {
		return err
	}
	if route.URL != p.Source.Route.URL {
		return initSetupAuthorityConflict()
	}
	actual, err := source.instanceUID(ctx)
	if err != nil {
		return err
	}
	if actual != p.Source.AuthorityUID {
		return initSetupAuthorityConflict()
	}
	return nil
}
