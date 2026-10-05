# PostgreSQL migrations

Kata installs a new PostgreSQL database from the current canonical schema. The
first release with PostgreSQL support is the migration floor, so the initial
release contains no historical migration SQL. A forward migration is required
only after a released schema must be upgraded in place.

Database changes require explicit maintainer approval before implementation.
This applies to canonical DDL, schema-version changes, migration assets and
registries, extensions, functions, triggers, indexes, and other persisted
objects. Approval for a feature is not approval for its database design.

## Adding a migration

After the persisted-state change is approved:

1. Add one forward-only SQL asset under
   `internal/db/pgstore/migrations/`, named
   `NNNNNN_description.up.sql`. The six-digit prefix is the target schema
   version.
2. Register the embedded asset in `internal/db/pgstore/migrations.go` with its
   exact source and target versions. The registry must form one complete,
   unambiguous chain from every supported released version to the current
   version.
3. Update the canonical `internal/db/pgstore/schema.sql` so a fresh install and
   a migrated install have the same physical schema.
4. Add a real PostgreSQL upgrade test that starts at the released source
   schema, runs the production migration path, and verifies both the resulting
   schema version and affected behavior.
5. Update the operator documentation with compatibility, downtime, role, and
   rollback requirements.

Kata does not run down migrations. Rollback uses a pre-upgrade backup and the
matching older binary. Do not add no-op version markers, conditional repair
DDL for impossible schema/version combinations, or a second transaction that
can separate physical schema changes from the recorded version.

## Immutable history

Once a migration reaches `main`, never edit, rename, replace, or delete it.
Correct a released migration with a new numbered forward migration. Each
target version identifies exactly one migration name.

The `migration-history` pre-commit hook compares staged migration SQL with
`origin/main` and validates target versions across the complete Git index, not
only the current commit's additions. It therefore rejects duplicate versions
accumulated across several feature-branch commits as well as changes to SQL
already on main, down migrations, and filenames outside the
`NNNNNN_description.up.sql` convention. Fetch `origin/main` before committing;
the check fails closed when the comparison ref is unavailable.

Use `KATA_MIGRATION_BASE_REF` only when the canonical base has another local
ref name, such as in an isolated test repository. `KATA_MIGRATION_DIR` exists
for checker tests and should not relocate production assets.

## Runtime guarantees

`kata storage postgres migrate` runs as the schema-owner role. It holds a
transaction-scoped PostgreSQL advisory lock and commits the migration SQL and
schema-version stamp together. Runtime credentials use validation-only startup
and cannot migrate. Preserve those properties in every future migration.

See [PostgreSQL operations](../operations/postgres.md) for the production
upgrade ceremony and role grants.

## Schema 30: issue status fields

The 29→30 migration adds four nullable TEXT columns to `import_mappings`:
`observed_status`, `observed_status_at`, `pending_event_uid`, and
`remote_locator`. Existing mappings keep their identities and receive NULL in
all four, so upgrading creates no outbound status intent. The migration adds no
table, index, trigger, function, or extension.

Stop the daemon and use schema-owner credentials for `kata storage postgres
migrate`, then start the matching binary. Adding the columns takes a brief
exclusive lock on `import_mappings`; schedule the operation while imports are
stopped. Existing table grants cover the new columns. Validation-only runtime
credentials cannot perform this upgrade. Schema 29 binaries cannot reopen
schema 30; rollback requires the pre-upgrade backup and matching older binary.


## Schema 31: dormant shared definitions and run evidence

The 30→31 migration adds exactly `cron_jobs`, `cron_flows`, and
`cron_runs`, with three identity sequences and four named indexes.
Definitions and attributed run evidence have bounded versioned JSON. Distinct
run UIDs may share an occurrence key or issue. An upgraded database starts with
all three tables empty. No scheduler, executor grant, claim, issue holder,
checkpoint, receipt, frozen execution snapshot, or authority metadata is created.
Existing native issue due notifications remain unchanged.

Stop serving daemons and imports, back up the database, and run the migration
with schema-owner credentials. New tables and the version stamp are committed
atomically under the existing migration lock. Reapply the runtime role grants
in [PostgreSQL operations](../operations/postgres.md) for the new tables and
sequences. Validation-only credentials cannot migrate. Fresh installations and
30→31 upgrades must have the same physical schema; startup validates it.
Versions 25–30 use the existing immutable chain followed by migration 31.

Migration 31 is feature-only and unissued on main; this branch replaces its
rejected experimental five-table design without changing the allocated version.
Migrations 26–30 remain immutable. An existing experimental schema 31 is
rejected without repair or restamp, including occurrence-exclusive run shapes.
Retain its backup and matching old binary for inspection/export; rebuilding it
requires an explicitly authorized isolated plan. Legacy authority exports are
not automatically converted into the reduced contract.

Use the matching schema 31 binary. Older binaries cannot open schema 31.
Rollback requires a pre-upgrade backup and matching older binary; there is no
down migration. Ordinary JSONL restore preserves shared definitions and run
evidence without epochs or reconciliation. SQLite retains its normal JSONL
30→31 rebuild/cutover and backup/swap protections, and rejects incompatible
experimental schema 31 before changing the database.
