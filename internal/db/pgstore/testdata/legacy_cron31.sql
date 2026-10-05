-- Schema 30 to 31: dormant native cron storage.
-- Dormant native cron definitions and authority history.
CREATE TABLE cron_jobs (
  id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  uid TEXT NOT NULL UNIQUE CHECK (uid ~ '^[0-7][0-9A-HJKMNP-TV-Z]{25}$'),
  project_id BIGINT NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
  name TEXT NOT NULL CHECK (length(trim(name)) BETWEEN 1 AND 256),
  definition_json TEXT NOT NULL CHECK (COALESCE((octet_length(definition_json) <= 262144 AND jsonb_typeof(definition_json::jsonb) = 'object' AND (definition_json::jsonb -> 'version')::text = '1'), FALSE)),
  definition_event_uid TEXT NOT NULL CHECK (definition_event_uid ~ '^[0-7][0-9A-HJKMNP-TV-Z]{25}$'),
  definition_hlc_json TEXT NOT NULL CHECK (COALESCE((octet_length(definition_hlc_json) <= 16384 AND jsonb_typeof(definition_hlc_json::jsonb) = 'object' AND (definition_hlc_json::jsonb -> 'version')::text = '1' AND ((definition_hlc_json::jsonb->'physical_ms')::text ~ '^[0-9]+$') AND ((definition_hlc_json::jsonb->'counter')::text ~ '^[0-9]+$') AND (definition_hlc_json::jsonb->>'physical_ms')::bigint >= 0 AND (definition_hlc_json::jsonb->>'counter')::bigint >= 0), FALSE)),
  schedule_state_json TEXT NOT NULL DEFAULT '{"version":1,"revision":0}' CHECK (COALESCE((octet_length(schedule_state_json) <= 16384 AND jsonb_typeof(schedule_state_json::jsonb) = 'object' AND (schedule_state_json::jsonb -> 'version')::text = '1' AND ((schedule_state_json::jsonb->'revision')::text ~ '^[0-9]+$') AND (schedule_state_json::jsonb ->> 'revision')::bigint >= 0), FALSE)),
  author TEXT NOT NULL CHECK (length(trim(author)) > 0),
  revision BIGINT NOT NULL DEFAULT 1 CHECK (revision >= 1),
  created_at TEXT NOT NULL DEFAULT to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'),
  updated_at TEXT NOT NULL DEFAULT to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'),
  deleted_at TEXT
);
CREATE INDEX idx_cron_jobs_project_name ON cron_jobs(project_id,name) WHERE deleted_at IS NULL;

CREATE TABLE cron_flows (
  id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  uid TEXT NOT NULL UNIQUE CHECK (uid ~ '^[0-7][0-9A-HJKMNP-TV-Z]{25}$'),
  project_id BIGINT NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
  name TEXT NOT NULL CHECK (length(trim(name)) BETWEEN 1 AND 256),
  definition_json TEXT NOT NULL CHECK (COALESCE((octet_length(definition_json) <= 262144 AND jsonb_typeof(definition_json::jsonb) = 'object' AND (definition_json::jsonb -> 'version')::text = '1'), FALSE)),
  definition_event_uid TEXT NOT NULL CHECK (definition_event_uid ~ '^[0-7][0-9A-HJKMNP-TV-Z]{25}$'),
  definition_hlc_json TEXT NOT NULL CHECK (COALESCE((octet_length(definition_hlc_json) <= 16384 AND jsonb_typeof(definition_hlc_json::jsonb) = 'object' AND (definition_hlc_json::jsonb -> 'version')::text = '1' AND ((definition_hlc_json::jsonb->'physical_ms')::text ~ '^[0-9]+$') AND ((definition_hlc_json::jsonb->'counter')::text ~ '^[0-9]+$') AND (definition_hlc_json::jsonb->>'physical_ms')::bigint >= 0 AND (definition_hlc_json::jsonb->>'counter')::bigint >= 0), FALSE)),
  author TEXT NOT NULL CHECK (length(trim(author)) > 0),
  revision BIGINT NOT NULL DEFAULT 1 CHECK (revision >= 1),
  created_at TEXT NOT NULL DEFAULT to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'),
  updated_at TEXT NOT NULL DEFAULT to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'),
  deleted_at TEXT
);
CREATE INDEX idx_cron_flows_project_name ON cron_flows(project_id,name) WHERE deleted_at IS NULL;

