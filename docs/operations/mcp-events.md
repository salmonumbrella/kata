---
title: MCP Events webhook delivery
description: Configure durable inbox change subscriptions, signed receivers, replay, retries, and recovery.
last_edited: 2026-10-18
---

# MCP Events webhook delivery

Kata can deliver signed `kata.inbox.changed` occurrences to a webhook receiver.
The receiver maps an exact recipient to its own worker, reads the current inbox,
and clears a request after handling it. A notification never grants authority to
run an agent or execute its text. Requests remain replaceable attention signals;
Events does not turn them into a lossless message queue.

## Start a durable service

Enable Events on the existing MCP server with an absolute private directory:

```sh
kata mcp serve --http 127.0.0.1:9888 \
  --http-token-file /path/to/private/inbound-mcp-token \
  --events-state-dir /path/to/private/mcp-events
```

Use the normal `--project`, `--projects`, or `--all` scope options. Without
`--events-state-dir`, the server keeps its ordinary tools and does not advertise
Events. The directory is exclusively locked by one process. A competing process,
insecure permissions, unsupported records, or corruption causes startup to fail;
Kata never silently switches to memory or resets accepted subscriptions.

The long-running HTTP server is the unattended deployment. Stdio hosts the same
webhook service while its process remains alive. HTTP request or session closure
does not unsubscribe. Process shutdown pauses delivery; reopening the same bound
state directory resumes unexpired records after live authorization checks.

State binds the canonical backend origin, daemon instance, actual backend
credential and effective principal, inbound HTTP credential or local process
identity, configured actor, and configured project scope. A restricted remote
backend token stays restricted when the MCP transport is stdio. Changing a bound
credential, daemon, actor, or scope requires explicit registration in a new private
directory. Retain the old directory as protected recovery material; its cursors
cannot be transferred to another service identity.

Directories are owner-private (0700 on Unix), and records are owner-private files
(0600 on Unix), with corresponding current-user protection on Windows. Identity
and subscription JSON files contain cursor authentication keys and webhook signing
keys. They are credential-bearing files, without encryption at rest. Keep them
out of source control, shared logs, federation exports, and public backups.

## Discover and subscribe

This implements the OpenAI MCP Events webhook profile inspected on 2026-10-18:
`capabilities.events`, `events/list`, `events/subscribe`, and
`events/unsubscribe`. Callback verification failures use JSON-RPC `-32015`.
Core MCP 2026-07-28 discovery and compatible legacy initialization advertise the
same enabled capability. Experimental proposal revisions with different error
numbers, extra delivery modes, or gap/terminated control messages are not this
profile.

An MCP client supplies the ordinary core protocol metadata and HTTP headers.
The following is the `events/subscribe` parameter object:

```json
{
  "name": "kata.inbox.changed",
  "arguments": {
    "for": "example-agent/worker",
    "project_uid": "01ARZ3NDEKTSV4RRFFQ69G5FAV"
  },
  "delivery": {
    "mode": "webhook",
    "url": "https://receiver.example/inbox",
    "secret": "whsec_<base64-of-a-private-24-to-64-byte-key>"
  },
  "cursor": null,
  "ttlMs": 86400000
}
```

Replace the callback and signing-key placeholders with the receiver's actual
configuration. The callback must use HTTPS and a publicly routable destination.
Every connection validates all resolved addresses and dials a validated address
with the original TLS hostname. Private, loopback, reserved, mixed-address, and
redirect destinations are rejected. Callback requests have no daemon bearer,
browser credentials, or environment proxy. There is no production localhost
exception; isolated tests inject a resolver and dialer while retaining validation.

`for` is one exact recipient, including its teammate suffix. Optional
`project_uid` and `issue_uid` pin immutable identities; an issue filter requires a
project UID. Omitting the project selects the existing configured MCP scope.
Filters cannot widen daemon authority. Unknown arguments fail before any callback.

A null cursor captures authorized current heads and durably queues an initial
resync. Read the current inbox on that resync. The response contains `id`, an
ISO8601 `refreshBefore`, a safe authenticated `cursor`, and `truncated`.
Refresh with the same event, callback, and canonical arguments. Default and maximum
lifetime are 24 hours; positive TTLs clamp to 60 seconds through 24 hours. Null TTL
also receives a finite lifetime. Refresh before the returned boundary; expired
records send nothing.

Live refresh with an old cursor is idempotent: it neither rewinds acknowledged
work nor replaces pending delivery. A cursor ahead of the safe watermark is
rejected. Recreating an expired or unsubscribed registration with an older valid
cursor replays retained source events. Purged history causes an explicit resync
and `truncated:true`. After a gap, send back the newly returned cursor to acknowledge
its continuity generation; an older cursor at the same numeric position cannot
acknowledge the gap.

Changing the signing key verifies the new key before accepting the refresh. Both
keys sign during a 120-second overlap; the previous key then expires durably.
Verification may be cached for at most five minutes for the same principal,
callback, and key fingerprint. A failed refresh preserves the accepted record.

## Receive safely

