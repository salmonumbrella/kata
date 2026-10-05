package sqlitestore

import (
	"context"
	"fmt"
	"strings"
)

// Version 31 was never issued with execution-authority tables. Do not silently
// open or repair an experimental database bearing that same version. Restore
// a compatible backup or export it with its matching binary and rebuild.
func (d *Store) validateCronSchema(ctx context.Context) error {
	var legacy int
	if err := d.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name IN ('cron_run_claims','cron_issue_holders')`).Scan(&legacy); err != nil {
		return err
	}
	if legacy != 0 {
		return fmt.Errorf("incompatible experimental cron schema 31: restore a compatible backup or export with the matching binary and rebuild; database unchanged")
	}
	var exclusive int
	if err := d.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_index_list('cron_runs') indexes
 WHERE indexes."unique" = 1 AND
 COALESCE((SELECT group_concat(name, ',') FROM (SELECT name FROM pragma_index_info(indexes.name) ORDER BY seqno)), '') <> 'uid'`).Scan(&exclusive); err != nil {
		return err
	}
	if exclusive != 0 {
		return fmt.Errorf("incompatible experimental cron schema 31: run history retains occurrence exclusivity; export with its matching binary and rebuild; database unchanged")
	}
	for table, expected := range map[string]string{
		"cron_jobs":  "id,uid,project_id,name,definition_json,definition_event_uid,definition_hlc_json,author,revision,created_at,updated_at,deleted_at",
		"cron_flows": "id,uid,project_id,name,definition_json,definition_event_uid,definition_hlc_json,author,revision,created_at,updated_at,deleted_at",
		"cron_runs":  "id,uid,project_id,job_uid,definition_event_uid,flow_uid,flow_definition_event_uid,occurrence_key,issue_uid,actor,teammate,executor_label,status,summary_json,revision,created_at,started_at,ended_at,updated_at",
	} {
		rows, err := d.QueryContext(ctx, "SELECT name FROM pragma_table_info(?) ORDER BY cid", table)
		if err != nil {
			return err
		}
		var columns []string
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				_ = rows.Close()
				return err
			}
			columns = append(columns, name)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return err
		}
		if strings.Join(columns, ",") != expected {
			return fmt.Errorf("incompatible experimental cron schema 31 (%s): restore a compatible backup or export with the matching binary and rebuild; database unchanged", table)
		}
	}
	return nil
}
