package mcpevents

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func runtimeFixture(t *testing.T, status int) (*Service, *State, *fixtureSource, *time.Time, *[]Envelope) {
	t.Helper()
	s, state, source, now, events := serviceFixture(t, status)
	t.Cleanup(func() { _ = s.Close() })
	return s, state, source, now, events
}

func TestEventServiceBootstrapAndSourceDelivery(t *testing.T) {
	s, state, source, now, events := runtimeFixture(t, 202)
	result, err := s.Subscribe(context.Background(), serviceRequest())
	if err != nil || result.ID == "" || result.Truncated || result.Cursor == "" {
		t.Fatalf("subscribe=%+v error=%v", result, err)
	}
	records, _ := state.Load()
	if records[0].Pending == nil || records[0].Positions[stateBinding().ProjectUIDs[0]] != 12 {
		t.Fatal("bootstrap did not persist current head and resync before activation")
	}
	if err := s.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	source.page = SourcePage{Events: []SourceEvent{{EventID: "evt_source", SourceEventUID: "01ARZ3NDEKTSV4RRFFQ69G5FAX", ProjectUID: stateBinding().ProjectUIDs[0], Reason: "notification", Timestamp: now.Add(-time.Hour), Positions: Position{stateBinding().ProjectUIDs[0]: 13}}}, Positions: Position{stateBinding().ProjectUIDs[0]: 13}}
	if err := s.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(*events) != 2 || (*events)[0].Data.Reason != "resync" || (*events)[1].EventID != "evt_source" || (*events)[1].Timestamp != now.Add(-time.Hour).Format(time.RFC3339Nano) {
		t.Fatalf("wrong source occurrences: %+v", *events)
	}
	records, _ = state.Load()
	if records[0].Pending != nil || records[0].Positions[stateBinding().ProjectUIDs[0]] != 13 {
		t.Fatal("receipt did not advance safe vector")
	}
}