Verification is a signed JSON object containing `type:"verification"` and a fresh
`challenge`. Return a 2xx JSON response containing that exact challenge. No
application occurrence is delivered before verification and durable activation.

Application envelopes contain `eventId`, `name`, the source `timestamp`, `data`,
and a conservative `cursor`. Data contains only the exact recipient, a reason
(`notification`, `visibility`, or `resync`), optional stable project/issue/source
event UIDs, and `resync_required`. It contains no titles, bodies, messages,
comments, credentials, or raw metadata.

Verify the Standard Webhooks headers before accepting a receipt:

- `webhook-id` equals `eventId`.
- `webhook-timestamp` is a fresh Unix-seconds signing time.
- Each `v1,<base64-signature>` in `webhook-signature` is HMAC-SHA256 over the exact
  bytes `webhook-id + "." + webhook-timestamp + "." + body`, using a decoded key.
- `X-MCP-Subscription-Id` identifies the registration.

Use constant-time signature comparison and a bounded timestamp tolerance. Persist
and deduplicate the receipt by event ID before returning 2xx. A 2xx means receipt,
not completed work. Identical source occurrences keep their ID across retries,
restarts, and separate subscriptions. Retries preserve the exact envelope bytes
while obtaining a fresh signing timestamp and signatures. The same occurrence
may have different cursors across registrations: acknowledge a valid duplicate
without dispatching again, and track refresh cursors separately by subscription ID.

Map recipients explicitly. An idle worker can wake; a busy worker can coalesce a
new inbox read; an unavailable worker's request remains pending for its accountable
actor. Reading or scheduling does not clear the request. After handling it, use
the normal attributed tools to record results and clear only that exact request.
Do not automatically comment or re-notify merely because an event arrived.

Load `kata.load_coordination` and follow `kata.inbox.next_cursor` until absent,
including empty filtered pages. Deduplicate requests by project and issue UID.
Use `kata.show` for details. Re-read after `resync_required` or `truncated`, and
keep the returned cursor for subsequent refresh/recovery. See the
[native cron MCP example](../reference/cron.md) for recurring inbox delivery.

## Delivery health and recovery

`kata.event_subscriptions`, loaded through coordination, reports subscription and
event IDs, safe cursors, attempts, timestamps, categorized outcomes, and continuity
state. It never returns callback URLs, signing keys, or response bodies.
`kata.event_delivery_retry` takes an `id` and explicitly re-arms a transiently
exhausted pending occurrence; it mutates the private runtime state.

Timeouts, 429, and transient failures use bounded exponential backoff, at most
eight attempts over 15 minutes. Retry-After is capped, and each callback is bounded
by ten seconds and the remaining retry window. Transient exhaustion retains the
exact failed pending envelope and freezes its watermark. Refresh or explicit
retry can re-arm it. Other subscriptions continue.

HTTP 410 and 413 terminally abandon that occurrence once, advance past it, retain
failure diagnostics, and mark continuity lost. They are never retried by refresh
or the retry tool. A resync rejected the same way is abandoned without generating
an endless resync loop. Subsequent ordinary occurrences carry `resync_required`
until the gap is acknowledged with the current cursor. Delivery is therefore
retryable with explicit terminal abandonment, rather than unconditional receipt.

Before refresh and delivery, Kata rechecks live backend authority. Temporary
backend failures pause and back off without consuming callback attempts. Confirmed
revocation disables the record. Aggregate scopes can lose one project while
keeping authorized projects; a scoped resync removes stale identifiers. Losing an
explicitly selected project or issue disables that subscription.

To unsubscribe, call `events/unsubscribe` with the original `name`, `arguments`,
and `delivery` mode/URL; a signing secret is unnecessary. The operation is
idempotent, durably fences the registration, and cancels/waits for an in-flight
send before returning. Bytes already accepted by the receiver cannot be recalled.

For a consistent backup, stop the process normally, copy the entire private state
directory including `identity.json`, and preserve ownership/permissions. Restore
that complete directory to the same bound backend and credentials. Do not combine
records from different services or restore subscription files without their
identity key. Unknown formats or partial/corrupt backups require operator recovery
or explicit registration in a new private directory; there is no automatic
schema migration or destructive reset.

The service uses the existing durable daemon event log and private atomic JSON
records. It adds no database tables, migrations, secondary database, cron scheduler,
or execution engine. Current limits are 128 subscriptions, 128 projects per
subscription, 16 KiB cursors, 1 MiB state files, 256 KiB envelopes, 64 KiB
verification responses, and eight concurrent callbacks. Dynamic scope growth past
the project limit suspends visibly instead of truncating the authorized catalog.
Source and inbox pages are capped at 8 MiB before decoding. Oversized source pages
shrink; an oversized single source event produces an explicit continuity gap and
a fresh authorized head. An oversized single inbox candidate returns an error.

The daemon switcher does not proxy a delegated Events identity. Point an MCP
service directly at each intended backend using its normal credential source,
with a separate private state directory for each binding. Callback validation
and signing remain independent of the daemon transport.
