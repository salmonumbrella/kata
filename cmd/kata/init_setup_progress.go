package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/uid"
	"go.kenn.io/kit/atomicfile"
)

const setupProgressFilename = ".kata.setup-progress.json"
const setupProgressLockFilename = ".kata.setup-progress.lock"

// Setup progress contains saved client inputs, never move decisions or tokens.
type initSetupRoute struct {
	URL    string `json:"url"`
	Daemon string `json:"daemon,omitempty"`
}
type initSetupDestination struct {
	Name         string         `json:"name"`
	ProjectUID   string         `json:"project_uid"`
	AuthorityUID string         `json:"authority_uid"`
	Route        initSetupRoute `json:"route"`
}
type initSetupProgress struct {
	KickoffRequest  *initSetupKickoffRequest  `json:"kickoff_request,omitempty"`
	Alias           *initSetupAlias           `json:"alias,omitempty"`
	Source          *initSetupSource          `json:"source,omitempty"`
	PreviousBinding *initSetupPreviousBinding `json:"previous_binding,omitempty"`
	Replace         bool                      `json:"replace"`
	Reassign        bool                      `json:"reassign"`
	Destination     initSetupDestination      `json:"destination"`
	Actor           string                    `json:"actor"`
	RequestUID      string                    `json:"request_uid"`
}

type initSetupAlias struct {
	Identity string `json:"identity"`
	Kind     string `json:"kind"`
}

func initSetupAliasFor(in localInit) *initSetupAlias {
	if in.Alias == nil {
		return nil
	}
	return &initSetupAlias{Identity: in.Alias.Identity, Kind: in.Alias.Kind}
}

func loadInitSetupProgress(path string) (initSetupProgress, error) {
	var p initSetupProgress
	bs, err := os.ReadFile(path)
	if err != nil {
		return p, err
	}
	if err := json.Unmarshal(bs, &p); err != nil {
		return p, fmt.Errorf("decode saved setup progress: %w", err)
	}
	if !uid.Valid(p.Destination.ProjectUID) || !uid.Valid(p.Destination.AuthorityUID) || !uid.Valid(p.RequestUID) || p.Destination.Route.URL == "" {
		return p, fmt.Errorf("saved setup progress has invalid identity; preserve it for reconciliation")
	}
	if p.Source != nil && (!uid.Valid(p.Source.IssueUID) || !uid.Valid(p.Source.ProjectUID) || !uid.Valid(p.Source.AuthorityUID) || p.Source.Route.URL == "") {
		return p, fmt.Errorf("saved kickoff identity is invalid; preserve it for reconciliation")
	}
	if err := config.ValidateProjectName(p.Destination.Name); err != nil {
		return p, err
	}
	if err := validateSavedInitKickoffRequest(p); err != nil {
		return p, err
	}
	return p, nil
}

func saveInitSetupProgress(path string, p initSetupProgress) error {
	bs, err := json.Marshal(p, json.Deterministic(true))
	if err != nil {
		return err
	}
	return atomicfile.WriteFile(path, append(bs, '\n'), atomicfile.WithPrivate())
}

