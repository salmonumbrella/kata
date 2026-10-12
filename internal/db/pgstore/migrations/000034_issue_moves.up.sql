-- Issue moves extend existing entity, reference and history owners. No move tables.

ALTER TABLE issues ADD COLUMN moved_to_authority_uid TEXT CHECK (length(moved_to_authority_uid) = 26);

ALTER TABLE issues ADD COLUMN moved_to_project_uid TEXT CHECK (length(moved_to_project_uid) = 26);

ALTER TABLE issues ADD COLUMN moved_event_uid TEXT CHECK (length(moved_event_uid) = 26);

ALTER TABLE issues ADD COLUMN recurrence_uid TEXT CHECK (length(recurrence_uid) = 26);

ALTER TABLE issues ADD COLUMN recurrence_authority_uid TEXT CHECK (length(recurrence_authority_uid) = 26);

ALTER TABLE issues ADD COLUMN recurrence_project_uid TEXT CHECK (length(recurrence_project_uid) = 26);

ALTER TABLE issues ADD COLUMN owner_authority_uid TEXT CHECK (length(owner_authority_uid) = 26);

ALTER TABLE issues ADD COLUMN location_generation BIGINT NOT NULL DEFAULT 1 CHECK (location_generation > 0);

ALTER TABLE issues ADD COLUMN moved_to_short_id TEXT;

ALTER TABLE recurrences ADD COLUMN moved_to_authority_uid TEXT CHECK (length(moved_to_authority_uid) = 26);

ALTER TABLE recurrences ADD COLUMN moved_to_project_uid TEXT CHECK (length(moved_to_project_uid) = 26);

ALTER TABLE recurrences ADD COLUMN moved_event_uid TEXT CHECK (length(moved_event_uid) = 26);

ALTER TABLE recurrences ADD COLUMN location_generation BIGINT NOT NULL DEFAULT 1 CHECK (location_generation > 0);

ALTER TABLE events ADD COLUMN source_project_uid TEXT CHECK (length(source_project_uid) = 26);

ALTER TABLE events ADD COLUMN source_event_bytes BYTEA;

ALTER TABLE project_aliases ADD COLUMN authority_uid TEXT CHECK (length(authority_uid) = 26);

ALTER TABLE project_aliases ADD COLUMN project_uid TEXT CHECK (length(project_uid) = 26);

ALTER TABLE links ADD COLUMN from_authority_uid TEXT CHECK (length(from_authority_uid) = 26);

ALTER TABLE links ADD COLUMN from_project_uid TEXT CHECK (length(from_project_uid) = 26);

ALTER TABLE links ADD COLUMN to_authority_uid TEXT CHECK (length(to_authority_uid) = 26);

ALTER TABLE links ADD COLUMN to_project_uid TEXT CHECK (length(to_project_uid) = 26);

ALTER TABLE links ADD COLUMN from_comment_uid TEXT CHECK (length(from_comment_uid) = 26);

ALTER TABLE links ADD COLUMN to_comment_uid TEXT CHECK (length(to_comment_uid) = 26);

ALTER TABLE links ADD COLUMN reply_kind TEXT;

ALTER TABLE purge_log ADD COLUMN recurrence_uid TEXT CHECK (length(recurrence_uid) = 26);

ALTER TABLE purge_log ADD COLUMN occurrence_key TEXT;

ALTER TABLE import_mappings ADD COLUMN moved_issue_uid TEXT CHECK (length(moved_issue_uid) = 26);

ALTER TABLE import_mappings ADD COLUMN moved_to_authority_uid TEXT CHECK (length(moved_to_authority_uid) = 26);

ALTER TABLE import_mappings ADD COLUMN moved_to_project_uid TEXT CHECK (length(moved_to_project_uid) = 26);

ALTER TABLE issues ADD CONSTRAINT issues_move_shape CHECK ((moved_to_authority_uid IS NULL AND moved_to_project_uid IS NULL AND moved_to_short_id IS NULL AND moved_event_uid IS NULL) OR (moved_to_authority_uid IS NOT NULL AND moved_to_project_uid IS NOT NULL AND moved_to_short_id IS NOT NULL AND moved_event_uid IS NOT NULL));

