// Package mcpevents owns the optional durable MCP Events webhook service.
package mcpevents

import (
	json "encoding/json/v2"
	"time"
)

const (
	MaxSubscriptions = 128
	MaxProjects      = 128
	MaxStateBytes    = 1 << 20
	MaxEnvelopeBytes = 256 << 10
)

// Arguments identifies an exact recipient and optional stable scope filters.
type Arguments struct {
	For        string `json:"for"`
	ProjectUID string `json:"project_uid,omitempty"`
	IssueUID   string `json:"issue_uid,omitempty"`
}

// Position is a vector; project log sequence numbers cannot be combined.
type Position map[string]int64

// UnmarshalJSON rejects null entries instead of silently converting them to a
// safe-looking zero watermark. A nil vector still denotes an empty scope.
func (p *Position) UnmarshalJSON(raw []byte) error {
	var fields map[string]*int64
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	if fields == nil {
		*p = nil
		return nil
	}
	if len(fields) > MaxProjects {
		return ErrState
	}
	result := make(Position, len(fields))
	for id, n := range fields {
		if n == nil {
			return ErrState
		}
		result[id] = *n
	}
	if err := validatePosition(result); err != nil {
		return err
	}
	*p = result
	return nil
}

type Project struct {
	UID  string `json:"uid"`
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type Scope struct {
	Arguments Arguments `json:"arguments"`
	Projects  []Project `json:"projects"`
	Dynamic   bool      `json:"dynamic"`
}

// Binding contains identities, never raw bearer credentials. The adapter must
// derive these from resolved backend authority; stdio is not owner authority.
type Binding struct {
	BackendOrigin                string   `json:"backend_origin"`
	DaemonUID                    string   `json:"daemon_uid"`
	BackendCredentialFingerprint string   `json:"backend_credential_fingerprint"`
	BackendPrincipal             string   `json:"backend_principal"`
	TransportPrincipal           string   `json:"transport_principal"`
	Actor                        string   `json:"actor"`
	ScopeMode                    string   `json:"scope_mode"`
	ProjectUIDs                  []string `json:"project_uids"`
}

// Identity must be included in private backups alongside subscription records.
// Keys are private local credentials, not encrypted-at-rest storage.
type Identity struct {
	ID        string  `json:"id"`
	CursorKey []byte  `json:"cursor_key"`
	Binding   Binding `json:"binding"`
}

type Subscription struct {
	ID                   string         `json:"id"`
	Event                string         `json:"event"`
	Arguments            Arguments      `json:"arguments"`
	Scope                Scope          `json:"scope"`
	CallbackURL          string         `json:"callback_url"`
	SigningKey           []byte         `json:"signing_key"`
	PreviousSigningKey   []byte         `json:"previous_signing_key,omitempty"`
	PreviousKeyExpiresAt time.Time      `json:"previous_key_expires_at,omitempty"`
	ExpiresAt            time.Time      `json:"expires_at"`
	RefreshBefore        time.Time      `json:"refresh_before"`
	Generation           uint64         `json:"generation"`
	ContinuityGeneration uint64         `json:"continuity_generation"`
	ContinuityLost       bool           `json:"continuity_lost"`
	Positions            Position       `json:"positions"`
	Pending              *Pending       `json:"pending,omitempty"`
	Status               DeliveryStatus `json:"status"`
	Disabled             bool           `json:"disabled"`
}

// Pending preserves the exact bytes and next vector atomically with the safe
// acknowledged Positions. It is never a second independently persisted queue.
type Pending struct {
	EventID       string    `json:"event_id"`
	Envelope      []byte    `json:"envelope"`
	NextPositions Position  `json:"next_positions"`
	Attempts      int       `json:"attempts"`
	CreatedAt     time.Time `json:"created_at"`
	NextAttemptAt time.Time `json:"next_attempt_at"`
	Failed        bool      `json:"failed"`
}

// DeliveryStatus contains only safe categorized diagnostics, never responses,
// callback URLs, payloads, or transport errors.
type DeliveryStatus struct {
	Category  string    `json:"category,omitempty"`
	EventID   string    `json:"event_id,omitempty"`
	Attempts  int       `json:"attempts"`
	UpdatedAt time.Time `json:"updated_at"`
}
