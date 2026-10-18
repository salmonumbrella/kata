package mcpserver

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/mcpevents"
	"go.kenn.io/kata/internal/notification"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kit/atomicfile"
)

const integrationCallback = "https://example.com/inbox"
const integrationBearer = "fixture-inbound-token"

type eventsIntegrationFixture struct {
	store          *sqlitestore.Store
	project        db.Project
	http           *httptest.Server
	service        *mcpevents.Service
	clock          atomic.Int64
	secret         string
	secretMu       sync.RWMutex
	received       chan mcpevents.Envelope
	receiptsMu     sync.Mutex
	receipts       map[string][]byte
	receiptDir     string
	dispatch       func(mcpevents.Envelope)
	restart        func()
	attempts       chan []byte
	callbackStatus atomic.Int32
}

func newEventsIntegrationFixture(t *testing.T) *eventsIntegrationFixture {
	t.Helper()
	f := &eventsIntegrationFixture{received: make(chan mcpevents.Envelope, 32), attempts: make(chan []byte, 64), receipts: map[string][]byte{}, secret: "whsec_" + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))}
	f.clock.Store(time.Now().UTC().UnixNano())
	f.callbackStatus.Store(http.StatusNoContent)
	f.receiptDir = t.TempDir()
	var err error
	f.store, err = sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "fixture.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, f.store.Close()) })
	f.project, err = f.store.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)
	backend := daemon.NewServer(daemon.ServerConfig{DB: f.store, StartedAt: f.now()})
	t.Cleanup(func() { require.NoError(t, backend.Close()) })
	backendHTTP := httptest.NewServer(backend.Handler())
	t.Cleanup(backendHTTP.Close)
	client, err := kataclient.NewWithHTTPClient(backendHTTP.URL, backendHTTP.Client())
	require.NoError(t, err)
	scope, err := NewBoundScope(ProjectIdentity{ID: f.project.ID, UID: f.project.UID, Name: f.project.Name})
	require.NoError(t, err)
	receiver := httptest.NewTLSServer(http.HandlerFunc(f.receive))
	t.Cleanup(receiver.Close)
	roots := x509.NewCertPool()
	roots.AddCert(receiver.Certificate())
	webhook := mcpevents.NewWebhookClient(mcpevents.WebhookOptions{
		Resolve: func(context.Context, string, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
		},
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			if address != "8.8.8.8:443" {
				return nil, fmt.Errorf("unvalidated test dial: %s", address)
			}
			return (&net.Dialer{}).DialContext(ctx, network, receiver.Listener.Addr().String())
		}, RootCAs: roots, Now: f.now,
	})
	credentialHash := sha256.Sum256([]byte(integrationBearer))
	stateDir := filepath.Join(t.TempDir(), "events")
	binding := mcpevents.Binding{BackendOrigin: backendHTTP.URL, DaemonUID: f.store.InstanceUID(), BackendCredentialFingerprint: strings.Repeat("a", 64), BackendPrincipal: "owner-local-fixture", TransportPrincipal: hex.EncodeToString(credentialHash[:]), Actor: "example-agent", ScopeMode: "bound", ProjectUIDs: []string{f.project.UID}}
	var serviceDone chan error
	start := func() {
		state, err := mcpevents.OpenState(stateDir, binding)
		require.NoError(t, err)
		f.service, err = mcpevents.NewService(mcpevents.ServiceOptions{State: state, Source: NewEventsSource(client, scope, f.store.InstanceUID()), Webhook: webhook, Now: f.now, PollInterval: 10 * time.Millisecond})
		require.NoError(t, err)
		serviceDone = make(chan error, 1)
		service, done := f.service, serviceDone
		go func() { done <- service.Run(t.Context()) }()
		server, err := New(Options{Client: client, Scope: scope, Actor: "example-agent", Teammate: "worker", Version: "test", Events: f.service})
		require.NoError(t, err)
		handler := NewHTTPHandler(server)
		f.http = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+integrationBearer {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			handler.ServeHTTP(w, r)
		}))
	}
	stop := func() {
		f.http.Close()
		require.NoError(t, f.service.Close())
		require.NoError(t, <-serviceDone)
	}
	start()
	f.restart = func() { stop(); start() }
	t.Cleanup(stop)

	return f
}
func (f *eventsIntegrationFixture) signingSecret() string {
	f.secretMu.RLock()
	defer f.secretMu.RUnlock()
	return f.secret
}
func (f *eventsIntegrationFixture) rotateSecret(secret string) {
	f.secretMu.Lock()
	defer f.secretMu.Unlock()
	f.secret = secret
}
func (f *eventsIntegrationFixture) now() time.Time { return time.Unix(0, f.clock.Load()).UTC() }
func (f *eventsIntegrationFixture) receive(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, mcpevents.MaxEnvelopeBytes+1))
	if err != nil || len(raw) > mcpevents.MaxEnvelopeBytes || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("Content-Type") != "application/json" {
		w.WriteHeader(400)
		return
	}
	key, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(f.signingSecret(), "whsec_"))
	mac := hmac.New(sha256.New, key)
	_, _ = io.WriteString(mac, r.Header.Get("Webhook-Id")+"."+r.Header.Get("Webhook-Timestamp")+".")
	_, _ = mac.Write(raw)
	validSignature := false
	for _, candidate := range strings.Fields(r.Header.Get("Webhook-Signature")) {
		if encoded, ok := strings.CutPrefix(candidate, "v1,"); ok {
			signature, decodeErr := base64.StdEncoding.DecodeString(encoded)
			validSignature = (decodeErr == nil && hmac.Equal(signature, mac.Sum(nil))) || validSignature
		}
	}
	stamp, timeErr := strconv.ParseInt(r.Header.Get("Webhook-Timestamp"), 10, 64)
	if timeErr != nil || !validSignature || (stamp < f.now().Unix()-300 || stamp > f.now().Unix()+300) || r.Header.Get("X-MCP-Subscription-Id") == "" {
		w.WriteHeader(401)
		return
	}
	var verification struct {
		Type      string `json:"type"`
		Challenge string `json:"challenge"`
	}
	if json.Unmarshal(raw, &verification) == nil && verification.Type == "verification" {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"challenge": verification.Challenge})
		return
	}
	var event mcpevents.Envelope
	if json.Unmarshal(raw, &event) != nil || event.EventID == "" || event.EventID != r.Header.Get("Webhook-Id") {
		w.WriteHeader(400)
		return
	}

	// Receipt durability precedes acknowledgment. Filename content comes only
	// from a SHA-256 digest, never a remotely supplied path.
	digest := sha256.Sum256([]byte(event.EventID))
	path := filepath.Join(f.receiptDir, hex.EncodeToString(digest[:])+".json")
	f.receiptsMu.Lock()
	writeErr := atomicfile.WriteNew(path, raw, atomicfile.WithPrivate())
	duplicate := errors.Is(writeErr, os.ErrExist)
	if duplicate {
		previous, readErr := os.ReadFile(path)
		var retained mcpevents.Envelope
		if readErr != nil || json.Unmarshal(previous, &retained) != nil || retained.EventID != event.EventID {
			f.receiptsMu.Unlock()
			w.WriteHeader(http.StatusConflict)
			return
		}
	} else if writeErr != nil {
		f.receiptsMu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	f.receipts[event.EventID] = bytes.Clone(raw)
	f.receiptsMu.Unlock()
	if !duplicate {
		if f.dispatch != nil {
			f.dispatch(event)
		}
		f.received <- event
	}

	f.attempts <- bytes.Clone(raw)
	w.WriteHeader(int(f.callbackStatus.Load()))
}
func (f *eventsIntegrationFixture) method(t *testing.T, method string, params map[string]any) map[string]any {
	t.Helper()
	params["_meta"] = map[string]any{"io.modelcontextprotocol/protocolVersion": "2026-07-28", "io.modelcontextprotocol/clientCapabilities": map[string]any{}}
	raw, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, f.http.URL, bytes.NewReader(raw))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+integrationBearer)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", "2026-07-28")
	req.Header.Set("Mcp-Method", method)
	if method == "tools/call" {
		name, _ := params["name"].(string)
		req.Header.Set("Mcp-Name", name)
	}
	response, err := f.http.Client().Do(req)
	require.NoError(t, err)
	defer response.Body.Close()
	raw, err = io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode, "%s", raw)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(raw, &decoded))
	require.NotContains(t, decoded, "error", "%s", raw)
	return decoded["result"].(map[string]any)
}
func (f *eventsIntegrationFixture) tool(t *testing.T, name string, args map[string]any) map[string]any {
	t.Helper()
	result := f.method(t, "tools/call", map[string]any{"name": "kata." + name, "arguments": args})
	require.NotEqual(t, true, result["isError"], "%s: %v", name, result)
	return result["structuredContent"].(map[string]any)
}
func (f *eventsIntegrationFixture) subscribe(t *testing.T, recipient string) map[string]any {
	t.Helper()
	return f.method(t, "events/subscribe", map[string]any{"name": "kata.inbox.changed", "arguments": map[string]any{"for": recipient, "project_uid": f.project.UID}, "delivery": map[string]any{"mode": "webhook", "url": integrationCallback, "secret": f.signingSecret()}})
}
func (f *eventsIntegrationFixture) next(t *testing.T, predicate func(mcpevents.Envelope) bool) mcpevents.Envelope {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case event := <-f.received:
			if predicate(event) {
				return event
			}
		case <-timer.C:
			t.Fatal("expected signed inbox event did not arrive")
			return mcpevents.Envelope{}
		}
	}
}

