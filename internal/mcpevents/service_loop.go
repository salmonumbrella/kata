package mcpevents

import (
	"context"
	"errors"
	"sync"
	"time"
)

func (s *Service) process(ctx context.Context, entry *subscriptionEntry) error {
	entry.op.Lock()
	defer entry.op.Unlock()
	if entry.stopping.Load() || entry.sub.ID == "" || entry.sub.Disabled || !s.now().Before(entry.sub.ExpiresAt) || s.now().Before(entry.nextSourceAt) {
		return nil
	}
	ctx, done, err := s.operation(ctx, entry)
	if err != nil {
		return nil
	}
	defer done()
	sub := cloneSubscription(entry.sub)
	if err = s.source.Authorize(ctx, sub.Scope); err != nil {
		if errors.Is(err, ErrScopeChanged) {
			return s.reconcile(ctx, entry)
		}
		if errors.Is(err, ErrResourceLimit) {
			return s.pauseSource(ctx, entry, "resource_limit")
		}
		if errors.Is(err, ErrRevoked) {
			err = s.sourceError(ctx, entry, err)
			if errors.Is(err, ErrState) {
				return err
			}
			return nil
		}
		return s.pauseSource(ctx, entry, "source_unavailable")
	}
	if len(sub.PreviousSigningKey) != 0 && !s.now().Before(sub.PreviousKeyExpiresAt) {
		sub.PreviousSigningKey = nil
		sub.PreviousKeyExpiresAt = time.Time{}
		if err = s.save(ctx, entry, sub); err != nil {
			return err
		}
	}
	if sub.Pending == nil {
		page, err := s.source.Read(ctx, sub.Scope, clonePosition(sub.Positions), 100)
		if err != nil {
			if errors.Is(err, ErrRevoked) {
				return s.sourceError(ctx, entry, err)
			}
			if errors.Is(err, ErrScopeChanged) {
				return s.reconcile(ctx, entry)
			}
			if errors.Is(err, ErrResourceLimit) {
				return s.pauseSource(ctx, entry, "resource_limit")
			}
			return s.pauseSource(ctx, entry, "source_unavailable")
		}
		if !validSourcePage(sub, page) {
			return s.pauseSource(ctx, entry, "source_unavailable")
		}
		if page.Reset {
			sub.Positions = clonePosition(page.Positions)
			sub, err = prepareResync(s.identity, sub, s.now(), true)
		} else if len(page.Events) > 0 {
			sub, err = preparePending(s.identity, sub, page.Events[0], s.now())
		} else if !equalPositions(sub.Positions, page.Positions) {
			sub.Positions = clonePosition(page.Positions)
		} else {
			entry.sourceFailures = 0
			entry.nextSourceAt = time.Time{}
			return nil
		}
		if err != nil {
			return err
		}
		if err = s.save(ctx, entry, sub); err != nil {
			return err
		}
	}
	if sub.Pending == nil || sub.Pending.Failed || s.now().Before(sub.Pending.NextAttemptAt) {
		return nil
	}
	select {
	case s.outbound <- struct{}{}:
	case <-ctx.Done():
		return nil
	}
	defer func() { <-s.outbound }()
	// Authorization is checked again after waiting for an outbound slot.
	if err = s.source.Authorize(ctx, sub.Scope); err != nil {
		if errors.Is(err, ErrRevoked) {
			return s.sourceError(ctx, entry, err)
		}
		if errors.Is(err, ErrScopeChanged) {
			return s.reconcile(ctx, entry)
		}
		return s.pauseSource(ctx, entry, "source_unavailable")
	}
	guardedCtx := withWebhookGuard(ctx, func(guardCtx context.Context) error { return s.webhookAuthority(guardCtx, sub.Scope) })
	_, err = attemptDelivery(guardedCtx, s.identity, sub, s.webhook, s.now(), func(next Subscription) error { return s.save(ctx, entry, next) })
	if errors.Is(err, context.Canceled) {
		return nil
	}
	if err == nil && entry.sub.Status.Category == "source_unavailable" {
		return s.pauseSource(ctx, entry, "source_unavailable")
	}
	if err == nil {
		entry.sourceFailures = 0
		entry.nextSourceAt = time.Time{}
	}
	return err
}

func equalPositions(a, b Position) bool {
	if len(a) != len(b) {
		return false
	}
	for uid, n := range a {
		if other, ok := b[uid]; !ok || n != other {
			return false
		}
	}
	return true
}