ALTER TABLE issues ADD CONSTRAINT issues_recurrence_identity_shape CHECK ((recurrence_uid IS NULL AND recurrence_authority_uid IS NULL AND recurrence_project_uid IS NULL) OR (recurrence_uid IS NOT NULL AND recurrence_authority_uid IS NOT NULL AND recurrence_project_uid IS NOT NULL));

ALTER TABLE recurrences ADD CONSTRAINT recurrences_move_shape CHECK ((moved_to_authority_uid IS NULL AND moved_to_project_uid IS NULL AND moved_event_uid IS NULL) OR (moved_to_authority_uid IS NOT NULL AND moved_to_project_uid IS NOT NULL AND moved_event_uid IS NOT NULL));

ALTER TABLE project_aliases ADD CONSTRAINT project_aliases_identity_shape CHECK ((alias_kind IN ('git','local') AND project_id IS NOT NULL) OR (alias_kind='name' AND authority_uid IS NOT NULL AND project_uid IS NOT NULL));

ALTER TABLE links ADD CONSTRAINT links_endpoint_shape CHECK ((type IN ('parent','blocks','related') AND from_issue_uid IS NOT NULL AND to_issue_uid IS NOT NULL AND from_comment_uid IS NULL AND to_comment_uid IS NULL) OR (type='reply' AND from_comment_uid IS NOT NULL AND to_comment_uid IS NOT NULL AND reply_kind IN ('reply','confirm','refute','supersede')));

ALTER TABLE import_mappings ADD CONSTRAINT import_mappings_move_shape CHECK ((moved_issue_uid IS NULL AND moved_to_authority_uid IS NULL AND moved_to_project_uid IS NULL) OR (moved_issue_uid IS NOT NULL AND moved_to_authority_uid IS NOT NULL AND moved_to_project_uid IS NOT NULL));

ALTER TABLE events ALTER COLUMN project_id DROP NOT NULL;

ALTER TABLE project_aliases ALTER COLUMN project_id DROP NOT NULL;

ALTER TABLE project_aliases DROP CONSTRAINT project_aliases_alias_identity_key;

ALTER TABLE project_aliases DROP CONSTRAINT project_aliases_alias_kind_check;

ALTER TABLE project_aliases ADD CONSTRAINT project_aliases_alias_kind_check CHECK(alias_kind IN ('git','local','name'));

ALTER TABLE links ALTER COLUMN from_issue_id DROP NOT NULL;

ALTER TABLE links ALTER COLUMN to_issue_id DROP NOT NULL;

ALTER TABLE links ALTER COLUMN from_issue_uid DROP NOT NULL;

ALTER TABLE links ALTER COLUMN to_issue_uid DROP NOT NULL;

ALTER TABLE links DROP CONSTRAINT links_unique_edge;

ALTER TABLE links DROP CONSTRAINT links_type_check;

ALTER TABLE links DROP CONSTRAINT links_not_self_check;

ALTER TABLE links DROP CONSTRAINT links_related_order_check;

ALTER TABLE links ADD CONSTRAINT links_type_check CHECK(type IN ('parent','blocks','related','reply'));

ALTER TABLE links ADD CONSTRAINT links_not_self_check CHECK(type='reply' OR from_issue_id <> to_issue_id);

DROP INDEX uniq_one_parent_per_child;

CREATE UNIQUE INDEX uniq_one_parent_per_child ON links(from_issue_uid) WHERE type='parent' AND from_comment_uid IS NULL AND to_comment_uid IS NULL;

ALTER TABLE federation_root_keys DROP CONSTRAINT federation_root_keys_project_uid_fkey;

ALTER TABLE federation_event_provenance DROP CONSTRAINT federation_event_provenance_project_uid_key_id_fkey;

ALTER TABLE federation_event_provenance ADD CONSTRAINT federation_event_provenance_project_uid_key_id_fkey FOREIGN KEY(project_uid,key_id) REFERENCES federation_root_keys(project_uid,key_id) ON DELETE RESTRICT;

ALTER TABLE federation_entity_provenance DROP CONSTRAINT federation_entity_provenance_project_uid_event_uid_fkey;