// The producer writes through the existing store, outside the MCP process.
// Delivery therefore proves durable-log polling, rather than an in-process hook.
func TestExternalInboxNativeWebhookEndToEnd(t *testing.T) {
	f := newEventsIntegrationFixture(t)
	for _, loader := range []string{"load_issue_mutation", "load_issue_discovery", "load_coordination"} {
		f.tool(t, loader, map[string]any{})
	}
	created := f.tool(t, "create", map[string]any{"title": "Review external request", "idempotency_key": "external-request-fixture"})["issue"].(map[string]any)
	subscription := f.subscribe(t, "example-agent/worker")
	f.next(t, func(e mcpevents.Envelope) bool { return e.Data.ResyncRequired })
	issue, err := f.store.IssueByUID(t.Context(), created["uid"].(string), db.IncludeDeletedNo)
	require.NoError(t, err)
	key := notification.MetadataKey("example-agent/worker")
	_, err = f.store.PatchIssueMetadata(t.Context(), db.PatchIssueMetadataIn{IssueID: issue.ID, IfMatchRev: new(issue.Revision), Actor: "external-producer", Patch: map[string]jsontext.Value{key: jsontext.Value(`{"from":"external-producer","message":"Review synthetic request"}`)}})
	require.NoError(t, err)
	delivery := f.next(t, func(e mcpevents.Envelope) bool { return e.Data.Reason == "notification" })
	require.Equal(t, issue.UID, delivery.Data.IssueUID)
	require.Equal(t, "example-agent/worker", delivery.Data.Recipient)
	require.NotEmpty(t, delivery.Cursor)
	inbox := f.tool(t, "inbox", map[string]any{"for": "example-agent/worker", "limit": 1})
	requests := inbox["requests"].([]any)
	require.Len(t, requests, 1)
	request := requests[0].(map[string]any)
	require.Equal(t, issue.UID, request["issue_uid"])
	shown := f.tool(t, "show", map[string]any{"ref": request["qualified_ref"]})["issue"].(map[string]any)
	require.Equal(t, "Review external request", shown["title"])
	require.Empty(t, f.tool(t, "inbox", map[string]any{"for": "example-agent/other-worker"})["requests"])
	require.Empty(t, f.tool(t, "inbox", map[string]any{"for": "example-agent"})["requests"])

	// Refresh and key rotation use the same native identity; old and new keys
	// overlap on the next occurrence, and the receiver accepts its current key.
	f.rotateSecret("whsec_" + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32)))
	refreshed := f.method(t, "events/subscribe", map[string]any{"name": "kata.inbox.changed", "arguments": map[string]any{"for": "example-agent/worker", "project_uid": f.project.UID}, "delivery": map[string]any{"mode": "webhook", "url": integrationCallback, "secret": f.signingSecret()}, "cursor": delivery.Cursor})
	require.Equal(t, subscription["id"], refreshed["id"])
	f.tool(t, "set_metadata", map[string]any{"ref": request["qualified_ref"], "revision": shown["revision"], "patch": map[string]any{key: nil}})
	cleared := f.next(t, func(e mcpevents.Envelope) bool {
		return e.Data.Reason == "notification" && e.EventID != delivery.EventID
	})
	require.Equal(t, issue.UID, cleared.Data.IssueUID)

	require.Empty(t, f.tool(t, "inbox", map[string]any{"for": "example-agent/worker"})["requests"])
	f.method(t, "events/unsubscribe", map[string]any{"name": "kata.inbox.changed", "arguments": map[string]any{"for": "example-agent/worker", "project_uid": f.project.UID}, "delivery": map[string]any{"mode": "webhook", "url": integrationCallback}})
}

