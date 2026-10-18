package mcpevents

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/oklog/ulid/v2"
	"go.kenn.io/kata/internal/notification"
	"sync"
	"sync/atomic"
	"time"
)

type ServiceOptions struct {
	State        *State
	Source       Source
	Webhook      *WebhookClient
	Now          func() time.Time
	PollInterval time.Duration
}

var errSubscriptionExpired = errors.New("event subscription expired")

type subscriptionEntry struct {
	op             sync.Mutex
	cancelMu       sync.Mutex
	cancel         context.CancelFunc
	stopping       atomic.Bool
	sub            Subscription
	sourceFailures int
	nextSourceAt   time.Time
	registrations  int // guarded by Service.mu, including callers waiting on op
}

func (e *subscriptionEntry) stop() {
	e.stopping.Store(true)
	e.cancelMu.Lock()
	if e.cancel != nil {
		e.cancel()
	}
	e.cancelMu.Unlock()
}

// Service owns subscriptions independently of transient MCP sessions. Each
// subscription has one operation lock, while other subscriptions can progress.
type Service struct {
	mu        sync.Mutex
	state     *State
	identity  Identity
	source    Source
	webhook   *WebhookClient
	now       func() time.Time
	poll      time.Duration
	entries   map[string]*subscriptionEntry
	verified  map[string]time.Time
	outbound  chan struct{}
	lifetime  context.Context
	cancel    context.CancelFunc
	closed    bool
	failure   error
	running   bool
	runDone   chan struct{}
	closeOnce sync.Once
	closeErr  error
}

func NewService(options ServiceOptions) (*Service, error) {
	if options.State == nil || options.Source == nil || options.Webhook == nil {
		return nil, ErrState
	}
	records, err := options.State.Load()
	if err != nil {
		return nil, err
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.PollInterval <= 0 {
		options.PollInterval = time.Second
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Service{state: options.State, identity: options.State.Identity(), source: options.Source, webhook: options.Webhook, now: options.Now, poll: options.PollInterval, entries: make(map[string]*subscriptionEntry), verified: make(map[string]time.Time), outbound: make(chan struct{}, 8), lifetime: ctx, cancel: cancel}
	for _, sub := range records {
		expected, args, identityErr := s.subscriptionIdentity(sub.Event, sub.Arguments, Delivery{Mode: "webhook", URL: sub.CallbackURL})
		if identityErr != nil || expected != sub.ID || args != sub.Arguments || sub.Scope.Arguments != sub.Arguments || !scopePositions(sub.Scope, sub.Positions) {
			cancel()
			return nil, ErrState
		}
		s.entries[sub.ID] = &subscriptionEntry{sub: sub}
	}
	return s, nil
}

func (s *Service) usable() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failure != nil {
		return s.failure
	}
	if s.closed {
		return ErrState
	}
	return nil
}

func (s *Service) fail(err error) error {
	if !errors.Is(err, ErrState) {
		return err
	}
	s.mu.Lock()
	s.failure = ErrState
	s.mu.Unlock()
	s.cancel()
	return ErrState
}

func (s *Service) operation(ctx context.Context, e *subscriptionEntry) (context.Context, func(), error) {
	if err := s.usable(); err != nil {
		return nil, nil, err
	}
	if e.stopping.Load() {
		return nil, nil, context.Canceled
	}
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.lifetime, cancel)
	e.cancelMu.Lock()
	e.cancel = cancel
	if e.stopping.Load() {
		cancel()
	}
	e.cancelMu.Unlock()
	return ctx, func() { stop(); cancel(); e.cancelMu.Lock(); e.cancel = nil; e.cancelMu.Unlock() }, nil
}

func eventInvalid(reason string) error { return &Error{Code: -32602, Reason: reason} }

func normalizeArguments(args Arguments) (Arguments, error) {
	forValue, err := notification.NormalizeRecipient(args.For)
	if err != nil {
		return args, eventInvalid("invalid_arguments")
	}
	args.For = forValue
	if args.IssueUID != "" && args.ProjectUID == "" {
		return args, eventInvalid("invalid_scope")
	}
	for _, field := range []*string{&args.ProjectUID, &args.IssueUID} {
		if *field != "" {
			uid, err := ulid.ParseStrict(*field)
			if err != nil {
				return args, eventInvalid("invalid_identifier")
			}
			*field = uid.String()
		}
	}
	return args, nil
}

func (s *Service) subscriptionIdentity(name string, args Arguments, delivery Delivery) (string, Arguments, error) {
	if name != "kata.inbox.changed" {
		return "", args, eventInvalid("unknown_event")
	}
	if delivery.Mode != "webhook" {
		return "", args, &Error{Code: -32014, Reason: "unsupported_delivery"}
	}
	var err error
	args, err = normalizeArguments(args)
	if err != nil {
		return "", args, err
	}
	if err = ValidateWebhookCallback(delivery.URL); err != nil {
		return "", args, eventInvalid("invalid_callback")
	}
	b, _ := json.Marshal(struct {
		Service, Name string
		Arguments     Arguments
		URL           string
	}{s.identity.ID, name, args, delivery.URL})
	hash := sha256.Sum256(b)
	return hex.EncodeToString(hash[:]), args, nil
}

func (s *Service) reserve(id string) (*subscriptionEntry, error) {
	s.mu.Lock()
	if s.closed || s.failure != nil {
		s.mu.Unlock()
		return nil, ErrState
	}
	if entry := s.entries[id]; entry != nil {
		entry.registrations++
		s.mu.Unlock()
		return entry, nil
	}
	if len(s.entries) >= MaxSubscriptions {
		s.mu.Unlock()
		if err := s.reapExpired(); err != nil {
			return nil, err
		}
		s.mu.Lock()
		if s.closed || s.failure != nil {
			s.mu.Unlock()
			return nil, ErrState
		}
		if entry := s.entries[id]; entry != nil {
			entry.registrations++
			s.mu.Unlock()
			return entry, nil
		}
		if len(s.entries) >= MaxSubscriptions {
			s.mu.Unlock()
			return nil, &Error{Code: -32013, Reason: "subscription_limit"}
		}
	}
	entry := &subscriptionEntry{registrations: 1}
	s.entries[id] = entry
	s.mu.Unlock()
	return entry, nil
}