ALTER TABLE federation_entity_provenance ADD CONSTRAINT federation_entity_provenance_project_uid_event_uid_fkey FOREIGN KEY(project_uid,event_uid) REFERENCES federation_event_provenance(project_uid,event_uid) ON DELETE RESTRICT;

ALTER TABLE import_mappings DROP CONSTRAINT import_mappings_issue_id_fkey;

ALTER TABLE import_mappings ADD CONSTRAINT import_mappings_issue_id_fkey FOREIGN KEY(issue_id) REFERENCES issues(id) ON DELETE SET NULL;

ALTER TABLE import_mappings DROP CONSTRAINT import_mappings_comment_id_fkey;

ALTER TABLE import_mappings ADD CONSTRAINT import_mappings_comment_id_fkey FOREIGN KEY(comment_id) REFERENCES comments(id) ON DELETE SET NULL;

ALTER TABLE import_mappings DROP CONSTRAINT import_mappings_link_id_fkey;

ALTER TABLE import_mappings ADD CONSTRAINT import_mappings_link_id_fkey FOREIGN KEY(link_id) REFERENCES links(id) ON DELETE SET NULL;

DO $$
DECLARE owner_check RECORD;
BEGIN
 FOR owner_check IN SELECT conname FROM pg_constraint
 WHERE conrelid='import_mappings'::regclass AND contype='c'
 AND pg_get_constraintdef(oid) LIKE '%object_type%'
 AND pg_get_constraintdef(oid) LIKE '%issue_id%'
 LOOP EXECUTE format('ALTER TABLE import_mappings DROP CONSTRAINT %I', owner_check.conname); END LOOP;
END $$;

ALTER TABLE import_mappings ADD CONSTRAINT import_mappings_issue_owner_check CHECK (moved_issue_uid IS NOT NULL OR object_type != 'issue' OR (issue_id IS NOT NULL));

ALTER TABLE import_mappings ADD CONSTRAINT import_mappings_comment_owner_check CHECK (moved_issue_uid IS NOT NULL OR object_type != 'comment' OR (issue_id IS NOT NULL AND comment_id IS NOT NULL));

ALTER TABLE import_mappings ADD CONSTRAINT import_mappings_label_owner_check CHECK (moved_issue_uid IS NOT NULL OR object_type != 'label' OR (issue_id IS NOT NULL AND label IS NOT NULL));

ALTER TABLE import_mappings ADD CONSTRAINT import_mappings_link_owner_check CHECK (moved_issue_uid IS NOT NULL OR object_type != 'link' OR (issue_id IS NOT NULL AND link_id IS NOT NULL));

CREATE OR REPLACE FUNCTION enforce_links_uid_consistency() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  from_uid TEXT;
  to_uid TEXT;
BEGIN
  SELECT uid INTO from_uid FROM issues WHERE id = NEW.from_issue_id;
  SELECT uid INTO to_uid   FROM issues WHERE id = NEW.to_issue_id;
  IF NEW.from_issue_id IS NOT NULL AND NEW.from_issue_uid IS DISTINCT FROM from_uid THEN
    RAISE EXCEPTION 'from_issue_uid does not match from_issue_id';
  END IF;
  IF NEW.to_issue_id IS NOT NULL AND NEW.to_issue_uid IS DISTINCT FROM to_uid THEN
    RAISE EXCEPTION 'to_issue_uid does not match to_issue_id';
  END IF;
  RETURN NEW;
END $$;

-- Backfill only identities available from real existing owners. Signed source
-- payload/hash/actor bytes are never rewritten or synthesized.
UPDATE events e SET source_project_uid=p.uid FROM projects p WHERE p.id=e.project_id;
UPDATE issues i SET owner_authority_uid=COALESCE(
 (SELECT k.authority_uid FROM federation_root_keys k JOIN projects p ON p.uid=k.project_uid WHERE p.id=i.project_id AND k.active=1),
 (SELECT value FROM meta WHERE key='instance_uid'));
