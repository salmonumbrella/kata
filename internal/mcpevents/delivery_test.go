package mcpevents

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"testing"
	"time"
)

func deliverySubscription() Subscription {
	s := stateSubscription()
	s.CallbackURL = fixtureTLSCallback
	s.Pending.CreatedAt = time.Unix(1790000000, 0).UTC()
	s.Pending.NextAttemptAt = s.Pending.CreatedAt
	return s
}

func TestDeliveryPersistBeforeSend(t *testing.T) {
	state, _ := openTestState(t)
	sub := deliverySubscription()
	if err := state.Save(sub); err != nil {
		t.Fatal(err)
	}
	var received int
	w, _ := fixtureWebhook(t, func(rw http.ResponseWriter, r *http.Request) {
		records, err := state.Load()
		if err != nil || len(records) != 1 || records[0].Pending == nil || records[0].Pending.Attempts != 1 || !bytes.Equal(records[0].Pending.Envelope, sub.Pending.Envelope) || !reflect.DeepEqual(records[0].Positions, sub.Positions) {
			t.Errorf("callback preceded durable pending/attempt: %+v %v", records, err)
		}
		received++
		rw.WriteHeader(202)
	})
	result, err := attemptDelivery(context.Background(), state.Identity(), sub, w, sub.Pending.CreatedAt, state.Save)
	if err != nil || result.Pending != nil || !reflect.DeepEqual(result.Positions, sub.Pending.NextPositions) || received != 1 {
		t.Fatalf("result=%+v error=%v received=%d", result, err, received)
	}
	stored, _ := state.Load()
	if stored[0].Pending != nil || !reflect.DeepEqual(stored[0].Positions, sub.Pending.NextPositions) {
		t.Fatal("receipt was not durably acknowledged")
	}
}

func TestDeliveryAcceptedBeforeCrashRetriesSameID(t *testing.T) {
	state, _ := openTestState(t)
	sub := deliverySubscription()
	if err := state.Save(sub); err != nil {
		t.Fatal(err)
	}
	var bodies [][]byte
	var ids []string
	w, _ := fixtureWebhook(t, func(rw http.ResponseWriter, r *http.Request) {
		var body bytes.Buffer
		_, _ = body.ReadFrom(r.Body)
		bodies = append(bodies, append([]byte(nil), body.Bytes()...))
		ids = append(ids, r.Header.Get("Webhook-Id"))
		rw.WriteHeader(204)
	})
	writes := 0
	_, err := attemptDelivery(context.Background(), state.Identity(), sub, w, sub.Pending.CreatedAt, func(s Subscription) error {
		writes++
		if writes == 2 {
			return ErrState
		}
		return state.Save(s)
	})
	if !errors.Is(err, ErrState) {
		t.Fatalf("post-receipt persistence failure ignored: %v", err)
	}
	restored, _ := state.Load()
	if restored[0].Pending == nil || !reflect.DeepEqual(restored[0].Positions, sub.Positions) {
		t.Fatal("crash moved safe watermark")
	}
	result, err := attemptDelivery(context.Background(), state.Identity(), restored[0], w, sub.Pending.CreatedAt.Add(time.Second), state.Save)
	if err != nil || result.Pending != nil || len(ids) != 2 || ids[0] != ids[1] || !bytes.Equal(bodies[0], bodies[1]) {
		t.Fatalf("restart changed pending envelope/ID: %v", err)
	}
}

func TestDeliveryPersistenceFailureSendsNothing(t *testing.T) {
	state, _ := openTestState(t)
	sub := deliverySubscription()
	var received int
	w, _ := fixtureWebhook(t, func(rw http.ResponseWriter, r *http.Request) { received++; rw.WriteHeader(204) })
	_, err := attemptDelivery(context.Background(), state.Identity(), sub, w, sub.Pending.CreatedAt, func(Subscription) error { return ErrState })
	if !errors.Is(err, ErrState) || received != 0 {
		t.Fatalf("unpersisted attempt was sent: received=%d error=%v", received, err)
	}
}

