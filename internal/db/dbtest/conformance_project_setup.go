package dbtest

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

type projectSetupStore interface {
	SetupProject(context.Context, db.SetupProjectParams) (db.SetupProjectResult, error)
}

// RunProjectSetup exercises the explicit atomic stable-UID setup contract.
// Splitting creation and alias writes breaks rollback; allocating a new UID
// on retry breaks recovery after a lost acknowledgement.
func RunProjectSetup(t *testing.T, store db.Storage) {
	setup, ok := store.(projectSetupStore)
	require.True(t, ok, "backend must support atomic project setup")
	ctx := t.Context()
	params := db.SetupProjectParams{Name: "example-project", ProjectUID: setupUID(t), AuthorityUID: store.InstanceUID(), AliasIdentity: "example.com/team/repo", AliasKind: "git", Actor: "setup-author"}
	first, err := setup.SetupProject(ctx, params)
	require.NoError(t, err)
	require.Equal(t, params.ProjectUID, first.Project.UID)
	require.Equal(t, first.Project.ID, first.Alias.ProjectID)
	require.NotNil(t, first.Event)
	require.Equal(t, "project.created", first.Event.Type)
	require.Equal(t, "setup-author", first.Event.Actor)
	events, err := store.EventsByUIDs(ctx, first.Project.ID, []string{first.Event.UID})
	require.NoError(t, err)
	require.Equal(t, []db.Event{*first.Event}, events)
	before, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	// Ignore the first acknowledgement and retry the saved UID.
	retry, err := setup.SetupProject(ctx, params)
	require.NoError(t, err)
	require.Equal(t, first.Project, retry.Project)
	require.Equal(t, first.Alias, retry.Alias)
	require.Nil(t, retry.Event)
	after, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	require.Equal(t, before, after)

	for _, change := range []func(*db.SetupProjectParams){
		func(p *db.SetupProjectParams) { p.ProjectUID = setupUID(t) },
		func(p *db.SetupProjectParams) { p.Name = "other-project"; p.ProjectUID = setupUID(t) },
		func(p *db.SetupProjectParams) { p.AuthorityUID = setupUID(t) },
		func(p *db.SetupProjectParams) { p.AliasKind = "name" },
	} {
		conflict := params
		change(&conflict)
		_, err := setup.SetupProject(ctx, conflict)
		require.Error(t, err)
		after, err := store.MaxEventID(ctx)
		require.NoError(t, err)
		require.Equal(t, before, after)
		if conflict.ProjectUID != params.ProjectUID {
			_, err = store.ProjectByUID(ctx, conflict.ProjectUID)
			require.ErrorIs(t, err, db.ErrNotFound)
		}
	}
	// Existing UID remains authoritative after a rename; setup must not undo it.
	renamed, err := store.RenameProject(ctx, first.Project.ID, "renamed-project")
	require.NoError(t, err)
	retry, err = setup.SetupProject(ctx, params)
	require.NoError(t, err)
	require.Equal(t, renamed, retry.Project)
	require.Nil(t, retry.Event)

	// Retained names cannot be reclaimed even when their project row is absent.
	q := store.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	})
	_, err = q.ExecContext(ctx, `INSERT INTO project_aliases(project_id,alias_identity,alias_kind,authority_uid,project_uid) VALUES(NULL,$1,'name',$2,$3)`, "retained-project", store.InstanceUID(), setupUID(t))
	require.NoError(t, err)
	retained := params
	retained.Name = "retained-project"
	retained.ProjectUID = setupUID(t)
	retained.AliasIdentity = "local:///retained-workspace"
	retained.AliasKind = "local"
	_, err = setup.SetupProject(ctx, retained)
	require.ErrorIs(t, err, db.ErrProjectNameRetained)
	_, err = store.ProjectByUID(ctx, retained.ProjectUID)
	require.ErrorIs(t, err, db.ErrNotFound)

	// Rejection by an existing request fence prevents both writes.
	rejected := params
	rejected.Name = "fenced-project"
	rejected.ProjectUID = setupUID(t)
	rejected.AliasIdentity = "local:///fenced-workspace"
	rejected.AliasKind = "local"
	rejection := errors.New("setup access denied")
	fenced := db.WithTransactionFence(ctx, func(context.Context, db.Transaction) error { return rejection })
	_, err = setup.SetupProject(fenced, rejected)
	require.ErrorIs(t, err, rejection)
	_, err = store.ProjectByUID(ctx, rejected.ProjectUID)
	require.ErrorIs(t, err, db.ErrNotFound)

	// A native resolve must recheck the current project grant before alias
	// attachment, even when request admission did not discover this UID.
	liveRejected := params
	liveRejected.AliasIdentity = "local:///denied-workspace"
	liveRejected.AliasKind = "local"
	liveCtx := db.WithProjectAccessTransactionCheck(ctx, func(_ context.Context, _ db.Transaction, projectUID string) error {
		require.Equal(t, params.ProjectUID, projectUID)
		return rejection
	})
	_, err = setup.SetupProject(liveCtx, liveRejected)
	require.ErrorIs(t, err, rejection)
	_, err = store.AliasByIdentity(ctx, liveRejected.AliasIdentity)
	require.ErrorIs(t, err, db.ErrNotFound)

	// Archived identities are not silently revived.
	_, _, err = store.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: first.Project.ID, Actor: "setup-author"})
	require.NoError(t, err)
	_, err = setup.SetupProject(ctx, params)
	require.Error(t, err)
}

func RunProjectSetupConcurrent(t *testing.T, store db.Storage, other db.Storage) {
	setup, ok := store.(projectSetupStore)
	require.True(t, ok, "backend must support atomic project setup")
	second, ok := other.(projectSetupStore)
	require.True(t, ok, "backend must support atomic project setup")
	writers := []projectSetupStore{setup, second}
	params := db.SetupProjectParams{Name: "concurrent-project", ProjectUID: setupUID(t), AuthorityUID: store.InstanceUID(), AliasIdentity: "local:///example-workspace", AliasKind: "local", Actor: "setup-author"}
	const n = 6
	results := make([]db.SetupProjectResult, n)
	errs := make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i], errs[i] = writers[i%len(writers)].SetupProject(t.Context(), params)
		}()
	}
	close(start)
	wg.Wait()
	created := 0
	for i := range n {
		require.NoError(t, errs[i])
		require.Equal(t, params.ProjectUID, results[i].Project.UID)
		require.Equal(t, results[0].Project.ID, results[i].Alias.ProjectID)
		if results[i].Event != nil {
			created++
		}
	}
	require.Equal(t, 1, created)
	projects, err := store.ListProjects(t.Context())
	require.NoError(t, err)
	require.Len(t, projects, 1)
}

func setupUID(t *testing.T) string {
	t.Helper()
	s, err := uid.New()
	require.NoError(t, err)
	return s
}
