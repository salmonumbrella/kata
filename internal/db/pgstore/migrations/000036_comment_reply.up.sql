ALTER TABLE comments
  ADD COLUMN reply_to_uid TEXT,
  ADD COLUMN reply_kind TEXT,
  ADD COLUMN edited_at TEXT;

CREATE INDEX idx_comments_reply_to ON comments(reply_to_uid) WHERE reply_to_uid IS NOT NULL;
