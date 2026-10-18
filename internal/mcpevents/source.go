package mcpevents

import (
	"context"
	"errors"
	"time"
)

var (
	ErrRevoked      = errors.New("event source authority revoked")
	ErrUnavailable  = errors.New("event source temporarily unavailable")
	ErrScopeChanged = errors.New("event source scope changed")
)

// Source exposes committed authorized daemon events without a database handle.
// Implementations bound raw responses before decoding and preserve vectors.
type Source interface {
	Resolve(context.Context, Arguments) (Scope, error)
	Authorize(context.Context, Scope) error
	Head(context.Context, Scope) (Position, error)
	Read(context.Context, Scope, Position, int) (SourcePage, error)
}

// SourceEvent.Positions is the vector immediately after this occurrence.
type SourceEvent struct {
	EventID        string
	SourceEventUID string
	ProjectUID     string
	IssueUID       string
	Reason         string
	Timestamp      time.Time
	Positions      Position
}

// SourcePage.Positions includes only inspected records. Reset carries a fresh
// authorized head and explicitly reports a gap, including an oversized event.
type SourcePage struct {
	Events    []SourceEvent
	Positions Position
	Reset     bool
}
