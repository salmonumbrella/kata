package client

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/pkg/client/generated"
)

func TestResponseLimitPrecedesDecode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "invalid JSON larger than the permitted response")
	}))
	defer server.Close()
	client, err := NewWithHTTPClient(server.URL, server.Client())
	require.NoError(t, err)
	_, err = client.WithResponseLimit(16).ListIssues(t.Context(), &generated.ListIssuesRequestOptions{PathParams: &generated.ListIssuesPath{ProjectID: 1}})
	require.ErrorIs(t, err, ErrResponseTooLarge)
}

type countedResponseBody struct {
	read   int
	closed bool
}

func (b *countedResponseBody) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = ' '
	}
	b.read += len(p)
	return len(p), nil
}
func (b *countedResponseBody) Close() error { b.closed = true; return nil }

func TestResponseLimitStopsAndClosesStream(t *testing.T) {
	body := new(countedResponseBody)
	transport := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: body}, nil
	})
	client, err := NewWithHTTPClient("https://daemon.example", &http.Client{Transport: transport})
	require.NoError(t, err)
	_, err = client.WithResponseLimit(128).ListIssues(t.Context(), &generated.ListIssuesRequestOptions{PathParams: &generated.ListIssuesPath{ProjectID: 1}})
	require.ErrorIs(t, err, ErrResponseTooLarge)
	require.Equal(t, 129, body.read, "read at most one byte beyond the cap before stopping")
	require.True(t, body.closed, "oversized response bodies release their connection")
}