CREATE TABLE cron_runs (
  id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  uid TEXT NOT NULL UNIQUE CHECK (uid ~ '^[0-7][0-9A-HJKMNP-TV-Z]{25}$'),
  project_id BIGINT NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
  job_uid TEXT NOT NULL CHECK (job_uid ~ '^[0-7][0-9A-HJKMNP-TV-Z]{25}$'),
  occurrence_key TEXT NOT NULL CHECK (length(occurrence_key) BETWEEN 1 AND 1024),
  definition_event_uid TEXT NOT NULL CHECK (definition_event_uid ~ '^[0-7][0-9A-HJKMNP-TV-Z]{25}$'),
  snapshot_json TEXT NOT NULL CHECK (COALESCE((octet_length(snapshot_json) <= 262144 AND jsonb_typeof(snapshot_json::jsonb) = 'object' AND (snapshot_json::jsonb -> 'version')::text = '1'), FALSE)),
  issue_uid TEXT CHECK (issue_uid IS NULL OR (issue_uid ~ '^[0-7][0-9A-HJKMNP-TV-Z]{25}$')),
  actor TEXT NOT NULL CHECK (length(trim(actor)) > 0),
  executor_uid TEXT NOT NULL CHECK (executor_uid ~ '^[0-7][0-9A-HJKMNP-TV-Z]{25}$'),
  executor_instance_uid TEXT NOT NULL CHECK (executor_instance_uid ~ '^[0-7][0-9A-HJKMNP-TV-Z]{25}$'),
  attempt BIGINT NOT NULL CHECK (attempt >= 0),
  state TEXT NOT NULL CHECK (state IN ('reserved','ready','starting','running','parked','uncertain','succeeded','failed','cancelled','retired','pending','delivered','native_reconciled','superseded')),
  summary_json TEXT NOT NULL DEFAULT '{"version":1}' CHECK (COALESCE((octet_length(summary_json) <= 65536 AND jsonb_typeof(summary_json::jsonb) = 'object' AND (summary_json::jsonb -> 'version')::text = '1' AND (summary_json::jsonb->'input_tokens' IS NULL OR ((summary_json::jsonb->'input_tokens')::text ~ '^[0-9]+$')) AND (summary_json::jsonb->'output_tokens' IS NULL OR ((summary_json::jsonb->'output_tokens')::text ~ '^[0-9]+$'))), FALSE)),
  notification_receipt_json TEXT NOT NULL DEFAULT '{"version":1,"state":"none"}' CHECK (COALESCE((octet_length(notification_receipt_json) <= 16384 AND jsonb_typeof(notification_receipt_json::jsonb) = 'object' AND (notification_receipt_json::jsonb -> 'version')::text = '1' AND (notification_receipt_json::jsonb->>'state') IN ('none','waiting','delivered','native_reconciled','superseded','cancelled','conflicted')), FALSE)),
  revision BIGINT NOT NULL DEFAULT 1 CHECK (revision >= 1),
  created_at TEXT NOT NULL DEFAULT to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'),
  started_at TEXT,
  ended_at TEXT,
  updated_at TEXT NOT NULL DEFAULT to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'),
  UNIQUE(project_id,job_uid,occurrence_key),
  UNIQUE(project_id,uid)
);
CREATE INDEX idx_cron_runs_project_time ON cron_runs(project_id,created_at);
CREATE INDEX idx_cron_runs_job_time ON cron_runs(project_id,job_uid,created_at);

CREATE TABLE cron_run_claims (
  run_uid TEXT PRIMARY KEY NOT NULL,
  claim_uid TEXT NOT NULL UNIQUE CHECK (claim_uid ~ '^[0-7][0-9A-HJKMNP-TV-Z]{25}$'),
  project_id BIGINT NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
  authority_instance_uid TEXT NOT NULL CHECK (authority_instance_uid ~ '^[0-7][0-9A-HJKMNP-TV-Z]{25}$'),
  authority_epoch TEXT NOT NULL CHECK (authority_epoch ~ '^[0-7][0-9A-HJKMNP-TV-Z]{25}$'),
  executor_uid TEXT NOT NULL CHECK (executor_uid ~ '^[0-7][0-9A-HJKMNP-TV-Z]{25}$'),
  executor_instance_uid TEXT NOT NULL CHECK (executor_instance_uid ~ '^[0-7][0-9A-HJKMNP-TV-Z]{25}$'),
  actor TEXT NOT NULL CHECK (length(trim(actor)) > 0),
  attempt BIGINT NOT NULL CHECK (attempt >= 1),
  state TEXT NOT NULL CHECK (state IN ('reserved','ready','starting','running','parked','uncertain','succeeded','failed','cancelled','retired')),
  revision BIGINT NOT NULL DEFAULT 1 CHECK (revision >= 1),
  launch_sequence BIGINT NOT NULL DEFAULT 0 CHECK (launch_sequence >= 0),
  launch_request_uid TEXT CHECK (launch_request_uid IS NULL OR (launch_request_uid ~ '^[0-7][0-9A-HJKMNP-TV-Z]{25}$')),
  launch_step_key TEXT,
  launch_state TEXT NOT NULL DEFAULT 'none' CHECK (launch_state IN ('none','granted','running','finished','uncertain')),
  operation_receipt_json TEXT NOT NULL DEFAULT '{"version":1}' CHECK (COALESCE((octet_length(operation_receipt_json) <= 16384 AND jsonb_typeof(operation_receipt_json::jsonb) = 'object' AND (operation_receipt_json::jsonb -> 'version')::text = '1' AND (operation_receipt_json::jsonb->'accepted_revision' IS NULL OR ((operation_receipt_json::jsonb->'accepted_revision')::text ~ '^[0-9]+$')) AND (operation_receipt_json::jsonb->'attempt' IS NULL OR ((operation_receipt_json::jsonb->'attempt')::text ~ '^[0-9]+$')) AND (operation_receipt_json::jsonb->'launch_sequence' IS NULL OR ((operation_receipt_json::jsonb->'launch_sequence')::text ~ '^[0-9]+$'))), FALSE)),
  claimed_at TEXT NOT NULL DEFAULT to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'),
  last_seen_at TEXT NOT NULL DEFAULT to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'),
  released_at TEXT,
  release_reason TEXT,
  FOREIGN KEY(project_id,run_uid) REFERENCES cron_runs(project_id,uid) ON DELETE RESTRICT
);

CREATE TABLE cron_issue_holders (
  project_id BIGINT NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
  issue_uid TEXT NOT NULL CHECK (issue_uid ~ '^[0-7][0-9A-HJKMNP-TV-Z]{25}$'),
  run_uid TEXT NOT NULL UNIQUE,
  created_at TEXT NOT NULL DEFAULT to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'),
  PRIMARY KEY(project_id,issue_uid),
  FOREIGN KEY(project_id,run_uid) REFERENCES cron_runs(project_id,uid) ON DELETE RESTRICT
);
