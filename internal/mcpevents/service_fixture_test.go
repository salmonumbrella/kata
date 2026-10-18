package mcpevents

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"
)

type fixtureSource struct {
	mu        sync.Mutex
	scope     Scope
	positions Position
	page      SourcePage
	authErr   error
	authCalls int
}

func (f *fixtureSource) Resolve(_ context.Context, args Arguments) (Scope, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.scope
	s.Arguments = args
	return s, nil
}
func (f *fixtureSource) Authorize(context.Context, Scope) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.authCalls++
	return f.authErr
}
func (f *fixtureSource) Head(context.Context, Scope) (Position, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return clonePosition(f.positions), nil
}
func (f *fixtureSource) Read(_ context.Context, _ Scope, p Position, _ int) (SourcePage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.page.Reset {
		return f.page, nil
	}
	if len(f.page.Events) > 0 && f.page.Events[0].Positions[stateBinding().ProjectUIDs[0]] > p[stateBinding().ProjectUIDs[0]] {
		return f.page, nil
	}
	return SourcePage{Positions: clonePosition(p)}, nil
}

func serviceRequest() SubscribeRequest {
	return SubscribeRequest{Name: "kata.inbox.changed", Arguments: Arguments{For: "example-user"}, Delivery: Delivery{Mode: "webhook", URL: fixtureTLSCallback, Secret: fixtureSecret(7, 32)}}
}

func serviceFixture(t *testing.T, status int) (*Service, *State, *fixtureSource, *time.Time, *[]Envelope) {
	t.Helper()
	state, _ := openTestState(t)
	now := time.Unix(1790000000, 0).UTC()
	source := &fixtureSource{scope: Scope{Projects: []Project{{UID: stateBinding().ProjectUIDs[0], ID: 1, Name: "example-project"}}, Dynamic: true}, positions: Position{stateBinding().ProjectUIDs[0]: 12}}
	var events []Envelope
	w, _ := fixtureWebhook(t, func(rw http.ResponseWriter, r *http.Request) {
		var raw json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&raw)
		var challenge struct {
			Type      string `json:"type"`
			Challenge string `json:"challenge"`
		}
		_ = json.Unmarshal(raw, &challenge)
		if challenge.Type == "verification" {
			_ = json.NewEncoder(rw).Encode(map[string]string{"challenge": challenge.Challenge})
			return
		}
		var event Envelope
		if err := json.Unmarshal(raw, &event); err != nil {
			t.Error(err)
		}
		events = append(events, event)
		rw.WriteHeader(status)
	})
	w.now = func() time.Time { return now }
	service, err := NewService(ServiceOptions{State: state, Source: source, Webhook: w, Now: func() time.Time { return now }, PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return service, state, source, &now, &events
}
