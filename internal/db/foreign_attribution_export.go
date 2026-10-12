package db

import (
	"context"
	"fmt"
	"strings"
)

// ForeignAttributionExportClause selects only original proofs attached to
// entities in the current authorized export scope. It never selects an entire
// foreign project's receipts merely because one issue moved from that project.
func ForeignAttributionExportClause(ctx context.Context, filter ExportFilter, kind string, args []any, postgres bool) (string, []any) {
	return attributionOwnerExportClause(ctx, filter, kind, args, postgres, true, true)
}

// EntityAttributionOwnerClause intersects direct proof reads with the real
// current body or retained issue purge owner. Owner reads preserve their existing
// ability to inspect historical receipts.
func EntityAttributionOwnerClause(ctx context.Context, args []any, postgres bool) (string, []any) {
	if _, restricted := AuthorizedProjects(ctx); !restricted {
		return "TRUE", args
	}
	return attributionOwnerExportClause(ctx, ExportFilter{IncludeDeleted: true}, "federation_entity_provenance", args, postgres, false, true)
}

// LegacyForeignAttributionExportClause supports frozen pre-move SQLite stores
// without referencing columns introduced by the schema34 cutover.
func LegacyForeignAttributionExportClause(ctx context.Context, filter ExportFilter, kind string, args []any) (string, []any) {
	return attributionOwnerExportClause(ctx, filter, kind, args, false, true, false)
}

func attributionOwnerExportClause(ctx context.Context, filter ExportFilter, kind string, args []any, postgres, foreignOnly, retainedComments bool) (string, []any) {
	bind := func(value any) string {
		args = append(args, value)
		if postgres {
			return fmt.Sprintf("$%d", len(args))
		}
		return "?"
	}
	allowed, restricted := AuthorizedProjects(ctx)
	if restricted && len(allowed) == 0 {
		return "FALSE", args
	}
	ref := "ep"
	if kind == "federation_entity_provenance" {
		ref = "k"
	}
	bodyScope := func(comment bool) string {
		from := "issues i JOIN projects owner_project ON owner_project.id=i.project_id"
		predicate := ref + ".kind='issue' AND i.uid=" + ref + ".entity_uid"
		if comment {
			from = "comments c JOIN issues i ON i.id=c.issue_id JOIN projects owner_project ON owner_project.id=i.project_id"
			predicate = ref + ".kind='comment' AND c.uid=" + ref + ".entity_uid"
		}
		if foreignOnly {
			predicate += " AND (p.id IS NULL OR i.project_id<>p.id)"
		}
		if !filter.IncludeDeleted {
			predicate += " AND i.deleted_at IS NULL"
		}
		if filter.ProjectID != nil {
			predicate += " AND i.project_id=" + bind(*filter.ProjectID)
		}
		if restricted {
			places := make([]string, len(allowed))
			for i, uid := range allowed {
				places[i] = bind(uid)
			}
			predicate += " AND owner_project.uid IN (" + strings.Join(places, ",") + ")"
		}
		return "EXISTS(SELECT 1 FROM " + from + " WHERE " + predicate + ")"
	}

	purgeScope := func() string {
		if !filter.IncludeDeleted {
			return "FALSE"
		}
		commentUID := "json_extract(original.payload,'$.comment_uid')"
		if postgres {
			commentUID = "(original.payload::jsonb->>'comment_uid')"
		}
		predicate := ref + ".kind='issue' AND tombstone.issue_uid=" + ref + ".entity_uid"
		if retainedComments {
			predicate = "(" + ref + ".kind='issue' AND tombstone.issue_uid=" + ref + ".entity_uid OR " + ref + ".kind='comment' AND EXISTS(SELECT 1 FROM events original WHERE original.uid=" + ref + ".event_uid AND original.source_project_uid=" + ref + ".project_uid AND original.source_event_bytes IS NOT NULL AND original.type='issue.commented' AND original.issue_uid=tombstone.issue_uid AND " + commentUID + "=" + ref + ".entity_uid))"
		}
		if foreignOnly {
			predicate += " AND (p.id IS NULL OR tombstone.project_uid<>p.uid)"
		}
		if filter.ProjectID != nil {
			predicate += " AND tombstone.project_id=" + bind(*filter.ProjectID)
		}
		if restricted {
			places := make([]string, len(allowed))
			for i, uid := range allowed {
				places[i] = bind(uid)
			}
			predicate += " AND tombstone.project_uid IN (" + strings.Join(places, ",") + ")"
		}
		return "EXISTS(SELECT 1 FROM purge_log tombstone WHERE " + predicate + ")"
	}
	visible := "(" + bodyScope(false) + " OR " + bodyScope(true) + " OR " + purgeScope() + ")"

	if ref == "k" {
		return visible, args
	}
	match := "ep.project_uid=k.project_uid"
	from := "federation_entity_provenance ep"
	if kind == "federation_root_key" {
		from += " JOIN federation_event_provenance receipt ON receipt.project_uid=ep.project_uid AND receipt.event_uid=ep.event_uid"
		match += " AND receipt.key_id=k.key_id"
	} else {
		match += " AND ep.event_uid=k.event_uid"
	}
	return "EXISTS(SELECT 1 FROM " + from + " WHERE " + match + " AND " + visible + ")", args
}
