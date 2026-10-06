---
title: Native cron
description: Shared job and workflow definitions and attributed run evidence.
last_edited: 2026-10-06
---

Kata stores portable job and workflow definitions and independent run observations.
External adapters decide when to run, resolve checkout and provider configuration,
create or update ordinary issues, deliver notifications, and launch processes.
An observation records evidence; its status never grants permission to run.

## Vocabulary

| Name | Meaning |
| --- | --- |
| Job | Configured work: its trigger, action, and execution settings. |
| Workflow | Reusable command or prompt steps and their dependencies. |
| Run | One execution and its reported status and results. |

A job can invoke a workflow or supply a single prompt. Each execution has its
own run UID. A workflow can also be referenced directly by a run.

Issue responsibility uses Kata's existing `claim`, `assign`, `unassign`, and
owner vocabulary. A federation lease provides exclusive coordination on an
issue. Recording a run changes neither ownership nor leases. See
[federation leases](../operations/federation.md#leases-and-write-gates) for that mechanism.

## Definitions

`kata cron job` and `kata cron workflow` support `create`, `list`, `show`,
`update`, `delete`, and `restore`. Commands return JSON. Creation retains a ULID:

```sh
kata cron job create --uid <job-uid> --file job.json --json
kata cron job list --json
kata cron workflow create --uid <workflow-uid> --file workflow.json --json
kata cron workflow list --json
kata cron job update <job-uid> --file replacement.json --json
kata cron job delete <job-uid> --expected-event-uid <event-uid> --json
```

Creation requires no expected event UID. Updates, deletion, and restoration
require the current `expected_event_uid`; conflicts require inspecting the
current definition. A lost create response can be inspected using the retained
UID. A repeated create does not silently become an update. Tombstones remain
readable, and `list --include-deleted` includes them.

Definitions have version 1, with a 256 KiB bound. Jobs use `kind: "job"`,
`enabled`, a trigger, an action, issue policy, overlap and catchup policy, and
optional portable configuration such as workflow UID, checkout key, secret references,
timeouts and provider options. Workflow steps carry portable adapter configuration.
Enabling a job changes shared configuration; it creates no timer or process.
Secret references are names, and checkout keys are portable names. Credentials,
host paths, runtime handles, executor grants and execution snapshots are not
part of this shared contract. The former `kind: "direct"` and `executor` fields
are rejected. Opaque option objects retain exact JSON numbers.

## Capabilities

`kata cron capabilities --json` reads the project's UID and advertised
`event_features` without changing state. The corresponding GET is
`/api/v1/projects/{project_id}/cron/capabilities`; the
`X-Kata-Event-Features` header advertises `cron_v1`. Missing, unknown, or
failed advertisements remain distinct outcomes. This is an ordinary project
read and creates no permissions. Ordinary authentication, project isolation,
and issue-scoped route restrictions apply.

## Independent run evidence

An adapter creates one independent run UID and retains it across retries:

```sh
kata cron run observe <run-uid> --json-input observation.json --json
kata cron run list --limit 100 --json
kata cron run show <run-uid> --json
```

The HTTP operation is `PUT /api/v1/projects/{project_id}/cron/runs/{run_uid}`
(`observeCronRun`). An omitted JSON `teammate` inherits `--teammate` or
`KATA_TEAMMATE`; an explicit empty flag suppresses that inheritance. An explicit
JSON string or `null` freezes attribution across retries and ignores the
environment. An explicit flag must match the frozen value (`null` matches an
empty flag). An empty JSON string remains invalid; it is never replaced by the
environment. For example:

```json
{
  "job_uid": "01ARZ3NDEKTSV4RRFFQ69G5FAA",
  "definition_event_uid": "01ARZ3NDEKTSV4RRFFQ69G5FAB",
  "occurrence_key": "daily:2026-10-06",
  "actor": "worker",
  "teammate": "adapter",
  "executor_label": "local-worker",
  "status": "running",
  "summary": {"version": 1, "message": "Review started"},
  "expected_revision": 0
}
```

At least one paired job/event or workflow/event reference is required; both pairs
may be supplied. References identify historical definitions, including earlier
versions and tombstones, within the project. An optional issue UID is an
ordinary existing issue in that project. Distinct run UIDs may share the same
occurrence key and issue. No occurrence is reserved by logging a run.

UID, project, definition references, occurrence key, issue UID, attributed actor,
teammate, executor label, and server-created time are immutable. Status, summary,
start time, and end time are mutable evidence. Supported statuses are `running`,
`succeeded`, `failed`, `cancelled`, and `unknown`; they impose no transition or
launch protocol. Summary version 1 permits a message and nonnegative integer
input/output token counts and is bounded to 64 KiB. The whole observation and
its event are bounded to 96 KiB. Unknown fields are rejected. Times are RFC3339
instants; an end cannot precede a start. An occurrence key is at most 1024
characters; actor and optional executor label are at most 256 bytes. Teammates
use Kata's existing 1–64 character ASCII names.

A new observation uses expected revision 0. A changed observation requires its
current revision. An identical retry of the same UID and canonical evidence
returns `replayed: true`, emits no event, and changes no revision even when its
expected revision is stale. A different immutable identity always conflicts.
Authenticated actor attribution overrides an untrusted request actor, and
revocation is rechecked in the write transaction. Logging does not change an
issue's owner, dates, lease, status, or readiness.

Responses contain `run`, the exact committed `events`, and `replayed`. Run
listing is project-scoped, supports an optional job UID, and orders by creation
time and UID descending. Use `next_before_uid` with `--before-uid` for another
page. Ordinary federation publishes observations from spokes, folds evidence
by original event provenance per run UID, and preserves independent runs in
baselines and backup. Run identity conflicts fail rather than replacing a run.

## Native issue planning dates

`kata show <issue-ref> --planning-dates --json` and GET
`/api/v1/projects/{project_id}/issues/{ref}/planning-dates` read one consistent
issue revision with required nullable `scheduled_on` and `deadline_on` objects.
Each present object contains `field`, raw `value`, resolved `timezone`, and a
UTC `instant`. The flat response also contains `project_id`, `issue_uid`, and
ordinary `revision`.

Native date parsing applies the issue timezone first, then the recurrence
fallback for scheduled dates, then daemon default timezone and UTC. Deadline
dates have no recurrence fallback. Explicit UTC values resolve to UTC. The read
uses native civil-time and DST behavior; it evaluates no clock and changes no
readiness or persisted state. Normal project and issue-subtree read policy applies.

## Adapter operation and local activation

Shared `enabled` configuration does not activate an adapter installation. Each
adapter owns its local activation, scheduler, overlap/catchup policy, checkout
and secret mappings, process lifecycle, and persistent conversation recovery.
A second adapter may independently execute the same occurrence. Each execution
retains a separate run UID; no shared observation reserves the issue or occurrence.
Local raw snapshots, transcripts, PIDs, session handles, credentials and delivery
buffers are not native run evidence. Logging is ordinary bookkeeping: a failed
write may be buffered without blocking process launch or reuse of a conversation.

[Herdr Kata's native setup](https://github.com/salmonumbrella/herdr-kata/blob/main/docs/native.md)
describes its explicit read-only upstream importer, local activation, live
persistent recovery, buffered results and exact-recipient inbox bridge. Importing
legacy definitions creates dormant ordinary native definitions. It does not
import obsolete collaboration stores or local runs, leases, PIDs or sessions.
Keep a stable source namespace across retries; creating definitions neither enrolls
an executor nor adopts runtime authority. Install Kata separately: Herdr Kata
resolves the installed CLI/TUI on each launch rather than bundling another issue UI.

The adapter must refresh current definitions before starting new scheduled work;
offline cached data is for labeled reads. Saved local run snapshots support
recovery under that adapter's local rules. Portable cron uses the configured trigger
timezone. Native issue-scheduled triggers use the planning-date instant above:
issue timezone, recurrence fallback, daemon default and UTC remain authoritative.
Future `scheduled_on` and `someday` defer ordinary execute readiness; `deadline_on`
never does. Deadline actions are notification-only.

Kata's default-date sweeper continues to request the current owner's attention,
or the author's when unowned, for reached native schedules and deadlines. The
same-source, zero-lead `current-owner-or-author` alias must not produce a second
adapter notification. Custom offsets and recipients use ordinary notifications.
Inbox addresses are exact: `worker` does not aggregate `worker/adapter`. Reads do
not clear requests; the recipient handler clears only after handling and reads
back. Attention is a replaceable per-issue/recipient slot, so replacement and clear
can race. Herdr Kata retains occupied or unavailable attention locally; generic
Herdr delivery currently requires manual wake and never automatically types into
an agent. See the adapter's operator docs for guarded runtime delivery support.

Shared definitions and run evidence inherit ordinary authentication, project/
issue-scoped roles, actor attribution, token revocation and credential-origin
checks. Capability discovery advertises compatibility only. Shared federation and
backup do not transfer local activation, checkout credentials, execution buffers
or conversation handles; preserve those separately under the adapter's backup
workflow. PostgreSQL runtime roles still cannot perform owner-only migrations.

## Storage and upgrades

SQLite and PostgreSQL schema 31 contain exactly `cron_jobs`,
`cron_workflows`, and `cron_runs`. The unissued five-table experimental
schema 31 is incompatible and is rejected without automatic repair or restamp.
Use its matching binary to inspect/export it, retain a compatible backup, and
plan an explicitly authorized isolated rebuild. Legacy authority exports are
rejected before an import clears its target. Normal JSONL export/import retains
shared definitions and bounded run evidence. Running evidence does not prevent
an explicitly requested restore, federation reset, adoption, or project merge;
those operations retain their ordinary identity, revision and credential guards.
See [PostgreSQL migrations](../development/postgres-migrations.md) for owner-only
migration, backup, runtime grants, and rollback requirements.

## Naming compatibility

Native cron uses `workflow` throughout the CLI, API, and stored records.
Workflow routes are `/api/v1/projects/{project_id}/cron/workflows` and
`/api/v1/projects/{project_id}/cron/workflows/{cron_uid}`. Responses use
`workflow` or `workflows`; job actions and run observations use `workflow_uid`,
and run observations pair it with `workflow_definition_event_uid`. Events use
`cron.workflow.*`, and JSONL workflow records use `cron_workflow`.

This replaces the experimental `flow` spelling without aliases. Update adapter
commands, request fields, and generated-client calls together. Older experimental
schema-31 databases and exports require their matching old binary for inspection
or export; this rename does not automatically convert them. See the
[database compatibility notes](../development/postgres-migrations.md#schema-31-dormant-shared-definitions-and-run-evidence).
