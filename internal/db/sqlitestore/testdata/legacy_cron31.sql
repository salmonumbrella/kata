CREATE TABLE cron_jobs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  uid TEXT NOT NULL UNIQUE CHECK (length(uid) = 26 AND substr(uid,1,1) BETWEEN '0' AND '7' AND uid NOT GLOB '*[^0-9A-HJKMNP-TV-Z]*'),
  project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
  name TEXT NOT NULL CHECK (length(trim(name)) BETWEEN 1 AND 256),
  definition_json TEXT NOT NULL CHECK (COALESCE((length(CAST(definition_json AS BLOB)) <= 262144 AND json_valid(definition_json) AND json_type(definition_json) = 'object' AND json_extract(definition_json, '$.version') = 1 AND json_type(definition_json, '$.version') = 'integer'), 0)),
  definition_event_uid TEXT NOT NULL CHECK (length(definition_event_uid) = 26 AND substr(definition_event_uid,1,1) BETWEEN '0' AND '7' AND definition_event_uid NOT GLOB '*[^0-9A-HJKMNP-TV-Z]*'),
  definition_hlc_json TEXT NOT NULL CHECK (COALESCE((length(CAST(definition_hlc_json AS BLOB)) <= 16384 AND json_valid(definition_hlc_json) AND json_type(definition_hlc_json) = 'object' AND json_extract(definition_hlc_json, '$.version') = 1 AND json_type(definition_hlc_json, '$.version') = 'integer' AND (json_type(definition_hlc_json, '$.physical_ms') = 'integer' AND json_extract(definition_hlc_json, '$.physical_ms') >= 0) AND (json_type(definition_hlc_json, '$.counter') = 'integer' AND json_extract(definition_hlc_json, '$.counter') >= 0)), 0)),
  schedule_state_json TEXT NOT NULL DEFAULT '{"version":1,"revision":0}' CHECK (COALESCE((length(CAST(schedule_state_json AS BLOB)) <= 16384 AND json_valid(schedule_state_json) AND json_type(schedule_state_json) = 'object' AND json_extract(schedule_state_json, '$.version') = 1 AND json_type(schedule_state_json, '$.version') = 'integer' AND (json_type(schedule_state_json, '$.revision') = 'integer' AND json_extract(schedule_state_json, '$.revision') >= 0) AND json_extract(schedule_state_json, '$.revision') >= 0), 0)),
  author TEXT NOT NULL CHECK (length(trim(author)) > 0),
  revision INTEGER NOT NULL DEFAULT 1 CHECK (revision >= 1),
  created_at DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  updated_at DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  deleted_at DATETIME
);
CREATE INDEX idx_cron_jobs_project_name ON cron_jobs(project_id,name) WHERE deleted_at IS NULL;

CREATE TABLE cron_flows (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  uid TEXT NOT NULL UNIQUE CHECK (length(uid) = 26 AND substr(uid,1,1) BETWEEN '0' AND '7' AND uid NOT GLOB '*[^0-9A-HJKMNP-TV-Z]*'),
  project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
  name TEXT NOT NULL CHECK (length(trim(name)) BETWEEN 1 AND 256),
  definition_json TEXT NOT NULL CHECK (COALESCE((length(CAST(definition_json AS BLOB)) <= 262144 AND json_valid(definition_json) AND json_type(definition_json) = 'object' AND json_extract(definition_json, '$.version') = 1 AND json_type(definition_json, '$.version') = 'integer'), 0)),
  definition_event_uid TEXT NOT NULL CHECK (length(definition_event_uid) = 26 AND substr(definition_event_uid,1,1) BETWEEN '0' AND '7' AND definition_event_uid NOT GLOB '*[^0-9A-HJKMNP-TV-Z]*'),
  definition_hlc_json TEXT NOT NULL CHECK (COALESCE((length(CAST(definition_hlc_json AS BLOB)) <= 16384 AND json_valid(definition_hlc_json) AND json_type(definition_hlc_json) = 'object' AND json_extract(definition_hlc_json, '$.version') = 1 AND json_type(definition_hlc_json, '$.version') = 'integer' AND (json_type(definition_hlc_json, '$.physical_ms') = 'integer' AND json_extract(definition_hlc_json, '$.physical_ms') >= 0) AND (json_type(definition_hlc_json, '$.counter') = 'integer' AND json_extract(definition_hlc_json, '$.counter') >= 0)), 0)),
  author TEXT NOT NULL CHECK (length(trim(author)) > 0),
  revision INTEGER NOT NULL DEFAULT 1 CHECK (revision >= 1),
  created_at DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  updated_at DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  deleted_at DATETIME
);
CREATE INDEX idx_cron_flows_project_name ON cron_flows(project_id,name) WHERE deleted_at IS NULL;