// The client lock serializes worktree retries. No native writer is held while
// the client performs HTTP reads or writes.
func runSetupInit(ctx context.Context, baseURL string, in localInit, opts callInitOpts) (string, error) {
	dest := config.WriteDestination(in.Disc, in.StartPath)
	lock := flock.New(filepath.Join(dest, setupProgressLockFilename))
	locked, err := lock.TryLockContext(ctx, 25*time.Millisecond)
	if err != nil {
		return "", err
	}
	if !locked {
		return "", errors.New("setup progress is being updated; retry kata init")
	}
	defer func() { _ = lock.Unlock() }()
	for _, name := range []string{setupProgressFilename, setupProgressLockFilename} {
		if _, err := ensureGitignoreEntry(dest, name); err != nil {
			return "", fmt.Errorf("ignore setup progress before creation: %w", err)
		}
	}
	api, err := newInitSetupClient(ctx, baseURL)
	if err != nil {
		return "", err
	}
	authority, err := api.instanceUID(ctx)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dest, setupProgressFilename)
	progress, err := loadInitSetupProgress(path)
	if errors.Is(err, os.ErrNotExist) {
		project, lookupErr := api.projectByName(ctx, in.Choice.Name)
		if lookupErr != nil {
			var cliErr *cliError
			if !errors.As(lookupErr, &cliErr) || cliErr.Kind != kindNotFound {
				return "", lookupErr
			}
		}
		projectUID := project.UID
		if projectUID == "" {
			projectUID, err = uid.New()
			if err != nil {
				return "", err
			}
		}
		requestUID, err := uid.New()
		if err != nil {
			return "", err
		}

		source, err := captureInitKickoff(ctx, baseURL, in.StartPath, opts)
		if err != nil {
			return "", err
		}
		var previous *initSetupPreviousBinding
		if in.ExistingToml != nil {
			previous = &initSetupPreviousBinding{Name: in.ExistingToml.Project.Name}
			original, lookupErr := api.projectByName(ctx, previous.Name)
			if lookupErr == nil {
				previous.ProjectUID = original.UID
			} else {
				var ce *cliError
				if !errors.As(lookupErr, &ce) || ce.Kind != kindNotFound {
					return "", lookupErr
				}
			}
		}
		progress = initSetupProgress{Destination: initSetupDestination{Name: in.Choice.Name, ProjectUID: projectUID, AuthorityUID: authority, Route: initSetupRouteForContext(ctx, baseURL)}, Actor: opts.Actor, RequestUID: requestUID, Source: source, Alias: initSetupAliasFor(in), PreviousBinding: previous, Replace: opts.Replace, Reassign: opts.Reassign}
		if _, err := prepareInitKickoffRequest(&progress, opts); err != nil {
			return "", err
		}

		if err := saveInitSetupProgress(path, progress); err != nil {
			return "", fmt.Errorf("save setup identity before creation: %w", err)
		}
	} else if err != nil {
		return "", err
	}
	if progress.Destination.Route.URL != baseURL || progress.Destination.AuthorityUID != authority {
		return "", initSetupAuthorityConflict()
	}
	if progress.Destination.Name != in.Choice.Name || progress.Actor != opts.Actor {
		return "", initSetupInputConflict()
	}
	alias := initSetupAliasFor(in)
	if (progress.Alias == nil) != (alias == nil) || (alias != nil && *alias != *progress.Alias) {
		return "", initSetupInputConflict()
	}

	if progress.Source == nil && opts.Kickoff != "" {
		captureOpts := opts
		if !uid.Valid(opts.Kickoff) && !strings.Contains(opts.Kickoff, "#") {
			if progress.PreviousBinding == nil || progress.PreviousBinding.Name == "" {
				return "", &cliError{Message: "a resumed bare kickoff has no original workspace binding", Kind: kindValidation, ExitCode: ExitValidation, Hint: "use an explicit project#short_id or full issue UID"}
			}
			captureOpts.Kickoff = progress.PreviousBinding.Name + "#" + opts.Kickoff
		}
		source, err := captureInitKickoff(ctx, baseURL, in.StartPath, captureOpts)
		if err != nil {
			return "", err
		}
		source.Ref = opts.Kickoff
		progress.Source = source
		if err := saveInitSetupProgress(path, progress); err != nil {
			return "", err
		}
	}
	if err := validateInitKickoffSource(ctx, baseURL, progress, opts); err != nil {
		return "", err
	}
	if err := validateInitSetupRoute(progress.Destination.Route); err != nil {
		return "", err
	}
	if changed, err := prepareInitKickoffRequest(&progress, opts); err != nil {
		return "", err
	} else if changed {
		if err := saveInitSetupProgress(path, progress); err != nil {
			return "", err
		}
	}
	// An alias conflict can be retried with explicit operator consent while
	// retaining the previously saved destination and request identities.
	if opts.Reassign && !progress.Reassign {
		progress.Reassign = true
		if err := saveInitSetupProgress(path, progress); err != nil {
			return "", err
		}
	}
	if err := reassignInitSetupAlias(ctx, api, progress, in); err != nil {
		return "", err
	}
	in.SetupRoute = &progress.Destination.Route
	in.ProjectUID = progress.Destination.ProjectUID
	in.AuthorityUID = progress.Destination.AuthorityUID

	out, err := runNameInit(ctx, baseURL, in, opts)
	if err != nil {
		return out, err
	}
	if progress.Source != nil {
		return runInitSetupKickoff(ctx, baseURL, path, progress, out)
	}
	return out, nil
}

// Saved inputs must be consulted before name derivation: a lost create reply
// can leave the workspace unbound, or still bound to its original project.
func initSetupOptionsFromProgress(startPath string, opts callInitOpts) (callInitOpts, error) {
	disc, err := config.DiscoverPaths(startPath)
	if err != nil {
		return opts, err
	}
	path := filepath.Join(config.WriteDestination(disc, startPath), setupProgressFilename)
	progress, err := loadInitSetupProgress(path)
	if (opts.SourceDaemon != "" || opts.MoveSeries || opts.MoveSeriesSet || opts.ProviderDisposition != "") && opts.Kickoff == "" && (errors.Is(err, os.ErrNotExist) || err == nil && progress.Source == nil) {
		return opts, &cliError{Message: "kickoff options require --kickoff or saved kickoff progress", Kind: kindUsage, ExitCode: ExitUsage}
	}
	if errors.Is(err, os.ErrNotExist) {
		return opts, nil
	}
	if err != nil {
		return opts, &cliError{Message: err.Error(), Code: "setup_progress_invalid", Kind: kindConflict, ExitCode: ExitConflict}
	}
	opts.Setup = true

	if progress.Replace && progress.PreviousBinding != nil {
		cfg, err := config.ReadProjectConfig(config.WriteDestination(disc, startPath))
		if err == nil && cfg.Project.Name == progress.PreviousBinding.Name {
			opts.Replace = true
		}
	}
	if opts.Project == "" {
		opts.Project = progress.Destination.Name
	}
	return opts, nil
}