func TestDeliveryTransientExhaustionFreezesWatermark(t *testing.T) {
	state, _ := openTestState(t)
	sub := deliverySubscription()
	var received int
	w, _ := fixtureWebhook(t, func(rw http.ResponseWriter, r *http.Request) { received++; rw.WriteHeader(503) })
	if err := state.Save(sub); err != nil {
		t.Fatal(err)
	}
	for range 8 {
		now := sub.Pending.NextAttemptAt
		var err error
		sub, err = attemptDelivery(context.Background(), state.Identity(), sub, w, now, state.Save)
		if err != nil {
			t.Fatal(err)
		}
	}
	if sub.Pending == nil || !sub.Pending.Failed || sub.Pending.Attempts != 8 || sub.Positions[stateBinding().ProjectUIDs[0]] != 12 || received != 8 {
		t.Fatalf("transient failure lost pending/safe watermark: %+v", sub)
	}
	if _, err := attemptDelivery(context.Background(), state.Identity(), sub, w, sub.Pending.NextAttemptAt.Add(time.Hour), state.Save); err != nil {
		t.Fatal(err)
	}
	if received != 8 {
		t.Fatal("exhausted pending was retried without explicit rearm")
	}
}

func TestDelivery410And413AbandonOnceWithGap(t *testing.T) {
	for _, status := range []int{410, 413} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			state, _ := openTestState(t)
			sub := deliverySubscription()
			sub.Pending.Envelope = []byte(`{"eventId":"occurrence-1","name":"kata.inbox.changed","timestamp":"2026-10-01T00:00:00Z","data":{"recipient":"example-user","reason":"notification","resync_required":false},"cursor":"old"}`)
			var received int
			w, _ := fixtureWebhook(t, func(rw http.ResponseWriter, r *http.Request) { received++; rw.WriteHeader(status) })
			var err error
			sub, err = attemptDelivery(context.Background(), state.Identity(), sub, w, sub.Pending.CreatedAt, state.Save)
			if err != nil || !sub.ContinuityLost || sub.ContinuityGeneration != 1 || sub.Pending == nil || sub.Positions[stateBinding().ProjectUIDs[0]] != 13 {
				t.Fatalf("terminal occurrence not abandoned with durable gap: %+v %v", sub, err)
			}
			var gap Envelope
			if json.Unmarshal(sub.Pending.Envelope, &gap) != nil || gap.Data.Reason != "resync" || !gap.Data.ResyncRequired {
				t.Fatalf("missing application resync: %s", sub.Pending.Envelope)
			}
			gapID := sub.Pending.EventID
			sub, err = attemptDelivery(context.Background(), state.Identity(), sub, w, sub.Pending.NextAttemptAt, state.Save)
			if err != nil || sub.Pending != nil || !sub.ContinuityLost || sub.Status.LastFailureEventID != gapID {
				t.Fatalf("terminal resync recursively regenerated or diagnostics lost: %+v %v", sub, err)
			}
			if _, err := attemptDelivery(context.Background(), state.Identity(), sub, w, time.Now(), state.Save); err != nil {
				t.Fatal(err)
			}
			if received != 2 {
				t.Fatal("terminal gap was retried")
			}
		})
	}
}

func TestDeliveryBackoffAndElapsedBound(t *testing.T) {
	state, _ := openTestState(t)
	sub := deliverySubscription()
	var received int
	w, _ := fixtureWebhook(t, func(rw http.ResponseWriter, r *http.Request) {
		received++
		rw.Header().Set("Retry-After", "300")
		rw.WriteHeader(429)
	})
	start := sub.Pending.CreatedAt
	var err error
	for range 3 {
		sub, err = attemptDelivery(context.Background(), state.Identity(), sub, w, sub.Pending.NextAttemptAt, state.Save)
		if err != nil {
			t.Fatal(err)
		}
	}
	if sub.Pending.NextAttemptAt.After(start.Add(15 * time.Minute)) {
		t.Fatal("retry escaped total elapsed bound")
	}
	sub, err = attemptDelivery(context.Background(), state.Identity(), sub, w, start.Add(15*time.Minute), state.Save)
	if err != nil || !sub.Pending.Failed || received != 3 {
		t.Fatalf("elapsed exhaustion did not freeze pending: %+v %v received=%d", sub, err, received)
	}
}
