package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/uid"
)

// SetupProjectParams binds a real workspace alias to a caller-selected stable
// project identity. AuthorityUID pins the database instance receiving the write.
// Empty AliasIdentity omits alias attachment; AliasKind must then also be empty.
type SetupProjectParams struct {
	Name, ProjectUID, AuthorityUID string
	AliasIdentity, AliasKind       string
	Actor                          string
}

// SetupProjectResult returns the project and alias from one committed transaction.
// Event is the exact newly committed creation event, or nil on an idempotent
// resolve. Callers must deliver it before doing fallible response-only work.
type SetupProjectResult struct {
	Project Project
	Alias   ProjectAlias
	Event   *Event
}

// ProjectSetupSQLAdapter supplies native row codecs and event insertion. Every
// callback runs in the caller's transaction and must not perform network I/O.
type ProjectSetupSQLAdapter struct {
	LoadProject func(context.Context, *sql.Tx, string) (Project, error)
	LoadAlias   func(context.Context, *sql.Tx, string) (ProjectAlias, error)
	InsertEvent func(context.Context, *sql.Tx, Project, string) (Event, error)
}

var (
	ErrProjectSetupConflict  = errors.New("project setup identity conflict")
	ErrProjectSetupAuthority = errors.New("project setup authority mismatch")
	ErrInvalidProjectSetup   = errors.New("invalid project setup")
)

// ValidateProjectSetup validates input before a backend opens its writer.
func ValidateProjectSetup(p SetupProjectParams) error {
	if !uid.Valid(p.ProjectUID) || p.ProjectUID == SystemProjectUID || !uid.Valid(p.AuthorityUID) {
		return fmt.Errorf("%w: valid non-reserved project and authority UIDs are required", ErrInvalidProjectSetup)
	}
	if err := config.ValidateProjectName(p.Name); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidProjectSetup, err)
	}
	if p.Name == SystemProjectName {
		return fmt.Errorf("%w: reserved project name", ErrInvalidProjectSetup)
	}
	if p.AliasIdentity == "" && p.AliasKind == "" {
		return nil
	}
	if err := config.ValidateAliasInfo(config.AliasInfo{Identity: p.AliasIdentity, Kind: p.AliasKind}); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidProjectSetup, err)
	}
	return nil
}

// SetupProjectSQL creates or resolves a UID and attaches its workspace alias
// within one native transaction. ON CONFLICT never overwrites another owner.
// The backend retries the entire transaction on its native transient errors.
func SetupProjectSQL(ctx context.Context, tx *sql.Tx, p SetupProjectParams, a ProjectSetupSQLAdapter) (SetupProjectResult, error) {
	var out SetupProjectResult
	var authority string
	if err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='instance_uid'`).Scan(&authority); err != nil {
		return out, err
	}
	if authority != p.AuthorityUID {
		return out, ErrProjectSetupAuthority
	}
	var reservedUID string
	err := tx.QueryRowContext(ctx, `SELECT project_uid FROM project_aliases WHERE alias_kind='name' AND authority_uid=$1 AND alias_identity=$2`, authority, p.Name).Scan(&reservedUID)
	if err == nil && reservedUID != p.ProjectUID {
		return out, &ProjectNameRetainedError{Name: p.Name, AuthorityUID: authority, ReservedProjectUID: reservedUID}
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return out, err
	}
	// Check the requested name even on a UID retry, so a renamed project's
	// former name cannot silently resolve through a different current owner.
	var namedUID string
	err = tx.QueryRowContext(ctx, `SELECT uid FROM projects WHERE name=$1`, p.Name).Scan(&namedUID)
	if err == nil && namedUID != p.ProjectUID {
		return out, ErrProjectSetupConflict
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return out, err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO projects(uid,name) VALUES($1,$2) ON CONFLICT DO NOTHING`, p.ProjectUID, p.Name)
	if err != nil {
		return out, err
	}
	inserted, err := res.RowsAffected()
	if err != nil {
		return out, err
	}
	out.Project, err = a.LoadProject(ctx, tx, p.ProjectUID)
	if errors.Is(err, ErrNotFound) {
		return SetupProjectResult{}, ErrProjectSetupConflict
	}
	if err != nil {
		return SetupProjectResult{}, err
	}
	if inserted == 0 {
		if err := CheckProjectAccessTransaction(ctx, tx, out.Project.UID); err != nil {
			return SetupProjectResult{}, err
		}
	}
	if out.Project.DeletedAt != nil {
		return SetupProjectResult{}, ErrProjectAlreadyArchived
	}
	if p.AliasIdentity != "" {
		_, err = tx.ExecContext(ctx, `INSERT INTO project_aliases(project_id,alias_identity,alias_kind) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, out.Project.ID, p.AliasIdentity, p.AliasKind)
		if err != nil {
			return SetupProjectResult{}, err
		}
		out.Alias, err = a.LoadAlias(ctx, tx, p.AliasIdentity)
		if err != nil {
			return SetupProjectResult{}, err
		}
		if out.Alias.ProjectID != out.Project.ID || out.Alias.AliasKind != p.AliasKind {
			return SetupProjectResult{}, ErrAliasExists
		}
	}
	if inserted != 0 {
		actor := p.Actor
		if actor == "" {
			actor = SystemActor
		}
		event, err := a.InsertEvent(ctx, tx, out.Project, actor)
		if err != nil {
			return SetupProjectResult{}, err
		}
		out.Event = &event
	}
	return out, nil
}