CREATE TABLE cron_runs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  uid TEXT NOT NULL UNIQUE CHECK (length(uid) = 26 AND substr(uid,1,1) BETWEEN '0' AND '7' AND uid NOT GLOB '*[^0-9A-HJKMNP-TV-Z]*'),
  project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
  job_uid TEXT NOT NULL CHECK (length(job_uid) = 26 AND substr(job_uid,1,1) BETWEEN '0' AND '7' AND job_uid NOT GLOB '*[^0-9A-HJKMNP-TV-Z]*'),
  occurrence_key TEXT NOT NULL CHECK (length(occurrence_key) BETWEEN 1 AND 1024),
  definition_event_uid TEXT NOT NULL CHECK (length(definition_event_uid) = 26 AND substr(definition_event_uid,1,1) BETWEEN '0' AND '7' AND definition_event_uid NOT GLOB '*[^0-9A-HJKMNP-TV-Z]*'),
  snapshot_json TEXT NOT NULL CHECK (COALESCE((length(CAST(snapshot_json AS BLOB)) <= 262144 AND json_valid(snapshot_json) AND json_type(snapshot_json) = 'object' AND json_extract(snapshot_json, '$.version') = 1 AND json_type(snapshot_json, '$.version') = 'integer'), 0)),
  issue_uid TEXT CHECK (issue_uid IS NULL OR (length(issue_uid) = 26 AND substr(issue_uid,1,1) BETWEEN '0' AND '7' AND issue_uid NOT GLOB '*[^0-9A-HJKMNP-TV-Z]*')),
  actor TEXT NOT NULL CHECK (length(trim(actor)) > 0),
  executor_uid TEXT NOT NULL CHECK (length(executor_uid) = 26 AND substr(executor_uid,1,1) BETWEEN '0' AND '7' AND executor_uid NOT GLOB '*[^0-9A-HJKMNP-TV-Z]*'),
  executor_instance_uid TEXT NOT NULL CHECK (length(executor_instance_uid) = 26 AND substr(executor_instance_uid,1,1) BETWEEN '0' AND '7' AND executor_instance_uid NOT GLOB '*[^0-9A-HJKMNP-TV-Z]*'),
  attempt INTEGER NOT NULL CHECK (attempt >= 0),
  state TEXT NOT NULL CHECK (state IN ('reserved','ready','starting','running','parked','uncertain','succeeded','failed','cancelled','retired','pending','delivered','native_reconciled','superseded')),
  summary_json TEXT NOT NULL DEFAULT '{"version":1}' CHECK (COALESCE((length(CAST(summary_json AS BLOB)) <= 65536 AND json_valid(summary_json) AND json_type(summary_json) = 'object' AND json_extract(summary_json, '$.version') = 1 AND json_type(summary_json, '$.version') = 'integer' AND (json_type(summary_json, '$.input_tokens') IS NULL OR (json_type(summary_json, '$.input_tokens') = 'integer' AND json_extract(summary_json, '$.input_tokens') >= 0)) AND (json_type(summary_json, '$.output_tokens') IS NULL OR (json_type(summary_json, '$.output_tokens') = 'integer' AND json_extract(summary_json, '$.output_tokens') >= 0))), 0)),
  notification_receipt_json TEXT NOT NULL DEFAULT '{"version":1,"state":"none"}' CHECK (COALESCE((length(CAST(notification_receipt_json AS BLOB)) <= 16384 AND json_valid(notification_receipt_json) AND json_type(notification_receipt_json) = 'object' AND json_extract(notification_receipt_json, '$.version') = 1 AND json_type(notification_receipt_json, '$.version') = 'integer' AND json_extract(notification_receipt_json, '$.state') IN ('none','waiting','delivered','native_reconciled','superseded','cancelled','conflicted')), 0)),
  revision INTEGER NOT NULL DEFAULT 1 CHECK (revision >= 1),
  created_at DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  started_at DATETIME,
  ended_at DATETIME,
  updated_at DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  UNIQUE(project_id,job_uid,occurrence_key),
  UNIQUE(project_id,uid)
);
CREATE INDEX idx_cron_runs_project_time ON cron_runs(project_id,created_at);
CREATE INDEX idx_cron_runs_job_time ON cron_runs(project_id,job_uid,created_at);

