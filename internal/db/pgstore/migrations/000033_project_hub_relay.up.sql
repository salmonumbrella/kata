-- Project hub relay: team access policies, signed root attribution, project
-- relay transport state, and portable embedding artifacts.

-- Teams are daemon-local actor groups; restricted empty policies stay restricted.
CREATE TABLE teams (
 uid TEXT PRIMARY KEY CHECK (length(uid)=26),
 name TEXT NOT NULL UNIQUE CHECK (length(trim(name))>0),
 revision BIGINT NOT NULL DEFAULT 1 CHECK (revision>0)
);
CREATE TABLE team_memberships (
 team_uid TEXT NOT NULL REFERENCES teams(uid) ON DELETE CASCADE,
 actor TEXT NOT NULL CHECK (length(trim(actor))>0),
 PRIMARY KEY(team_uid,actor)
);
CREATE INDEX idx_team_memberships_actor ON team_memberships(actor,team_uid);
CREATE TABLE project_access_policies (
 project_uid TEXT PRIMARY KEY REFERENCES projects(uid) ON DELETE CASCADE,
 visibility TEXT NOT NULL CHECK (visibility IN ('all','teams')),
 revision BIGINT NOT NULL DEFAULT 1 CHECK (revision>0)
);
CREATE TABLE project_access_teams (
 project_uid TEXT NOT NULL REFERENCES project_access_policies(project_uid) ON DELETE CASCADE,
 team_uid TEXT NOT NULL REFERENCES teams(uid) ON DELETE CASCADE,
 PRIMARY KEY(project_uid,team_uid)
);
CREATE INDEX idx_project_access_teams_team ON project_access_teams(team_uid,project_uid);
INSERT INTO meta(key,value) VALUES('project_access_revision','1');

-- Root public-key pins and proofs are portable. Private signing material is not.
CREATE TABLE federation_root_keys (
 project_uid TEXT NOT NULL REFERENCES projects(uid) ON DELETE CASCADE,
 authority_uid TEXT NOT NULL,
 key_id TEXT NOT NULL,
 public_key TEXT NOT NULL,
 active SMALLINT NOT NULL DEFAULT 1 CHECK(active IN (0,1)),
 PRIMARY KEY(project_uid,key_id)
);
CREATE UNIQUE INDEX uniq_federation_root_active ON federation_root_keys(project_uid) WHERE active=1;
CREATE TABLE federation_event_provenance (
 project_uid TEXT NOT NULL,
 event_uid TEXT NOT NULL,
 content_hash TEXT NOT NULL,
 reset_epoch BIGINT NOT NULL CHECK(reset_epoch>0),
 sequence BIGINT NOT NULL CHECK(sequence>0),
 key_id TEXT NOT NULL,
 receipt TEXT NOT NULL,
 PRIMARY KEY(project_uid,event_uid),
 UNIQUE(project_uid,reset_epoch,sequence),
 FOREIGN KEY(project_uid,key_id) REFERENCES federation_root_keys(project_uid,key_id) ON DELETE CASCADE
);
CREATE TABLE federation_entity_provenance (
 project_uid TEXT NOT NULL,
 kind TEXT NOT NULL CHECK(kind IN ('issue','comment')),
 entity_uid TEXT NOT NULL,
 event_uid TEXT NOT NULL,
 PRIMARY KEY(project_uid,kind,entity_uid),
 FOREIGN KEY(project_uid,event_uid) REFERENCES federation_event_provenance(project_uid,event_uid) ON DELETE CASCADE
);

-- Project-scoped transport grants remain linked to their human API credential.
ALTER TABLE api_tokens DROP CONSTRAINT api_tokens_scope_shape;
ALTER TABLE api_tokens ADD CONSTRAINT api_tokens_scope_shape CHECK (
 (scope_kind IS NULL AND scope_project_uid IS NULL AND scope_root_issue_uid IS NULL)
 OR (scope_kind='issue_subtree' AND length(scope_project_uid)=26 AND length(scope_root_issue_uid)=26 AND expires_at IS NOT NULL)
);
ALTER TABLE federation_bindings ADD COLUMN relay_config TEXT CHECK(relay_config IS NULL OR jsonb_typeof(relay_config::jsonb)='object');
ALTER TABLE federation_enrollments ADD COLUMN relay_binding_uid TEXT UNIQUE;
ALTER TABLE federation_enrollments ADD COLUMN relay_protocol_version INTEGER NOT NULL DEFAULT 0 CHECK(relay_protocol_version IN (0,1));
ALTER TABLE federation_enrollments ADD COLUMN parent_token_id BIGINT REFERENCES api_tokens(id) DEFERRABLE INITIALLY DEFERRED;
ALTER TABLE federation_enrollments ADD COLUMN relay_reset_epoch BIGINT NOT NULL DEFAULT 1 CHECK(relay_reset_epoch>0);
ALTER TABLE federation_enrollments ADD COLUMN relay_serve_downstream INTEGER NOT NULL DEFAULT 0 CHECK(relay_serve_downstream IN (0,1));
ALTER TABLE federation_enrollments ADD CONSTRAINT federation_enrollments_relay_shape CHECK (
 (relay_protocol_version=0 AND relay_binding_uid IS NULL AND parent_token_id IS NULL AND relay_serve_downstream=0)
 OR (relay_protocol_version=1 AND relay_binding_uid IS NOT NULL AND length(relay_binding_uid)=26 AND parent_token_id IS NOT NULL AND project_id IS NOT NULL)
);