func TestReceiverDurableDeduplicationAndWorkerAvailability(t *testing.T) {
	for _, availability := range []string{"idle", "busy", "unavailable"} {
		t.Run(availability, func(t *testing.T) {
			f := newEventsIntegrationFixture(t)
			worker := &fixtureWorker{availability: availability}
			f.dispatch = worker.accept
			event := mcpevents.Envelope{EventID: "fixture-event", Name: "kata.inbox.changed", Timestamp: f.now().Format(time.RFC3339Nano), Data: mcpevents.Payload{Recipient: "example-agent/worker", Reason: "notification"}, Cursor: "opaque-cursor"}
			raw, err := json.Marshal(event)
			require.NoError(t, err)
			subscriptionID := "fixture-subscription"
			post := func() {
				req := httptest.NewRequest(http.MethodPost, integrationCallback, bytes.NewReader(raw))
				stamp := strconv.FormatInt(f.now().Unix(), 10)
				mac := hmac.New(sha256.New, bytes.Repeat([]byte{7}, 32))
				_, _ = io.WriteString(mac, event.EventID+"."+stamp+".")
				_, _ = mac.Write(raw)
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Webhook-Id", event.EventID)
				req.Header.Set("Webhook-Timestamp", stamp)
				req.Header.Set("Webhook-Signature", "v1,"+base64.StdEncoding.EncodeToString(mac.Sum(nil)))
				req.Header.Set("X-MCP-Subscription-Id", subscriptionID)
				response := httptest.NewRecorder()
				f.receive(response, req)
				require.Equal(t, http.StatusNoContent, response.Code)
			}
			post()
			received := f.next(t, func(e mcpevents.Envelope) bool { return e.EventID == event.EventID })
			require.Equal(t, event.EventID, received.EventID)
			// Forget the in-memory receipt index: deduplication survives a
			// receiver restart because the durable receipt is authoritative.
			f.receiptsMu.Lock()
			f.receipts = map[string][]byte{}
			f.receiptsMu.Unlock()
			post()
			require.Empty(t, f.received, "a duplicate receipt must not dispatch a second wake")
			switch availability {
			case "idle":
				require.Equal(t, 1, worker.starts)
				require.Empty(t, worker.pending)
			case "busy":
				require.Zero(t, worker.starts)
				require.True(t, worker.pending["example-agent/worker"])
			case "unavailable":
				require.Zero(t, worker.starts)
				require.True(t, worker.pending["example-agent"])
			}

			entries, err := os.ReadDir(f.receiptDir)
			require.NoError(t, err)
			require.Len(t, entries, 1)
			receipt, err := os.ReadFile(filepath.Join(f.receiptDir, entries[0].Name()))
			require.NoError(t, err)
			require.Equal(t, raw, receipt)
			original := bytes.Clone(raw)
			// One source occurrence may have a different cursor in another
			// registration. Verify it, acknowledge it, retain the first receipt.
			event.Cursor = "another-registration-cursor"
			subscriptionID = "second-fixture-subscription"
			raw, err = json.Marshal(event)
			require.NoError(t, err)
			post()
			require.Empty(t, f.received)
			retained, err := os.ReadFile(filepath.Join(f.receiptDir, entries[0].Name()))
			require.NoError(t, err)
			require.Equal(t, original, retained)

			// A distinct later occurrence for the same inbox coalesces with
			// the busy worker's refresh; it never starts another worker.
			event.EventID = "second-fixture-event"
			raw, err = json.Marshal(event)
			require.NoError(t, err)
			post()
			f.next(t, func(e mcpevents.Envelope) bool { return e.EventID == event.EventID })
			post()
			require.Empty(t, f.received)
			if availability == "idle" {
				require.Equal(t, 1, worker.starts)
			} else {
				require.Zero(t, worker.starts)
			}
			require.Len(t, worker.pending, 1)
		})
	}
}