func (s *Service) reapExpired() error {
	for _, entry := range s.snapshot() {
		if !entry.op.TryLock() {
			continue
		}
		s.mu.Lock()
		reserved := entry.registrations != 0
		s.mu.Unlock()
		if reserved {
			entry.op.Unlock()
			continue
		}
		if entry.sub.ID != "" && !s.now().Before(entry.sub.ExpiresAt) {
			entry.stop()
			if err := s.state.Delete(entry.sub.ID); err != nil {
				entry.op.Unlock()
				return s.fail(err)
			}
			s.mu.Lock()
			if s.entries[entry.sub.ID] == entry {
				delete(s.entries, entry.sub.ID)
			}
			s.mu.Unlock()
		}
		entry.op.Unlock()
	}
	return nil
}

func (s *Service) save(ctx context.Context, e *subscriptionEntry, sub Subscription) error {
	if ctx.Err() != nil || e.stopping.Load() {
		return context.Canceled
	}
	if err := s.state.Save(sub); err != nil {
		return s.fail(err)
	}
	e.sub = sub
	return nil
}

func (s *Service) verify(ctx context.Context, callback, id, secret string) error {
	fingerprint := sha256.Sum256([]byte(callback + "\x00" + secret))
	key := hex.EncodeToString(fingerprint[:])
	s.mu.Lock()
	validUntil := s.verified[key]
	s.mu.Unlock()
	if s.now().Before(validUntil) {
		return nil
	}
	select {
	case s.outbound <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	err := s.webhook.Verify(ctx, callback, id, secret)
	<-s.outbound
	if err != nil {
		return &Error{Code: -32015, Reason: safeWebhookError(err).Reason}
	}
	s.mu.Lock()
	for key, expires := range s.verified {
		if !s.now().Before(expires) {
			delete(s.verified, key)
		}
	}
	if len(s.verified) >= 2*MaxSubscriptions {
		var oldestKey string
		var oldest time.Time
		for key, expires := range s.verified {
			if oldestKey == "" || expires.Before(oldest) {
				oldestKey = key
				oldest = expires
			}
		}
		delete(s.verified, oldestKey)
	}
	s.verified[key] = s.now().Add(5 * time.Minute)
	s.mu.Unlock()
	return nil
}

func scopePositions(scope Scope, position Position) bool {
	if len(scope.Projects) > MaxProjects || position == nil || validatePosition(position) != nil {
		return false
	}
	allowed := make(map[string]bool, len(scope.Projects))
	for _, project := range scope.Projects {
		if allowed[project.UID] {
			return false
		}
		allowed[project.UID] = true
	}
	for uid := range position {
		if !allowed[uid] {
			return false
		}
	}
	for uid := range allowed {
		if _, ok := position[uid]; !ok {
			return false
		}
	}
	return true
}

func cursorBehind(cursor Cursor, sub Subscription) bool {
	if cursor.Generation > sub.ContinuityGeneration {
		return false
	}
	for uid, n := range cursor.Positions {
		if safe, ok := sub.Positions[uid]; ok {
			if n > safe {
				return false
			}
		} else if cursor.Generation >= sub.ContinuityGeneration {
			return false
		}
	}
	return true
}

func (s *Service) result(sub Subscription) (SubscribeResult, error) {
	cursor, err := EncodeCursor(s.identity, sub.Event, sub.Arguments, sub.ContinuityGeneration, sub.Positions)
	if err != nil {
		return SubscribeResult{}, err
	}
	return SubscribeResult{ID: sub.ID, RefreshBefore: sub.RefreshBefore.UTC().Format(time.RFC3339Nano), Cursor: cursor, Truncated: sub.ContinuityLost}, nil
}

func (s *Service) snapshot() []*subscriptionEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries := make([]*subscriptionEntry, 0, len(s.entries))
	for _, entry := range s.entries {
		entries = append(entries, entry)
	}
	return entries
}

func (s *Service) webhookAuthority(ctx context.Context, scope Scope) error {
	if err := s.usable(); err != nil {
		return err
	}
	err := s.source.Authorize(ctx, scope)
	if err == nil || errors.Is(err, ErrRevoked) || errors.Is(err, ErrScopeChanged) {
		return err
	}
	return ErrUnavailable
}

func (s *Service) webhookOccurrenceAuthorityBefore(ctx context.Context, scope Scope, payload Payload, expiresAt time.Time) error {
	if !s.now().Before(expiresAt) {
		return ErrUnavailable
	}
	if err := s.usable(); err != nil {
		return err
	}
	err := s.source.AuthorizeOccurrence(ctx, scope, payload)
	if !s.now().Before(expiresAt) {
		return ErrUnavailable
	}
	switch {
	case err == nil || errors.Is(err, ErrRevoked):
		return err
	case errors.Is(err, ErrOccurrenceHidden):
		return &WebhookError{Reason: "occurrence_hidden", cause: ErrOccurrenceHidden}
	case errors.Is(err, ErrScopeChanged):
		return &WebhookError{Reason: "source_unavailable", cause: ErrScopeChanged}
	case errors.Is(err, ErrResourceLimit):
		return &WebhookError{Reason: "resource_limit", cause: ErrResourceLimit}
	default:
		return ErrUnavailable
	}
}
