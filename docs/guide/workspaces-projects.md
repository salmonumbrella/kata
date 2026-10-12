---
title: Workspaces and projects
description: Understand how workspaces resolve Kata projects and configure local or remote daemon targets.
last_edited: 2026-10-12
---

# Workspaces and projects

kata separates repository files from issue data. Repositories and workspaces
carry only enough information to resolve a project. The database lives under
`KATA_HOME` unless `KATA_DB` points somewhere else.

## Initialize

```sh
kata init
```

In a git workspace, `kata init` derives the project name from the git remote.
Pass `--project` to choose the name explicitly:

```sh
kata init --project product
```

`kata init` writes `.kata.toml` and ensures `.kata.local.toml` is ignored. The
local file is for per-machine settings such as a remote daemon URL.

Inside a Git repository, Kata searches for those files only as far as the
nearest Git root, including when the workspace was entered through a symlink.
A `.kata.toml` above that boundary cannot bind a nested repository or redirect
`kata init` output outside it. Non-Git workspaces retain normal ancestor
discovery.

## Resume project setup after a timeout

Use `--setup` to save the project identity before creating or resolving it:

```sh
kata init --setup --project example-project
```

Kata reuses an existing dedicated project. Otherwise it generates a stable
project UID, then creates that project and attaches the workspace alias in
one database transaction. An alias conflict leaves no new project behind.
Existing name reservations and daemon authorization still apply.

The ignored `.kata.setup-progress.json` stores the destination UID, daemon
authority and route, workspace alias, actor, and request UID. It contains no
credentials. Keep it after an interrupted request. Repeating `kata init`
resumes those inputs and verifies the destination before binding it. A changed
daemon authority, project UID, or workspace alias stops setup with a conflict.
For a named destination, repeat the original `--daemon` selection when its
catalog credential is needed. Setup uses the existing local-profile selector
or remote URL in `.kata.local.toml`; it does not save bearer tokens.

To change an existing project binding, pass `--replace`. If its workspace
alias also needs reassignment, pass `--reassign` explicitly. Setup only detaches
an alias belonging to the saved original project. You can add `--reassign`
on a retry after inspecting an alias conflict without generating another UID.

`--kickoff <ref>` also enables setup and captures the initiating issue's UID
and source authority before changing the workspace binding. Use
`--source-daemon <catalog-name>` when that issue is on another configured
daemon. Source requests use that daemon's own credential.

Kata binds the verified destination, previews the complete initiating issue
tree, and executes the native move. Completed children belong to that tree.
Add `--move-series` to include recurrence definitions and their full families.
Provider ownership defaults to `preserve`; choose `--provider-disposition
detach` explicitly when needed.

The saved complete request survives a lost project-create acknowledgement.
After preview, Kata also saves the preview digest and selected issue UIDs
before execution. Repeating init submits those exact inputs. Success requires
native committed event UID/hash evidence and finalization of every selected
issue. A partial, unknown, or conflict result leaves the workspace routed to
the verified destination, reports the remaining state, and exits nonzero.
Keep the progress file and follow the retry or reconciliation hint; new work
belongs in the destination project.

If `KATA_SERVER` still selects the original source, unset it or point it at
the destination before setup. Kata refuses that conflict before creating the
project, so later workspace commands cannot silently return to the source.

## Bind many workspaces to one project

Use the same project name in each workspace:

```sh
cd ~/code/product
kata init --project product

cd ~/code/product-worktree
kata init --project product
```

Both workspaces now resolve to the same project in the same local kata
database. Issue short IDs, labels, links, and events are shared.

## Run from outside a workspace

Use the global `--workspace` flag:

```sh
kata --workspace ~/code/product ready --unowned
kata --workspace ~/code/product show abc4
```

This is useful for scripts, cron jobs, and agents that keep their own working
directory.

## Project commands

List and inspect projects:

```sh
kata projects list
kata projects show product
```

Create a project by name without binding the current directory:

```sh
kata projects create research
```

`projects create` talks to the daemon only. It creates or returns the active
project named `research` without writing `.kata.toml`, `.gitignore`, or agent
guidance files, and without attaching a git alias. Use it for projects that do
not map one-to-one to a repository workspace, then run `kata init --project
research` later from any workspace that should resolve to the same project.

Rename a project:

```sh
kata projects rename product platform
```

Merge accidental duplicates:

```sh
kata projects merge old-repo new-repo --rename-target new-repo
```

Archive and restore a project:

```sh
kata projects remove old-lab
kata projects restore old-lab
```

`projects remove` hides the project from normal resolution but preserves events
for audit. The project name stays reserved; `kata init` with the same name is
rejected while the project is archived.

Permanently delete an archived project:

```sh
kata projects purge old-lab --force --confirm "PURGE old-lab"
```

`projects purge` is irreversible. It deletes all project data and frees the
name so `kata init --project old-lab` works again afterward.

The project must be archived first (`kata projects remove`). Purging an active
project fails with `project_not_archived`.

Two flags are required to prevent accidental data loss:

- `--force` must be present.
- `--confirm` must be the exact string `PURGE <project>` (e.g.
  `"PURGE old-lab"`). A wrong or missing value is rejected.

An optional `--reason` records a free-text note in the audit tombstone:

```sh
kata projects purge old-lab --force --confirm "PURGE old-lab" \
  --reason "decommissioned after team reorg"
```

Pass `--json` to receive the audit tombstone with row counts as JSON.

**Federation**: a project with a federation binding cannot be purged.

- If the project is a **spoke**, run `kata federation leave old-lab` first.
- If the project is a **hub**, purge is not currently supported. Hub teardown
  must be handled manually before the project can be purged.

!!! warning
    `projects purge` cannot be undone. Confirm you have the right project with
    `kata projects show <project>` *before* you `kata projects remove` it
    (`projects show` resolves active projects only). Once archived, the
    `PURGE <project>` confirmation string is your last check before deletion.

Detach one alias when a workspace identity was attached to the wrong project:

```sh
kata projects detach github.com/example/wrong
```

Use `kata projects show <project>` before destructive or structural project
operations so you know which project and aliases are affected.

## `.kata.toml`

The committed binding file is intentionally small:

```toml
version = 1

[project]
name = "product"
```

Do not put tokens or host-specific daemon URLs in `.kata.toml`.

## `.kata.local.toml`

The local override file is ignored by git. A common use is routing one
workspace to a remote daemon:

```toml
version = 1

[server]
url = "http://100.64.0.5:7777"
```

`KATA_SERVER` wins over `.kata.local.toml` when both are set.

## Non-git workspaces

kata works without git. Use an explicit project name:

```sh
mkdir ~/scratch/research
cd ~/scratch/research
kata init --project research
```

The issue model does not depend on git commits. Git is only one way to derive a
default project name and one possible source of close evidence.
