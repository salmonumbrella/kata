package mcpevents

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const (
	maxDeliveryAttempts = 8
	maxDeliveryElapsed  = 15 * time.Minute
)

func clonePosition(position Position) Position {
	result := make(Position, len(position))
	for uid, n := range position {
		result[uid] = n
	}
	return result
}

func cloneSubscription(sub Subscription) Subscription {
	sub.Positions = clonePosition(sub.Positions)
	sub.Scope.Projects = append([]Project(nil), sub.Scope.Projects...)
	if sub.Pending != nil {
		pending := *sub.Pending
		pending.NextPositions = clonePosition(pending.NextPositions)
		sub.Pending = &pending
	}
	return sub
}

func preparePending(identity Identity, sub Subscription, event SourceEvent, now time.Time) (Subscription, error) {
	sub = cloneSubscription(sub)
	cursor, err := EncodeCursor(identity, sub.Event, sub.Arguments, sub.ContinuityGeneration, sub.Positions)
	if err != nil {
		return sub, err
	}
	body, err := json.Marshal(Envelope{
		EventID: event.EventID, Name: sub.Event, Timestamp: event.Timestamp.UTC().Format(time.RFC3339Nano),
		Data:   Payload{Recipient: sub.Arguments.For, Reason: event.Reason, ProjectUID: event.ProjectUID, IssueUID: event.IssueUID, SourceEventUID: event.SourceEventUID, ResyncRequired: sub.ContinuityLost || event.Reason == "resync"},
		Cursor: cursor,
	})
	if err != nil || len(body) > MaxEnvelopeBytes {
		return sub, ErrState
	}
	sub.Pending = &Pending{EventID: event.EventID, Envelope: body, NextPositions: clonePosition(event.Positions), CreatedAt: now, NextAttemptAt: now}
	return sub, nil
}

func prepareResync(identity Identity, sub Subscription, now time.Time, lost bool) (Subscription, error) {
	if lost {
		sub.ContinuityGeneration++
		sub.ContinuityLost = true
	}
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return sub, ErrState
	}
	return preparePending(identity, sub, SourceEvent{EventID: "evt_resync_" + hex.EncodeToString(random[:]), Reason: "resync", Timestamp: now, Positions: sub.Positions}, now)
}

// attemptDelivery requires the subscription operation lock. The persist callback
// must generation-check and durably replace the entire record before returning.
// No callback is sent when the attempt record cannot be committed.
func attemptDelivery(ctx context.Context, identity Identity, current Subscription, webhook *WebhookClient, now time.Time, persist func(Subscription) error) (Subscription, error) {
	sub := cloneSubscription(current)
	pending := sub.Pending
	if pending == nil || pending.Failed || now.Before(pending.NextAttemptAt) {
		return sub, nil
	}
	if pending.Attempts >= maxDeliveryAttempts || !now.Before(pending.CreatedAt.Add(maxDeliveryElapsed)) {
		pending.Failed = true
		sub.Status.Category = "retry_exhausted"
		sub.Status.UpdatedAt = now
		return sub, persist(sub)
	}
	pending.Attempts++
	sub.Status.EventID = pending.EventID
	sub.Status.Attempts = pending.Attempts
	sub.Status.UpdatedAt = now
	sub.Status.Category = "sending"
	if err := persist(sub); err != nil {
		return current, err
	}
	keys := []string{"whsec_" + base64.StdEncoding.EncodeToString(sub.SigningKey)}
	if len(sub.PreviousSigningKey) != 0 && now.Before(sub.PreviousKeyExpiresAt) {
		keys = append(keys, "whsec_"+base64.StdEncoding.EncodeToString(sub.PreviousSigningKey))
	}
	// The last request cannot extend the overall retry window by another
	// callback timeout. The callback's own ten-second limit still applies.
	ctx, cancel := context.WithTimeout(ctx, pending.CreatedAt.Add(maxDeliveryElapsed).Sub(now))
	defer cancel()
	receipt, err := webhook.Send(ctx, sub.CallbackURL, pending.EventID, sub.ID, pending.Envelope, keys)
	if errors.Is(err, ErrUnavailable) || errors.Is(err, ErrRevoked) {
		pending.Attempts--
		sub.Status.Attempts = pending.Attempts
		sub.Status.Category = "source_unavailable"
		if errors.Is(err, ErrRevoked) {
			sub.Disabled = true
			sub.Pending = nil
			sub.Generation++
			sub.Status.Category = "principal_revoked"
		}
		return sub, persist(sub)
	}
	category := ""
	if err != nil {
		category = safeWebhookError(err).Reason
	} else if receipt.StatusCode >= 200 && receipt.StatusCode < 300 {
		sub.Positions = clonePosition(pending.NextPositions)
		sub.Pending = nil
		sub.Status.Category = "accepted"
		return sub, persist(sub)
	} else {
		category = fmt.Sprintf("http_%dxx", receipt.StatusCode/100)
	}
	sub.Status.LastFailureCategory = category
	sub.Status.LastFailureEventID = pending.EventID
	sub.Status.LastFailureAt = now
	sub.Status.Category = category
	if err == nil && (receipt.StatusCode == 410 || receipt.StatusCode == 413) {
		sub.Positions = clonePosition(pending.NextPositions)
		sub.Pending = nil
		sub.Status.Category = "abandoned"
		var envelope Envelope
		if json.Unmarshal(pending.Envelope, &envelope) != nil {
			return current, ErrState
		}
		if envelope.Data.Reason == "resync" {
			sub.ContinuityGeneration++
			sub.ContinuityLost = true
		} else {
			sub, err = prepareResync(identity, sub, now, true)
			if err != nil {
				return current, err
			}
		}
		return sub, persist(sub)
	}
	if pending.Attempts == maxDeliveryAttempts {
		pending.Failed = true
		sub.Status.Category = "retry_exhausted"
	} else {
		backoff := time.Second << uint(pending.Attempts-1)
		if receipt.RetryAfter > backoff {
			backoff = receipt.RetryAfter
		}
		pending.NextAttemptAt = now.Add(backoff)
		if deadline := pending.CreatedAt.Add(maxDeliveryElapsed); pending.NextAttemptAt.After(deadline) {
			pending.NextAttemptAt = deadline
		}
	}
	return sub, persist(sub)
}
