package pgstore

import "go.kenn.io/kata/internal/db"

// Creation proof and its historical pin are read in the entity's own native
// snapshot. This preserves UI consistency and applies to every scoped selector.
const attributionObject = `json_build_object('receipt',r.receipt::json,'pin',json_build_object('project_uid',k.project_uid,'authority_uid',k.authority_uid,'key_id',k.key_id,'public_key',k.public_key))::text`
const attributionJoins = `FROM federation_entity_provenance ep
 JOIN federation_event_provenance r ON r.project_uid=ep.project_uid AND r.event_uid=ep.event_uid
 JOIN federation_root_keys k ON k.project_uid=r.project_uid AND k.key_id=r.key_id
 LEFT JOIN events creation ON creation.uid=ep.event_uid`
const attributionSource = `(creation.uid IS NULL OR ep.project_uid=COALESCE(creation.source_project_uid,(SELECT uid FROM projects WHERE id=creation.project_id)))`
const issueAttributionColumn = `COALESCE((SELECT ` + attributionObject + ` ` + attributionJoins + ` WHERE ` + attributionSource + ` AND ep.kind='issue' AND ep.entity_uid=i.uid), (SELECT '{"pending":true}' FROM meta WHERE key='` + db.PendingCreationMetadataPrefix + `'||p.uid||'.issue.'||i.uid))`
const commentAttributionColumn = `COALESCE((SELECT ` + attributionObject + ` ` + attributionJoins + ` WHERE ` + attributionSource + ` AND ep.kind='comment' AND ep.entity_uid=comments.uid), (SELECT '{"pending":true}' FROM meta WHERE key='` + db.PendingCreationMetadataPrefix + `'||(SELECT p.uid FROM issues i JOIN projects p ON p.id=i.project_id WHERE i.id=comments.issue_id)||'.comment.'||comments.uid))`
const commentColumns = `id, uid, issue_id, author, body, created_at, teammate, reply_to_uid, reply_kind, edited_at, ` + commentAttributionColumn
