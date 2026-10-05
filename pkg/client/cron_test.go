package client_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/oklog/ulid/v2"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

func TestCronUIDCanBeRetainedAcrossClientRetries(t *testing.T) {
	requestUID, err := client.NewCronUID()
	require.NoError(t, err)
	parsed, err := ulid.ParseStrict(requestUID)
	require.NoError(t, err)
	require.Equal(t, parsed.String(), requestUID)
	next, err := client.NewCronUID()
	require.NoError(t, err)
	require.NotEqual(t, requestUID, next)
	var received []string
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPut, r.Method)
		mu.Lock()
		received = append(received, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":"cron_conflict","message":"inspect current state"}}`))
	}))
	defer server.Close()
	api, err := client.NewWithHTTPClient(server.URL, server.Client())
	require.NoError(t, err)
	options := &generated.ObserveCronRunRequestOptions{PathParams: &generated.ObserveCronRunPath{ProjectID: 7, RunUID: requestUID}, Body: &generated.ObserveCronRunBody{}}
	for range 2 {
		_, err := api.ObserveCronRunWithResponse(context.Background(), options)
		require.Error(t, err)
	}
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{"/api/v1/projects/7/cron/runs/" + requestUID, "/api/v1/projects/7/cron/runs/" + requestUID}, received, "the SDK never substitutes a new identity after a conflict")
	require.Equal(t, requestUID, options.PathParams.RunUID)
}
