package mcpevents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestEventServiceStatusIsSanitized(t *testing.T) {
	s, _, _, _, _ := serviceFixture(t, 204)
	if _, err := s.Subscribe(context.Background(), serviceRequest()); err != nil {
		t.Fatal(err)
	}
	status, err := s.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(status)
	for _, secret := range []string{"https://", serviceRequest().Delivery.Secret, "example.com", "envelope", "signing_key"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("status leaked private delivery state: %s", raw)
		}
	}
}

func TestEventServiceCatalogAndValidation(t *testing.T) {
	s, _, _, _, _ := serviceFixture(t, 204)
	catalog, err := s.List(context.Background())
	if err != nil || len(catalog.Events) != 1 || catalog.Events[0].Name != "kata.inbox.changed" || !reflect.DeepEqual(catalog.Events[0].Delivery, []string{"webhook"}) {
		t.Fatalf("catalog=%+v error=%v", catalog, err)
	}
	for _, mutate := range []func(*SubscribeRequest){
		func(r *SubscribeRequest) { r.Name = "unknown" }, func(r *SubscribeRequest) { r.Arguments.For = "" }, func(r *SubscribeRequest) { r.Arguments.IssueUID = stateBinding().ProjectUIDs[0] }, func(r *SubscribeRequest) { r.Arguments.ProjectUID = "bad" }, func(r *SubscribeRequest) { r.Delivery.Mode = "stdio" }, func(r *SubscribeRequest) { r.Delivery.Secret = "secret" },
	} {
		request := serviceRequest()
		mutate(&request)
		if _, err := s.Subscribe(context.Background(), request); err == nil {
			t.Fatal("invalid native subscribe contract accepted")
		}
	}
}