CREATE TABLE cron_run_claims (
  run_uid TEXT PRIMARY KEY NOT NULL,
  claim_uid TEXT NOT NULL UNIQUE CHECK (length(claim_uid) = 26 AND substr(claim_uid,1,1) BETWEEN '0' AND '7' AND claim_uid NOT GLOB '*[^0-9A-HJKMNP-TV-Z]*'),
  project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
  authority_instance_uid TEXT NOT NULL CHECK (length(authority_instance_uid) = 26 AND substr(authority_instance_uid,1,1) BETWEEN '0' AND '7' AND authority_instance_uid NOT GLOB '*[^0-9A-HJKMNP-TV-Z]*'),
  authority_epoch TEXT NOT NULL CHECK (length(authority_epoch) = 26 AND substr(authority_epoch,1,1) BETWEEN '0' AND '7' AND authority_epoch NOT GLOB '*[^0-9A-HJKMNP-TV-Z]*'),
  executor_uid TEXT NOT NULL CHECK (length(executor_uid) = 26 AND substr(executor_uid,1,1) BETWEEN '0' AND '7' AND executor_uid NOT GLOB '*[^0-9A-HJKMNP-TV-Z]*'),
  executor_instance_uid TEXT NOT NULL CHECK (length(executor_instance_uid) = 26 AND substr(executor_instance_uid,1,1) BETWEEN '0' AND '7' AND executor_instance_uid NOT GLOB '*[^0-9A-HJKMNP-TV-Z]*'),
  actor TEXT NOT NULL CHECK (length(trim(actor)) > 0),
  attempt INTEGER NOT NULL CHECK (attempt >= 1),
  state TEXT NOT NULL CHECK (state IN ('reserved','ready','starting','running','parked','uncertain','succeeded','failed','cancelled','retired')),
  revision INTEGER NOT NULL DEFAULT 1 CHECK (revision >= 1),
  launch_sequence INTEGER NOT NULL DEFAULT 0 CHECK (launch_sequence >= 0),
  launch_request_uid TEXT CHECK (launch_request_uid IS NULL OR (length(launch_request_uid) = 26 AND substr(launch_request_uid,1,1) BETWEEN '0' AND '7' AND launch_request_uid NOT GLOB '*[^0-9A-HJKMNP-TV-Z]*')),
  launch_step_key TEXT,
  launch_state TEXT NOT NULL DEFAULT 'none' CHECK (launch_state IN ('none','granted','running','finished','uncertain')),
  operation_receipt_json TEXT NOT NULL DEFAULT '{"version":1}' CHECK (COALESCE((length(CAST(operation_receipt_json AS BLOB)) <= 16384 AND json_valid(operation_receipt_json) AND json_type(operation_receipt_json) = 'object' AND json_extract(operation_receipt_json, '$.version') = 1 AND json_type(operation_receipt_json, '$.version') = 'integer' AND (json_type(operation_receipt_json, '$.accepted_revision') IS NULL OR (json_type(operation_receipt_json, '$.accepted_revision') = 'integer' AND json_extract(operation_receipt_json, '$.accepted_revision') >= 0)) AND (json_type(operation_receipt_json, '$.attempt') IS NULL OR (json_type(operation_receipt_json, '$.attempt') = 'integer' AND json_extract(operation_receipt_json, '$.attempt') >= 0)) AND (json_type(operation_receipt_json, '$.launch_sequence') IS NULL OR (json_type(operation_receipt_json, '$.launch_sequence') = 'integer' AND json_extract(operation_receipt_json, '$.launch_sequence') >= 0))), 0)),
  claimed_at DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  last_seen_at DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  released_at DATETIME,
  release_reason TEXT,
  FOREIGN KEY(project_id,run_uid) REFERENCES cron_runs(project_id,uid) ON DELETE RESTRICT
);

CREATE TABLE cron_issue_holders (
  project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
  issue_uid TEXT NOT NULL CHECK (length(issue_uid) = 26 AND substr(issue_uid,1,1) BETWEEN '0' AND '7' AND issue_uid NOT GLOB '*[^0-9A-HJKMNP-TV-Z]*'),
  run_uid TEXT NOT NULL UNIQUE,
  created_at DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  PRIMARY KEY(project_id,issue_uid),
  FOREIGN KEY(project_id,run_uid) REFERENCES cron_runs(project_id,uid) ON DELETE RESTRICT
);
