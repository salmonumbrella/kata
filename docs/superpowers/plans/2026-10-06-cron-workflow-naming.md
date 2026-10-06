# Cron Workflow Naming Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [x]`) syntax for tracking.

**Goal:** Use jobs, workflows, and runs consistently throughout native cron, including its source code and public contracts.

**Architecture:** Keep `kata cron`. Rename the reusable stepped definition from flow to workflow across the CLI, HTTP API, Go and TypeScript models, database storage, event replay, federation, and JSONL. Issue responsibility continues to use claim/owner; federation retains lease for exclusive coordination.

**Tech Stack:** Go, Cobra, SQLite, PostgreSQL, Huma/OpenAPI, generated Go and TypeScript clients, Markdown.

**Spec:** The approved naming decision is reproduced below; the user explicitly requested this plan followed by implementation, allowing breaking changes.

## Naming contract

| Concept | Name |
| --- | --- |
| Configured work and its trigger/action | Job |
| Reusable command/prompt steps and their dependencies | Workflow |
| One execution and its reported results | Run |
| Issue responsibility | Claim / owner |
| Exclusive federation hold | Lease |

Canonical names become `kata cron workflow`, `/cron/workflows/{cron_uid}`, `workflow`/`workflows` response keys, `workflow_uid`, `workflow_definition_event_uid`, `cron.workflow.*` events, `cron_workflow` JSONL records, and `Workflow` Go/TypeScript identifiers. Jobs and runs keep their names. Old automation flow spellings receive no compatibility aliases. Ordinary prose about control flow, unrelated workflows, and historical incompatible-schema fixtures are outside the rename.

## Global Constraints

- Keep the `kata cron` command group and `cron_v1` feature name.
- Preserve behavior, attribution, project isolation, revision checks, number precision, independent run identities, and federation origin rules.
- No new scheduler, execution ownership, or reservation mechanism.
- Do not change existing claim/owner or federation lease semantics.
- Do not modify another worktree or unrelated local changes.
- Commit completed work without squashing or amending.
- The user explicitly approved the full SQLite/PostgreSQL persisted-state inventory in this conversation before implementation.

## Persisted-state inventory

For both SQLite and PostgreSQL, rename `cron_flows` to `cron_workflows`, `idx_cron_flows_project_name` to `idx_cron_workflows_project_name`, and `cron_runs.flow_uid` / `flow_definition_event_uid` to `workflow_uid` / `workflow_definition_event_uid`. PostgreSQL's generated sequence and constraint names follow the renamed table/columns. Rename corresponding stored JSON keys, event names, snapshot fields, and JSONL record kinds.

Update both canonical schemas, schema manifests/validation, SQL queries, and the feature-only PostgreSQL `000031_native_cron.up.sql` together. `origin/main` has no migration 31 at plan creation; recheck before editing and committing. Keep schema version 31 and migrations 26–30 unchanged. Older experimental schema-31 databases and exports require their matching old binary and are not silently converted. Preserve historical fixtures to prove refusal. No live database operations are part of this plan.

## Review Focus

1. Workflow CRUD is reachable through CLI and HTTP, including revision conflicts and delete/restore.
2. Jobs and runs retain workflow references through storage, federation, and export/import.
3. Canonical PostgreSQL installs and real 30→31 upgrades match, while incompatible experimental databases are refused.
4. Generated clients and opaque JSON payloads use the new spelling without losing integer precision.
5. No accidental changes to issue claims, federation leases, generic control-flow wording, or the `kata cron` group.

---

### Task 1: Pin the new public names with failing behavior tests

**Files:** `cmd/kata/cron_test.go`, `internal/cron/validate_test.go`, `internal/daemon/handlers_cron_test.go` as needed.

**Interfaces:** Consumes the existing cron CRUD implementation; produces behavioral tests for the new command, route, envelope, and reference names without requiring renamed Go symbols to compile first.

- [x] Run the existing cron parser and CLI CRUD tests as a baseline.
- [x] Update the existing CLI CRUD test to invoke `workflow` and decode the `workflow` response field. Keep its create/update/conflict/delete/list/restore assertions.
- [x] Add a parser test that accepts an execute action with `workflow_uid` and rejects the retired `flow_uid` field; inspect actual marshaled keys without referring to a renamed Go field yet.
- [x] Run the targeted tests and confirm failures are the missing command and rejected new JSON field.