func TestEventServiceFailedRefreshPreservesAcceptedRecord(t *testing.T) {
	s, state, _, _, _ := serviceFixture(t, 204)
	request := serviceRequest()
	first, err := s.Subscribe(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := state.Load()
	w, _ := fixtureWebhook(t, func(rw http.ResponseWriter, r *http.Request) { _, _ = rw.Write([]byte(`{"challenge":"wrong"}`)) })
	s.webhook = w
	request.Delivery.Secret = fixtureSecret(9, 32)
	if _, err = s.Subscribe(context.Background(), request); err == nil {
		t.Fatal("failed new-key verification accepted")
	}
	after, _ := state.Load()
	if !reflect.DeepEqual(before, after) || after[0].ID != first.ID {
		t.Fatal("failed refresh modified accepted state")
	}
}

func TestEventServiceRegistrationTTLBounds(t *testing.T) {
	s, _, _, now, _ := serviceFixture(t, 204)
	for _, tc := range []struct {
		ttl  int64
		want time.Duration
	}{{1, time.Minute}, {60000, time.Minute}, {1 << 62, 24 * time.Hour}} {
		request := serviceRequest()
		request.TTLMS = &tc.ttl
		result, err := s.Subscribe(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		expires, _ := time.Parse(time.RFC3339Nano, result.RefreshBefore)
		if !expires.Equal(now.Add(tc.want)) {
			t.Fatalf("ttl=%d expiry=%s", tc.ttl, result.RefreshBefore)
		}
	}
}

func TestEventServiceExpiredCapacityReclaimed(t *testing.T) {
	s, _, _, now, _ := serviceFixture(t, 204)
	ttl := int64(60000)
	for i := range MaxSubscriptions {
		request := serviceRequest()
		request.Arguments.For = fmt.Sprintf("example-user-%d", i)
		request.TTLMS = &ttl
		if _, err := s.Subscribe(context.Background(), request); err != nil {
			t.Fatal(err)
		}
	}
	request := serviceRequest()
	request.Arguments.For = "another-user"
	if _, err := s.Subscribe(context.Background(), request); err == nil {
		t.Fatal("full accepted capacity was silently evicted")
	}
	*now = now.Add(time.Minute)
	if _, err := s.Subscribe(context.Background(), request); err != nil {
		t.Fatalf("expired records permanently exhausted service: %v", err)
	}
}

func TestEventServiceRejectsMismatchedLoadedIdentity(t *testing.T) {
	s, state, source, _, _ := serviceFixture(t, 204)
	if _, err := s.Subscribe(context.Background(), serviceRequest()); err != nil {
		t.Fatal(err)
	}
	records, _ := state.Load()
	sub := records[0]
	if err := state.Delete(sub.ID); err != nil {
		t.Fatal(err)
	}
	sub.ID = strings.Repeat("c", 64)
	if err := state.Save(sub); err != nil {
		t.Fatal(err)
	}
	if _, err := NewService(ServiceOptions{State: state, Source: source, Webhook: s.webhook}); err == nil {
		t.Fatal("mismatched deterministic state identity accepted")
	}
}

func TestEventServiceFailedRegistrationKeepsConcurrentReservation(t *testing.T) {
	s, _, _, _, _ := serviceFixture(t, 204)
	request := serviceRequest()
	id, _, err := s.subscriptionIdentity(request.Name, request.Arguments, request.Delivery)
	if err != nil {
		t.Fatal(err)
	}
	// One caller has acquired its reservation and can be waiting on the same
	// operation lock while a second caller's verification fails.
	reserved, err := s.reserve(id)
	if err != nil {
		t.Fatal(err)
	}
	w, _ := fixtureWebhook(t, func(rw http.ResponseWriter, r *http.Request) { _, _ = rw.Write([]byte(`{"challenge":"wrong"}`)) })
	s.webhook = w
	if _, err = s.Subscribe(context.Background(), request); err == nil {
		t.Fatal("invalid challenge accepted")
	}
	s.mu.Lock()
	retained := s.entries[id]
	s.mu.Unlock()
	if retained != reserved {
		t.Fatal("failed verification orphaned another in-flight registration")
	}
}

func TestEventServicePreCanceledUnsubscribeKeepsSubscriptionUsable(t *testing.T) {
	s, state, _, _, _ := serviceFixture(t, http.StatusNoContent)
	request := serviceRequest()
	if _, err := s.Subscribe(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := s.Unsubscribe(ctx, UnsubscribeRequest{Name: request.Name, Arguments: request.Arguments, Delivery: request.Delivery})
	var protocolError *Error
	if !errors.As(err, &protocolError) || protocolError.Code != -32015 || protocolError.Reason != "events_unavailable" {
		t.Errorf("pre-canceled unsubscribe did not return a sanitized protocol error: %v", err)
	}
	records, err := state.Load()
	if err != nil || len(records) != 1 {
		t.Fatalf("pre-canceled request changed durable registration: records=%d error=%v", len(records), err)
	}
	status, err := s.Status(context.Background())
	if err != nil || len(status) != 1 {
		t.Errorf("pre-canceled request hid a durable subscription: status=%d error=%v", len(status), err)
	}
	if _, err := s.Subscribe(context.Background(), request); err != nil {
		t.Errorf("pre-canceled request prevented subscription refresh: %v", err)
	}
}

func TestEventServiceCanceledWaitingUnsubscribeCompletesDurableDeletion(t *testing.T) {
	s, state, _, _, _ := serviceFixture(t, http.StatusNoContent)
	request := serviceRequest()
	registered, err := s.Subscribe(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	entry := s.entries[registered.ID]
	s.mu.Unlock()
	entry.op.Lock()
	locked := true
	defer func() {
		if locked {
			entry.op.Unlock()
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- s.Unsubscribe(ctx, UnsubscribeRequest{Name: request.Name, Arguments: request.Arguments, Delivery: request.Delivery})
	}()
	deadline := time.Now().Add(3 * time.Second)
	for !entry.stopping.Load() {
		if time.Now().After(deadline) {
			t.Fatal("unsubscribe did not fence its in-flight operation")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	entry.op.Unlock()
	locked = false
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("unsubscribe abandoned a fenced operation after cancellation: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("unsubscribe did not finish after its in-flight operation stopped")
	}
	records, err := state.Load()
	if err != nil || len(records) != 0 {
		t.Errorf("cancellation left a permanently fenced durable record: records=%d error=%v", len(records), err)
	}
	if _, err := s.Subscribe(context.Background(), request); err != nil {
		t.Errorf("cancellation prevented subsequent re-registration: %v", err)
	}
}