func validSourcePage(sub Subscription, page SourcePage) bool {
	if !scopePositions(sub.Scope, page.Positions) || len(page.Events) > 100 {
		return false
	}
	if page.Reset {
		return len(page.Events) == 0
	}
	for uid, n := range sub.Positions {
		if page.Positions[uid] < n {
			return false
		}
	}
	if len(page.Events) != 0 {
		position := page.Events[0].Positions
		if !scopePositions(sub.Scope, position) {
			return false
		}
		for uid, n := range position {
			if n < sub.Positions[uid] || n > page.Positions[uid] {
				return false
			}
		}
	}
	return true
}

func (s *Service) pauseSource(ctx context.Context, entry *subscriptionEntry, category string) error {
	entry.sourceFailures++
	shift := entry.sourceFailures - 1
	if shift > 5 {
		shift = 5
	}
	delay := time.Second << uint(shift)
	if delay > 30*time.Second {
		delay = 30 * time.Second
	}
	entry.nextSourceAt = s.now().Add(delay)
	sub := cloneSubscription(entry.sub)
	sub.Status.Category = category
	sub.Status.UpdatedAt = s.now()
	return s.save(ctx, entry, sub)
}

func (s *Service) reconcile(ctx context.Context, entry *subscriptionEntry) error {
	sub := cloneSubscription(entry.sub)
	scope, err := s.source.Resolve(ctx, sub.Arguments)
	if err != nil {
		if errors.Is(err, ErrRevoked) {
			return s.sourceError(ctx, entry, err)
		}
		if errors.Is(err, ErrResourceLimit) {
			return s.pauseSource(ctx, entry, "resource_limit")
		}
		return s.pauseSource(ctx, entry, "source_unavailable")
	}
	if len(scope.Projects) > MaxProjects {
		sub.Status.Category = "resource_limit"
		sub.Status.UpdatedAt = s.now()
		return s.save(ctx, entry, sub)
	}
	if !sub.Scope.Dynamic {
		return s.sourceError(ctx, entry, ErrRevoked)
	}
	head, err := s.source.Head(ctx, scope)
	if err != nil && errors.Is(err, ErrRevoked) {
		return s.sourceError(ctx, entry, err)
	}
	if err != nil || !scopePositions(scope, head) {
		return s.pauseSource(ctx, entry, "source_unavailable")
	}
	positions := make(Position, len(scope.Projects))
	for _, project := range scope.Projects {
		if n, ok := sub.Positions[project.UID]; ok {
			positions[project.UID] = n
		} else {
			positions[project.UID] = head[project.UID]
		}
	}
	scope.Arguments = sub.Arguments
	sub.Scope = scope
	sub.Positions = positions
	sub.Pending = nil
	sub.Generation++
	sub, err = prepareResync(s.identity, sub, s.now(), true)
	if err != nil {
		return err
	}
	return s.save(ctx, entry, sub)
}

func (s *Service) step(ctx context.Context) error {
	if err := s.usable(); err != nil {
		return err
	}
	var wg sync.WaitGroup
	errorsC := make(chan error, MaxSubscriptions)
	for _, entry := range s.snapshot() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.process(ctx, entry); err != nil {
				errorsC <- err
			}
		}()
	}
	wg.Wait()
	close(errorsC)
	for err := range errorsC {
		if errors.Is(err, ErrState) {
			return s.fail(err)
		}
	}
	return nil
}

func (s *Service) Run(ctx context.Context) error {
	s.mu.Lock()
	if s.closed || s.running {
		s.mu.Unlock()
		return &Error{Code: -32013, Reason: "events_already_running"}
	}
	s.running = true
	s.runDone = make(chan struct{})
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.running = false; close(s.runDone); s.mu.Unlock() }()
	ticker := time.NewTicker(s.poll)
	defer ticker.Stop()
	for {
		if err := s.step(ctx); err != nil {
			s.mu.Lock()
			closed, failed := s.closed, s.failure
			s.mu.Unlock()
			if closed && failed == nil {
				return nil
			}
			return err
		}
		select {
		case <-ctx.Done():
			s.mu.Lock()
			failure := s.failure
			s.mu.Unlock()
			return failure
		case <-s.lifetime.Done():
			s.mu.Lock()
			failure := s.failure
			s.mu.Unlock()
			return failure
		case <-ticker.C:
		}
	}
}

func (s *Service) Close() error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		done := s.runDone
		s.mu.Unlock()
		s.cancel()
		for _, entry := range s.snapshot() {
			entry.stop()
			entry.op.Lock()
			entry.op.Unlock()
		}
		if done != nil {
			<-done
		}
		s.closeErr = s.state.Close()
	})
	return s.closeErr
}