### Task 2: Rename the complete native cron contract

**Files:** `internal/cron/*`, `internal/api/cron*.go`, `cmd/kata/cron*.go`, `internal/daemon/*cron*.go`, `internal/daemon/host_operation_policy_tasks.go`, `internal/db/cron*.go`, `internal/db/fold*.go`, `internal/db/import*.go`, `internal/db/federation*.go`, `internal/db/storage.go`, both storage implementations and schemas, `internal/federation/*`, `internal/jsonl/*`, associated tests, `pkg/client/cron*.go`, `pkg/client/generated/templates/type_def.tmpl`, `api/openapi.yaml`, `pkg/client/openapi.yaml`, `pkg/client/generated/*`, `web/src/lib/api/generated/*`.

**Interfaces:** Produces `cron.WorkflowDefinition`, `cron.WorkflowStep`, `cron.ParseWorkflow`, `db.CronWorkflow`, `db.PutCronWorkflow`, `WorkflowUID`, `WorkflowDefinitionEventUID`, matching storage/client methods and the canonical names above.

- [x] Obtain explicit approval for the persisted-state inventory before edits to it.
- [x] Rename native-cron identifiers, resource strings, payload keys, SQL objects, event/snapshot/export names, and fixtures for the current contract. Preserve historical rejected-schema fixtures and immutable migrations.
- [x] Regenerate OpenAPI and Go clients with `make api-generate`, and TypeScript clients with `make web-generate`; do not retain hand-edited generated output.
- [x] Run the Task 1 tests to GREEN, then cron/client/JSONL tests and SQLite/PostgreSQL conformance for definitions, observations, references, replay, merge, and migration.
- [x] Audit remaining flow references and check all Go packages compile.
- [x] Commit the coherent code, schema, generated artifacts, and tests.

### Task 3: Explain the vocabulary and verify the complete change

**Files:** `docs/reference/cron.md`, `docs/reference/cli.md`, `docs/reference/go-client.md`, `docs/design/native-cron-storage.md`, `docs/development/postgres-migrations.md`, and any maintained cron references found by the audit.

**Interfaces:** Consumes the canonical Task 2 commands/types and documents the same contract for users and adapter authors.

- [x] Define job, workflow, and run together; show workflow commands and explain claim/owner versus federation lease using existing behavior.
- [x] Update examples, storage notes, and document the deliberate breaking rename and experimental schema-31 compatibility limit. Update edited-page dates where required.
- [x] Run `make api-check`, `make web-check`, `make docs-check`, lint, and the repository Go suite (`make test`), using an isolated PostgreSQL instance where available. Prefer Crabbox for broad validation if ready; retain explicit evidence for any environment limitation.
- [x] Commit documentation and the completed plan checklist.
- [x] Request one fresh whole-change code review, address substantive findings with failing tests first, and commit verified fixes.
- [x] Close the tracked naming issue only after the change is verified; otherwise leave an accurate handoff. Report branch, commits, checks, and compatibility implications.

## Completion evidence

The CLI CRUD and strict workflow-reference tests were observed failing before
implementation and passing afterward. The complete shuffled Go suite passed on
an isolated runner with SQLite and PostgreSQL 17 using
`make test GOFLAGS_TEST='-shuffle=on -p=2'`. PostgreSQL 16 bootstrap,
30→31 migration, and legacy-schema rejection checks also passed locally.

`make api-check`, `make web-check`, `ZENSICAL_POLL_WATCHER=1 make docs-check`,
and `GOMAXPROCS=2 make lint LINT_NEW_FLAGS=--new-from-rev=deda3451` passed.
OpenAPI, Go clients, and TypeScript clients were regenerated by their repository
targets. Remaining retired names are intentional rejection tests, historical
fixtures, or compatibility explanations.

One independent review identified stale PostgreSQL fingerprints and federation
documentation; both were corrected and verified. The shared HTTP path parameter
remains `{cron_uid}` for jobs and workflows. No runtime semantics, live database,
released migration, or schema-version changes were introduced.