// fixtureWorker is a receiver-owned runtime map, not a production agent launcher.
// Busy workers coalesce by inbox; unavailable workers retain actor attention.
type fixtureWorker struct {
	availability string
	starts       int
	pending      map[string]bool
}

func (w *fixtureWorker) accept(event mcpevents.Envelope) {
	if w.pending == nil {
		w.pending = map[string]bool{}
	}
	switch w.availability {
	case "idle":
		w.starts++
		w.availability = "busy"
	case "busy":
		w.pending[event.Data.Recipient] = true
	default:
		actor, _, _ := strings.Cut(event.Data.Recipient, "/")
		w.pending[actor] = true
	}
}

func TestNativeWebhookPendingRetryAfterServiceRestart(t *testing.T) {
	f := newEventsIntegrationFixture(t)
	f.tool(t, "load_issue_mutation", map[string]any{})
	created := f.tool(t, "create", map[string]any{"title": "Restart delivery fixture", "idempotency_key": "restart-fixture"})["issue"].(map[string]any)
	sub := f.subscribe(t, "example-agent/worker")
	f.next(t, func(e mcpevents.Envelope) bool { return e.Data.ResyncRequired })
	// Wait for the bootstrap receipt to be acknowledged before losing the next
	// acknowledgment. The receiver will durably accept a new receipt but return500.
	require.Eventually(t, func() bool {
		status, err := f.service.Status(t.Context())
		return err == nil && len(status) == 1 && status[0].Delivery.Category == "accepted"
	}, 5*time.Second, 10*time.Millisecond)
	f.callbackStatus.Store(http.StatusInternalServerError)
	issue, err := f.store.IssueByUID(t.Context(), created["uid"].(string), db.IncludeDeletedNo)
	require.NoError(t, err)
	_, err = f.store.PatchIssueMetadata(t.Context(), db.PatchIssueMetadataIn{IssueID: issue.ID, Actor: "external-producer", Patch: map[string]jsontext.Value{notification.MetadataKey("example-agent/worker"): jsontext.Value(`{"from":"external-producer","message":"Review after restart"}`)}})
	require.NoError(t, err)
	delivered := f.next(t, func(e mcpevents.Envelope) bool { return e.Data.Reason == "notification" })
	nextAttempt := func() []byte {
		timer := time.NewTimer(5 * time.Second)
		defer timer.Stop()
		for {
			select {
			case raw := <-f.attempts:
				var event mcpevents.Envelope
				require.NoError(t, json.Unmarshal(raw, &event))
				if event.EventID == delivered.EventID {
					return raw
				}
			case <-timer.C:
				t.Fatal("expected retried receipt did not arrive")
				return nil
			}
		}
	}
	original := nextAttempt()
	require.Eventually(t, func() bool {
		status, err := f.service.Status(t.Context())
		return err == nil && len(status) == 1 && status[0].Delivery.Category == "http_5xx"
	}, 5*time.Second, 10*time.Millisecond)
	f.callbackStatus.Store(http.StatusNoContent)
	f.restart()
	f.clock.Add(int64(2 * time.Second))
	require.Equal(t, original, nextAttempt(), "restart retries the exact accepted-before-ack envelope")
	require.Eventually(t, func() bool {
		status, err := f.service.Status(t.Context())
		return err == nil && len(status) == 1 && status[0].ID == sub["id"] && status[0].Delivery.Category == "accepted"
	}, 5*time.Second, 10*time.Millisecond)
	require.Empty(t, f.received, "a retried durable receipt never dispatches another wake")
}