-- Per-hop source mappings retain retry identities independently of source history.
CREATE TABLE federation_relay_outbox (
 id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
 project_uid TEXT NOT NULL REFERENCES projects(uid) ON DELETE CASCADE,
 binding_uid TEXT NOT NULL CHECK(length(binding_uid)=26),
 stream TEXT NOT NULL CHECK(stream IN ('events','receipts','artifacts')),
 reset_epoch BIGINT NOT NULL CHECK(reset_epoch>0),
 source_uid TEXT NOT NULL,
 source_hash TEXT NOT NULL CHECK(length(source_hash)=64),
 envelope TEXT NOT NULL,
 emitted INTEGER NOT NULL DEFAULT 0 CHECK(emitted IN (0,1)),
 acknowledged INTEGER NOT NULL DEFAULT 0 CHECK(acknowledged IN (0,1)),
 UNIQUE(binding_uid,stream,reset_epoch,source_uid),
 CHECK(acknowledged=0 OR emitted=1)
);
CREATE INDEX idx_relay_outbox_pending ON federation_relay_outbox(binding_uid,stream,reset_epoch,id) WHERE acknowledged=0;

-- Offers remain unresolved until complete data is durably committed. Numeric
-- sequence gaps are valid; the recorded offer order defines the accepted prefix.
CREATE TABLE federation_relay_inbox (
 project_uid TEXT NOT NULL REFERENCES projects(uid) ON DELETE CASCADE,
 binding_uid TEXT NOT NULL CHECK(length(binding_uid)=26),
 stream TEXT NOT NULL CHECK(stream IN ('events','receipts','artifacts')),
 reset_epoch BIGINT NOT NULL CHECK(reset_epoch>0),
 sequence BIGINT NOT NULL CHECK(sequence>0),
 source_uid TEXT NOT NULL,
 source_hash TEXT NOT NULL CHECK(length(source_hash)=64),
 envelope_digest TEXT NOT NULL CHECK(length(envelope_digest)=64),
 envelope TEXT NOT NULL,
 accepted INTEGER NOT NULL DEFAULT 0 CHECK(accepted IN (0,1)),
 PRIMARY KEY(binding_uid,stream,reset_epoch,sequence),
 UNIQUE(binding_uid,stream,reset_epoch,source_uid)
);
CREATE INDEX idx_relay_inbox_prefix ON federation_relay_inbox(binding_uid,stream,reset_epoch,sequence) WHERE accepted=0;
CREATE TABLE federation_relay_cursors (
 project_uid TEXT NOT NULL REFERENCES projects(uid) ON DELETE CASCADE,
 binding_uid TEXT NOT NULL CHECK(length(binding_uid)=26),
 stream TEXT NOT NULL CHECK(stream IN ('events','receipts','artifacts')),
 reset_epoch BIGINT NOT NULL CHECK(reset_epoch>0),
 offered_through BIGINT NOT NULL DEFAULT 0 CHECK(offered_through>=0),
 accepted_through BIGINT NOT NULL DEFAULT 0 CHECK(accepted_through>=0 AND accepted_through<=offered_through),
 emitted_through BIGINT NOT NULL DEFAULT 0 CHECK(emitted_through>=0),
 acknowledged_through BIGINT NOT NULL DEFAULT 0 CHECK(acknowledged_through>=0 AND acknowledged_through<=emitted_through),
 PRIMARY KEY(binding_uid,stream,reset_epoch)
);

-- Portable original float32 artifacts are separate from the local vector index.
-- Non-null staging_expires_at never establishes durable relay acceptance.
CREATE TABLE federation_embedding_artifacts (
  project_uid TEXT NOT NULL REFERENCES projects(uid) ON DELETE CASCADE,
  digest TEXT NOT NULL CHECK(length(digest)=64),
  issue_uid TEXT NOT NULL CHECK(length(issue_uid)=26),
  input_hash TEXT NOT NULL CHECK(length(input_hash)=64),
  recipe_fingerprint TEXT NOT NULL CHECK(length(recipe_fingerprint)=64),
  vector_byte_size BIGINT NOT NULL CHECK(vector_byte_size BETWEEN 1 AND 16777216),
  manifest TEXT NOT NULL CHECK(length(manifest) BETWEEN 1 AND 2097152),
  artifact TEXT NOT NULL CHECK(length(artifact) BETWEEN 1 AND 33554432),
  staging_expires_at TEXT,
  PRIMARY KEY(project_uid,digest)
);
CREATE INDEX idx_embedding_artifacts_input ON federation_embedding_artifacts(project_uid,issue_uid,input_hash,recipe_fingerprint);
CREATE INDEX idx_embedding_artifacts_staging ON federation_embedding_artifacts(project_uid,staging_expires_at) WHERE staging_expires_at IS NOT NULL;