UPDATE issues i SET recurrence_uid=r.uid,
 recurrence_authority_uid=COALESCE((SELECT k.authority_uid FROM federation_root_keys k JOIN projects p ON p.uid=k.project_uid WHERE p.id=r.project_id AND k.active=1),(SELECT value FROM meta WHERE key='instance_uid')),
 recurrence_project_uid=p.uid FROM recurrences r JOIN projects p ON p.id=r.project_id WHERE r.id=i.recurrence_id;
UPDATE links l SET from_authority_uid=i.owner_authority_uid,from_project_uid=p.uid FROM issues i JOIN projects p ON p.id=i.project_id WHERE i.id=l.from_issue_id;
UPDATE links l SET to_authority_uid=i.owner_authority_uid,to_project_uid=p.uid FROM issues i JOIN projects p ON p.id=i.project_id WHERE i.id=l.to_issue_id;

-- Issue moves extend existing entity and audit owners; no move tables.
ALTER TABLE issues ADD CONSTRAINT issues_moved_event_uid_fkey FOREIGN KEY (moved_event_uid) REFERENCES events(uid) ON DELETE RESTRICT;
ALTER TABLE recurrences ADD CONSTRAINT recurrences_moved_event_uid_fkey FOREIGN KEY (moved_event_uid) REFERENCES events(uid) ON DELETE RESTRICT;
CREATE INDEX idx_comments_reply_to ON comments(reply_to_uid) WHERE reply_to_uid IS NOT NULL;
CREATE INDEX idx_issues_move_route ON issues(moved_to_authority_uid,moved_to_project_uid,uid) WHERE moved_event_uid IS NOT NULL;
CREATE UNIQUE INDEX uniq_issues_series_key ON issues(recurrence_uid,occurrence_key) WHERE recurrence_uid IS NOT NULL AND occurrence_key IS NOT NULL;
CREATE INDEX idx_recurrences_move_route ON recurrences(moved_to_project_uid,uid) WHERE moved_event_uid IS NOT NULL;
CREATE UNIQUE INDEX uniq_purge_log_series_key ON purge_log(recurrence_uid,occurrence_key) WHERE recurrence_uid IS NOT NULL AND occurrence_key IS NOT NULL;
CREATE INDEX idx_events_move_request ON events(project_id,type,(payload::jsonb->>'request_uid'),issue_uid);
CREATE UNIQUE INDEX uniq_events_move_request ON events(project_id,type,(payload::jsonb->>'request_uid'),issue_uid) WHERE type IN ('issue.move_received','issue.moved') AND (payload::jsonb->>'request_uid') IS NOT NULL;
CREATE INDEX idx_events_move_reference ON events((payload::jsonb->>'from_authority_uid'),(payload::jsonb->>'from_project_uid'),(payload::jsonb->>'from_short_id'),issue_uid) WHERE type='issue.moved';
CREATE INDEX idx_events_move_location ON events(issue_uid,type,id DESC) WHERE type IN ('issue.move_received','issue.moved');
CREATE UNIQUE INDEX uniq_project_aliases_workspace_identity ON project_aliases(alias_identity) WHERE alias_kind IN ('git','local');
CREATE UNIQUE INDEX uniq_project_aliases_name ON project_aliases(authority_uid,alias_identity) WHERE alias_kind='name';
CREATE INDEX idx_project_aliases_namespace ON project_aliases(authority_uid,project_uid,alias_kind);
CREATE UNIQUE INDEX uniq_links_issue_edge ON links(from_issue_uid,to_issue_uid,type) WHERE from_comment_uid IS NULL AND to_comment_uid IS NULL;
CREATE UNIQUE INDEX uniq_links_comment_edge ON links(from_comment_uid,to_comment_uid,type) WHERE from_comment_uid IS NOT NULL AND to_comment_uid IS NOT NULL;
CREATE INDEX idx_links_from_comment ON links(from_comment_uid) WHERE from_comment_uid IS NOT NULL;
CREATE INDEX idx_links_to_comment ON links(to_comment_uid) WHERE to_comment_uid IS NOT NULL;
CREATE INDEX idx_import_mappings_moved ON import_mappings(moved_issue_uid) WHERE moved_issue_uid IS NOT NULL;