func TestLiveStaleRefreshDoesNotRewind(t *testing.T) {
	s, state, _, _, _ := runtimeFixture(t, 503)
	request := serviceRequest()
	initial, err := s.Subscribe(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	before, _ := state.Load()
	request.Cursor = &initial.Cursor
	if _, err = s.Subscribe(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	after, _ := state.Load()
	if !reflect.DeepEqual(before[0].Positions, after[0].Positions) || !reflect.DeepEqual(before[0].Pending, after[0].Pending) {
		t.Fatal("stale refresh rewound or replaced accepted pending state")
	}
	future, _ := EncodeCursor(state.Identity(), request.Name, request.Arguments, 0, Position{stateBinding().ProjectUIDs[0]: 999})
	request.Cursor = &future
	if _, err = s.Subscribe(context.Background(), request); err == nil {
		t.Fatal("refresh fast-forwarded safe watermark")
	}
}

func TestEventServiceTTLAndExpiredDelivery(t *testing.T) {
	s, state, _, now, events := runtimeFixture(t, 204)
	request := serviceRequest()
	zero := int64(0)
	request.TTLMS = &zero
	if _, err := s.Subscribe(context.Background(), request); err == nil {
		t.Fatal("zero TTL accepted")
	}
	negative := int64(-1)
	request.TTLMS = &negative
	if _, err := s.Subscribe(context.Background(), request); err == nil {
		t.Fatal("negative TTL accepted")
	}
	one := int64(1)
	request.TTLMS = &one
	result, err := s.Subscribe(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	expires, _ := time.Parse(time.RFC3339Nano, result.RefreshBefore)
	if !expires.Equal(now.Add(time.Minute)) {
		t.Fatalf("minimum TTL not granted: %s", result.RefreshBefore)
	}
	*now = now.Add(time.Minute)
	if err = s.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(*events) != 0 {
		t.Fatal("expired subscription delivered")
	}
	request.TTLMS = nil
	result, err = s.Subscribe(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	expires, _ = time.Parse(time.RFC3339Nano, result.RefreshBefore)
	if !expires.Equal(now.Add(24 * time.Hour)) {
		t.Fatal("null TTL was not finite 24 hours")
	}
	records, _ := state.Load()
	if records[0].ExpiresAt.IsZero() {
		t.Fatal("expiry not persisted")
	}
}

func TestEventServiceAuthorizationFailureDoesNotConsumeCallbackAttempt(t *testing.T) {
	s, state, source, now, events := runtimeFixture(t, 204)
	if _, err := s.Subscribe(context.Background(), serviceRequest()); err != nil {
		t.Fatal(err)
	}
	source.authErr = ErrUnavailable
	if err := s.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	records, _ := state.Load()
	if records[0].Pending.Attempts != 0 || records[0].Disabled || len(*events) != 0 {
		t.Fatal("temporary authority failure consumed callback or revoked subscription")
	}
	source.authErr = ErrRevoked
	*now = now.Add(time.Second)
	if err := s.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	records, _ = state.Load()
	if !records[0].Disabled || len(*events) != 0 {
		t.Fatal("confirmed revocation did not disable pending delivery")
	}
}

func TestEventServiceSourceResetGenerationAndAcknowledgment(t *testing.T) {
	s, state, source, _, events := runtimeFixture(t, 204)
	request := serviceRequest()
	first, err := s.Subscribe(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	source.page = SourcePage{Reset: true, Positions: Position{stateBinding().ProjectUIDs[0]: 20}}
	if err = s.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	source.page = SourcePage{}
	request.Cursor = &first.Cursor
	reset, err := s.Subscribe(context.Background(), request)
	if err != nil || !reset.Truncated {
		t.Fatalf("reset not disclosed: %+v %v", reset, err)
	}
	request.Cursor = &reset.Cursor
	acked, err := s.Subscribe(context.Background(), request)
	if err != nil || acked.Truncated {
		t.Fatalf("new continuity generation did not acknowledge gap: %+v %v", acked, err)
	}
	if len(*events) != 2 || !(*events)[1].Data.ResyncRequired {
		t.Fatalf("missing resync webhook: %+v", *events)
	}
	records, _ := state.Load()
	if records[0].ContinuityLost {
		t.Fatal("acknowledged continuity remains stale")
	}
}

func TestUnsubscribeFencesInflightGeneration(t *testing.T) {
	s, state, _, _, _ := runtimeFixture(t, 204)
	request := serviceRequest()
	if _, err := s.Subscribe(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	w, _ := fixtureWebhook(t, func(rw http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
		}
	})
	s.webhook = w
	done := make(chan error, 1)
	go func() { done <- s.step(context.Background()) }()
	<-started
	if err := s.Unsubscribe(context.Background(), UnsubscribeRequest{Name: request.Name, Arguments: request.Arguments, Delivery: Delivery{Mode: "webhook", URL: request.Delivery.URL}}); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	records, _ := state.Load()
	if len(records) != 0 {
		t.Fatal("unsubscribe did not durably remove owned record")
	}
	if err := s.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.Unsubscribe(context.Background(), UnsubscribeRequest{Name: request.Name, Arguments: request.Arguments, Delivery: Delivery{Mode: "webhook", URL: request.Delivery.URL}}); err != nil {
		t.Fatal("unsubscribe was not idempotent")
	}
}

func TestEventServiceAuthorityChangesDuringDNS(t *testing.T) {
	s, state, source, _, events := runtimeFixture(t, 204)
	if _, err := s.Subscribe(context.Background(), serviceRequest()); err != nil {
		t.Fatal(err)
	}
	resolve := s.webhook.resolve
	s.webhook.resolve = func(ctx context.Context, network, host string) ([]netip.Addr, error) {
		source.authErr = ErrUnavailable
		return resolve(ctx, network, host)
	}
	if err := s.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	records, _ := state.Load()
	if len(*events) != 0 || records[0].Pending.Attempts != 0 {
		t.Fatal("backend failure during DNS sent callback or consumed callback attempt")
	}
}

func TestEventServiceRotationIsDurableAndExpires(t *testing.T) {
	s, state, _, now, _ := runtimeFixture(t, 204)
	request := serviceRequest()
	first, err := s.Subscribe(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	request.Delivery.Secret = fixtureSecret(9, 32)
	second, err := s.Subscribe(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID {
		t.Fatal("rotation changed subscription identity")
	}
	records, _ := state.Load()
	if len(records[0].PreviousSigningKey) != 32 || !records[0].PreviousKeyExpiresAt.Equal(now.Add(120*time.Second)) {
		t.Fatal("old signing key overlap not durably granted")
	}
	*now = now.Add(120 * time.Second)
	if err = s.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	records, _ = state.Load()
	if len(records[0].PreviousSigningKey) != 0 {
		t.Fatal("expired signing key retained")
	}
}

func TestEventServiceTerminalRefreshAndRetryNeverResurrect(t *testing.T) {
	s, state, _, now, events := runtimeFixture(t, 410)
	request := serviceRequest()
	result, err := s.Subscribe(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = s.Retry(context.Background(), result.ID); err == nil {
		t.Fatal("terminal resync rearmed by explicit retry")
	}
	if _, err = s.Subscribe(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Second)
	if err = s.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	records, _ := state.Load()
	if len(*events) != 1 || records[0].Pending != nil || !records[0].ContinuityLost {
		t.Fatal("routine refresh resurrected terminal occurrence")
	}
}

func TestEventServiceRestartKeepsPendingBodyAndID(t *testing.T) {
	s, state, source, now, events := runtimeFixture(t, 503)
	result, err := s.Subscribe(context.Background(), serviceRequest())
	if err != nil {
		t.Fatal(err)
	}
	if err = s.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	records, _ := state.Load()
	pending := records[0].Pending
	dir := state.dir
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenState(dir, stateBinding())
	if err != nil {
		t.Fatal(err)
	}
	w := s.webhook
	resumed, err := NewService(ServiceOptions{State: reopened, Source: source, Webhook: w, Now: func() time.Time { return *now }})
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	*now = now.Add(time.Second)
	if err = resumed.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(*events) != 2 || (*events)[0].EventID != pending.EventID || (*events)[1].EventID != pending.EventID {
		t.Fatalf("restart changed pending event for %s", result.ID)
	}
	restored, _ := reopened.Load()
	if !bytes.Equal(restored[0].Pending.Envelope, pending.Envelope) {
		t.Fatal("restart changed exact envelope")
	}
}

func TestEventServiceDynamicScopeDropsRemovedIdentifiers(t *testing.T) {
	s, state, source, _, _ := runtimeFixture(t, 204)
	if _, err := s.Subscribe(context.Background(), serviceRequest()); err != nil {
		t.Fatal(err)
	}
	source.authErr = ErrScopeChanged
	source.scope = Scope{Dynamic: true}
	source.positions = Position{}
	if err := s.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	records, _ := state.Load()
	if len(records[0].Scope.Projects) != 0 || len(records[0].Positions) != 0 || records[0].Disabled || !records[0].ContinuityLost || records[0].Pending == nil {
		t.Fatal("empty aggregate scope did not remain active with scoped resync")
	}
	if strings.Contains(string(records[0].Pending.Envelope), stateBinding().ProjectUIDs[0]) {
		t.Fatal("resync leaked removed project identifier")
	}
}

func TestEventServiceIdleSourceDoesNotRewriteState(t *testing.T) {
	s, state, _, _, _ := runtimeFixture(t, 204)
	if _, err := s.Subscribe(context.Background(), serviceRequest()); err != nil {
		t.Fatal(err)
	}
	if err := s.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	write := state.write
	writes := 0
	state.write = func(path string, body []byte) error { writes++; return write(path, body) }
	if err := s.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if writes != 0 {
		t.Fatal("idle source rewrote durable state")
	}
}

func TestEventServiceSourceOutageBacksOff(t *testing.T) {
	s, _, source, now, _ := runtimeFixture(t, 204)
	if _, err := s.Subscribe(context.Background(), serviceRequest()); err != nil {
		t.Fatal(err)
	}
	source.authErr = ErrUnavailable
	if err := s.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	calls := source.authCalls
	if err := s.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if source.authCalls != calls {
		t.Fatal("source outage retried before backoff")
	}
	*now = now.Add(time.Second)
	if err := s.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	calls = source.authCalls
	*now = now.Add(time.Second)
	if err := s.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if source.authCalls != calls {
		t.Fatal("source outage backoff did not increase")
	}
}

func TestEventServiceLimitsConcurrentOutboundRequests(t *testing.T) {
	s, _, _, _, _ := runtimeFixture(t, 204)
	for i := range 16 {
		request := serviceRequest()
		request.Arguments.For = fmt.Sprintf("example-user-%d", i)
		if _, err := s.Subscribe(context.Background(), request); err != nil {
			t.Fatal(err)
		}
	}
	var active, maximum atomic.Int32
	started := make(chan struct{}, 16)
	release := make(chan struct{})
	w, _ := fixtureWebhook(t, func(rw http.ResponseWriter, r *http.Request) {
		n := active.Add(1)
		defer active.Add(-1)
		for old := maximum.Load(); n > old && !maximum.CompareAndSwap(old, n); old = maximum.Load() {
		}
		started <- struct{}{}
		<-release
		rw.WriteHeader(204)
	})
	s.webhook = w
	done := make(chan error, 1)
	go func() { done <- s.step(context.Background()) }()
	for range 8 {
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			close(release)
			t.Fatal("outbound workers did not progress")
		}
	}
	select {
	case <-started:
		close(release)
		t.Fatal("more than eight requests began concurrently")
	default:
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if maximum.Load() > 8 {
		t.Fatal("global outbound cap exceeded")
	}
}

func TestEventServiceRunOutlivesSubscribeContextAndCloses(t *testing.T) {
	s, _, _, _, _ := runtimeFixture(t, 204)
	ctx, cancel := context.WithCancel(context.Background())
	if _, err := s.Subscribe(ctx, serviceRequest()); err != nil {
		t.Fatal(err)
	}
	cancel()
	delivered := make(chan struct{})
	w, _ := fixtureWebhook(t, func(rw http.ResponseWriter, r *http.Request) { close(delivered); rw.WriteHeader(204) })
	s.webhook = w
	done := make(chan error, 1)
	go func() { done <- s.Run(context.Background()) }()
	select {
	case <-delivered:
	case <-time.After(3 * time.Second):
		t.Fatal("subscription died with original request context")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown did not stop worker")
	}
}

func TestEventServiceInvalidSourceVectorCannotAdvance(t *testing.T) {
	s, state, source, now, events := runtimeFixture(t, 204)
	if _, err := s.Subscribe(context.Background(), serviceRequest()); err != nil {
		t.Fatal(err)
	}
	if err := s.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	uid := stateBinding().ProjectUIDs[0]
	source.page = SourcePage{Events: []SourceEvent{{EventID: "evt_invalid", Reason: "notification", Timestamp: *now, Positions: Position{uid: 13}}}, Positions: Position{uid: 11}}
	if err := s.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	records, _ := state.Load()
	if len(*events) != 1 || records[0].Positions[uid] != 12 {
		t.Fatal("inconsistent source page advanced beyond the inspected safe vector")
	}
}

func TestEventServicePersistenceFailureStopsBeforeCallback(t *testing.T) {
	s, state, _, _, events := runtimeFixture(t, 204)
	if _, err := s.Subscribe(context.Background(), serviceRequest()); err != nil {
		t.Fatal(err)
	}
	state.write = func(string, []byte) error { return errors.New("fixture full disk") }
	if err := s.Run(context.Background()); !errors.Is(err, ErrState) {
		t.Fatalf("storage failure did not stop runtime: %v", err)
	}
	if len(*events) != 0 {
		t.Fatal("callback ran after uncertain durable write")
	}
}

func TestEventServiceRefreshAndRetryRearmOnlyTransientPending(t *testing.T) {
	for _, action := range []string{"refresh", "retry"} {
		t.Run(action, func(t *testing.T) {
			s, state, _, now, events := runtimeFixture(t, 503)
			request := serviceRequest()
			initial, err := s.Subscribe(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			for range 8 {
				records, _ := state.Load()
				*now = records[0].Pending.NextAttemptAt
				if err = s.step(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			records, _ := state.Load()
			before := records[0].Pending
			if before == nil || !before.Failed {
				t.Fatal("transient exhaustion was not retained")
			}
			if action == "retry" {
				err = s.Retry(context.Background(), initial.ID)
			} else {
				request.Cursor = &initial.Cursor
				_, err = s.Subscribe(context.Background(), request)
			}
			if err != nil {
				t.Fatal(err)
			}
			records, _ = state.Load()
			after := records[0].Pending
			if after == nil || after.Failed || after.Attempts != 0 || after.EventID != before.EventID || !bytes.Equal(after.Envelope, before.Envelope) || records[0].Positions[stateBinding().ProjectUIDs[0]] != 12 {
				t.Fatal("rearm changed failed occurrence or its safe watermark")
			}
			if err = s.step(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(*events) != 9 {
				t.Fatal("explicit transient rearm did not dispatch retained occurrence")
			}
		})
	}
}

func TestEventServiceConcurrentSubscribeRemainsScheduledAfterFailure(t *testing.T) {
	s, state, _, _, _ := runtimeFixture(t, 204)
	started, release := make(chan struct{}), make(chan struct{})
	var verifications, deliveries atomic.Int32
	w, _ := fixtureWebhook(t, func(rw http.ResponseWriter, r *http.Request) {
		var value struct {
			Type      string `json:"type"`
			Challenge string `json:"challenge"`
		}
		_ = json.NewDecoder(r.Body).Decode(&value)
		if value.Type == "verification" {
			if verifications.Add(1) == 1 {
				close(started)
				<-release
				_, _ = rw.Write([]byte(`{"challenge":"wrong"}`))
				return
			}
			_ = json.NewEncoder(rw).Encode(map[string]string{"challenge": value.Challenge})
			return
		}
		deliveries.Add(1)
		rw.WriteHeader(204)
	})
	s.webhook = w
	first, second := make(chan error, 1), make(chan error, 1)
	go func() { _, err := s.Subscribe(context.Background(), serviceRequest()); first <- err }()
	<-started
	go func() { _, err := s.Subscribe(context.Background(), serviceRequest()); second <- err }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		s.mu.Lock()
		count := 0
		for _, entry := range s.entries {
			count = entry.registrations
		}
		s.mu.Unlock()
		if count == 2 {
			break
		}
		if time.Now().After(deadline) {
			close(release)
			t.Fatal("concurrent registration did not reserve identity")
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	if err := <-first; err == nil {
		t.Fatal("failed initial verification accepted")
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	if err := s.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	records, _ := state.Load()
	if len(records) != 1 || deliveries.Load() != 1 || records[0].Pending != nil {
		t.Fatal("accepted concurrent registration was orphaned from delivery scheduler")
	}
}
