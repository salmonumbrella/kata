package sqlitestore_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json/jsontext"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/uid"
)

type evidenceConnector struct {
	driver driver.Driver
	path string
	queries *atomic.Int64
}

func (c evidenceConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := c.driver.Open(c.path)
	if err != nil { return nil, err }
	return evidenceConn{Conn: conn, queries: c.queries}, nil
}
func (c evidenceConnector) Driver() driver.Driver { return c.driver }

type evidenceConn struct {
	driver.Conn
	queries *atomic.Int64
}

func (c evidenceConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
}
func (c evidenceConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if strings.Contains(query, "SELECT EXISTS(SELECT 1 FROM cron_runs") {
		c.queries.Add(1)
	}
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
}

type evidenceStore struct {
	*sqlitestore.Store
	reader *sqlitestore.Store
}

func (s evidenceStore) ListCronRequests(ctx context.Context, in db.CronRequestList) ([]db.CronRequest, error) {
	return s.reader.ListCronRequests(ctx, in)
}

// A request-aware read must derive evidence only for its requested UID.
func TestCronRequestResolutionEvidenceQueries(t *testing.T) {
	store, path := openTestDBWithPath(t)
	ctx := t.Context()
	project := createProject(ctx, t, store, "request-project")
	definition := cron.JobDefinition{Version: 1, Kind: "job", Enabled: true, Trigger: cron.Trigger{Kind: "interval", IntervalSeconds: 60}, Action: cron.Action{Kind: "execute", Prompt: "Inspect"}, Issue: &cron.IssuePolicy{Kind: "per-run", Title: "Inspect"}, Overlap: "forbid", Catchup: "all", Options: []byte(`{"inputs":{"batch":{"type":"int"}}}`)}
	job, _, err := store.PutCronJob(ctx, db.PutCronJob{ProjectID: project.ID, Actor: "operator", Name: "Inspect", Definition: definition})
	require.NoError(t, err)
	var selected string
	for i := 0; i < 4; i++ {
		id, err := uid.New()
		require.NoError(t, err)
		_, err = store.RequestCron(ctx, db.RequestCronIn{ProjectID: project.ID, UID: id, Kind: "run", TargetUID: job.UID, Actor: "operator", Inputs: map[string]jsontext.Value{"batch": jsontext.Value("3")}})
		require.NoError(t, err)
		selected = id
	}
	var queries atomic.Int64
	raw := sql.OpenDB(evidenceConnector{driver: store.DB.Driver(), path: path, queries: &queries})
	raw.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	cfg := daemon.ServerConfig{DB: evidenceStore{Store: store, reader: &sqlitestore.Store{DB: raw}}}
	cfg.Auth.Token = "fixture-token"
	server := daemon.NewServer(cfg)
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	request := httptest.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/cron/jobs/%s/inputs?request_uid=%s", project.ID, job.UID, strings.ToLower(selected)), nil)
	request.Header.Set("Authorization", "Bearer fixture-token")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), `"source":"request"`)
	require.EqualValues(t, 1, queries.Load(), "unrelated requests must not trigger evidence reads")
}
