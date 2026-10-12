package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
	"go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/httpurl"
	"go.kenn.io/kata/pkg/client/generated"
	"go.kenn.io/kit/atomicfile"
)

func validateInitSetupRoute(route initSetupRoute) error {
	if override := strings.TrimSpace(os.Getenv("KATA_SERVER")); override != "" {
		actual, err := httpurl.CanonicalHTTPBaseURL(override)
		if err != nil || actual != route.URL {
			return &cliError{Message: "KATA_SERVER would route later workspace commands away from the saved destination", Hint: "unset KATA_SERVER or select the saved destination URL before retrying setup", Code: "setup_route_conflict", Kind: kindConflict, ExitCode: ExitConflict}
		}
	}
	return nil
}

// Reassignment uses existing attributed alias administration only when the
// operator explicitly requested it and the alias belongs to the saved binding.
// Creation plus attachment still use the strict atomic stable-UID API.
func reassignInitSetupAlias(ctx context.Context, c initSetupClient, p initSetupProgress, in localInit) error {
	if !p.Reassign || in.Alias == nil || p.PreviousBinding == nil || p.PreviousBinding.ProjectUID == "" || p.PreviousBinding.ProjectUID == p.Destination.ProjectUID {
		return nil
	}
	list, callErr := c.api.ListProjectsWithResponse(ctx, &generated.ListProjectsRequestOptions{})
	if list == nil {
		return externalCLITransportError(list, callErr)
	}
	if err := externalCLIResponseError(list.StatusCode, list.Body, callErr); err != nil {
		return err
	}
	if list.JSON200 == nil {
		return fmt.Errorf("daemon returned no project catalog")
	}
	var projectID int64
	for _, project := range list.JSON200.Projects {
		if project.UID == p.PreviousBinding.ProjectUID {
			projectID = project.ID
			break
		}
	}
	if projectID == 0 {
		return fmt.Errorf("original project binding is unavailable; reconcile its alias before retrying setup")
	}
	response, callErr := c.api.ShowProjectWithResponse(ctx, &generated.ShowProjectRequestOptions{PathParams: &generated.ShowProjectPath{ProjectID: projectID}})
	if response == nil {
		return externalCLITransportError(response, callErr)
	}
	if err := externalCLIResponseError(response.StatusCode, response.Body, callErr); err != nil {
		return err
	}
	if response.JSON200 == nil || response.JSON200.Project.UID != p.PreviousBinding.ProjectUID {
		return initSetupAuthorityConflict()
	}
	for _, alias := range response.JSON200.Aliases {
		if alias.AliasIdentity != in.Alias.Identity || alias.AliasKind != in.Alias.Kind {
			continue
		}
		force := true
		detached, callErr := c.api.DetachProjectAliasWithResponse(ctx, &generated.DetachProjectAliasRequestOptions{PathParams: &generated.DetachProjectAliasPath{ProjectID: projectID, AliasID: alias.ID}, Query: &generated.DetachProjectAliasQuery{Actor: p.Actor, Force: &force}})
		if detached == nil {
			return externalCLITransportError(detached, callErr)
		}
		return externalCLIResponseError(detached.StatusCode, detached.Body, callErr)
	}
	return nil // A retry may find it already detached or attached at destination.
}

func bindInitSetupRoute(ctx context.Context, dir, name string, route initSetupRoute) error {
	// Default owner-local runtime selection is already pinned by KATA_HOME.
	// Named profiles instead persist the existing server.daemon selector.
	if selected, ok := ctx.Value(resolvedDaemonContextKey{}).(client.ResolvedDaemon); ok && selected.Source == client.DaemonSourceLocalRuntime && route.Daemon == "" {
		return nil
	}
	if strings.HasPrefix(route.URL, "unix://") && route.Daemon == "" {
		return nil
	}
	path := filepath.Join(dir, config.LocalConfigFilename)
	settings := map[string]any{}
	bs, err := os.ReadFile(path)
	if err == nil {
		if _, err := toml.Decode(string(bs), &settings); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	settings["version"] = int64(1)
	server, _ := settings["server"].(map[string]any)
	if server == nil {
		server = map[string]any{}
	}
	selected, _ := ctx.Value(resolvedDaemonContextKey{}).(client.ResolvedDaemon)
	if route.Daemon != "" && selected.LocalProfile != nil {
		delete(server, "url")
		server["daemon"] = route.Daemon
	} else {
		delete(server, "daemon")
		server["url"] = route.URL
	}
	if selected.AllowInsecure {
		server["allow_insecure"] = true
	}
	settings["server"] = server
	if project, ok := settings["project"].(map[string]any); ok {
		if _, exists := project["name"]; exists {
			project["name"] = name
		}
	}
	var out bytes.Buffer
	if err := toml.NewEncoder(&out).Encode(settings); err != nil {
		return err
	}
	return atomicfile.WriteFile(path, out.Bytes(), atomicfile.WithPrivate())
}

func writeInitSetupProjectBinding(dir, name string) error {
	if err := config.ValidateProjectName(name); err != nil {
		return err
	}
	bs := []byte(fmt.Sprintf("version = 1\n\n[project]\nname = %q\n", name))
	return atomicfile.WriteFile(filepath.Join(dir, config.ProjectConfigFilename), bs, atomicfile.WithPerm(0644), atomicfile.WithPreserveMode())
}

func initSetupInputConflict() error {
	return &cliError{Message: "saved setup inputs differ from this invocation; reconcile the existing progress before starting another setup", Code: "setup_input_conflict", Kind: kindConflict, ExitCode: ExitConflict}
}
func initSetupAuthorityConflict() error {
	return &cliError{Message: "saved setup targets a different daemon authority; restore its original route before retrying", Code: "setup_authority_conflict", Kind: kindConflict, ExitCode: ExitConflict}
}
