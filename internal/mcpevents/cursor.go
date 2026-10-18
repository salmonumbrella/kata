package mcpevents

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"sort"
	"strings"

	"github.com/oklog/ulid/v2"
	"go.kenn.io/kata/internal/notification"
)

const MaxCursorBytes = 16 << 10

var ErrCursor = errors.New("invalid MCP Events cursor")

// Cursor carries a per-project safe watermark and its continuity epoch. The
// service must compare Positions component-wise against its live safe vector
// before accepting a refresh; authentication alone cannot reject future state.
type Cursor struct {
	Generation uint64
	Positions  Position
}

type cursorRecord struct {
	Version       int               `json:"version"`
	ServiceID     string            `json:"service_id"`
	DaemonUID     string            `json:"daemon_uid"`
	BindingHash   string            `json:"binding_hash"`
	ArgumentsHash string            `json:"arguments_hash"`
	Generation    *uint64           `json:"generation"`
	Positions     map[string]*int64 `json:"positions"`
}

// EncodeCursor authenticates the exact event/arguments, service/backend binding
// and vector without putting credentials, recipients, or callback URLs inside.
func EncodeCursor(id Identity, event string, args Arguments, continuity uint64, position Position) (string, error) {
	bh, ah, err := cursorIdentityHashes(id, event, args)
	if err != nil || validateCursorPosition(position) != nil {
		return "", ErrCursor
	}
	positions := make(map[string]*int64, len(position))
	for k, v := range position {
		n := v
		positions[k] = &n
	}
	r := cursorRecord{Version: 1, ServiceID: id.ID, DaemonUID: id.Binding.DaemonUID, BindingHash: bh, ArgumentsHash: ah, Generation: &continuity, Positions: positions}
	b, err := json.Marshal(r)
	if err != nil {
		return "", ErrCursor
	}
	mac := hmac.New(sha256.New, id.CursorKey)
	_, _ = mac.Write(b)
	raw := base64.RawURLEncoding.EncodeToString(b) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if len(raw) > MaxCursorBytes {
		return "", ErrCursor
	}
	return raw, nil
}

func DecodeCursor(id Identity, event string, args Arguments, raw string) (Cursor, error) {
	bad := Cursor{}
	if len(raw) == 0 || len(raw) > MaxCursorBytes {
		return bad, ErrCursor
	}
	bh, ah, err := cursorIdentityHashes(id, event, args)
	if err != nil {
		return bad, ErrCursor
	}
	body, signature, ok := strings.Cut(raw, ".")
	if !ok {
		return bad, ErrCursor
	}
	b, err := base64.RawURLEncoding.Strict().DecodeString(body)
	if err != nil || base64.RawURLEncoding.EncodeToString(b) != body {
		return bad, ErrCursor
	}
	sig, err := base64.RawURLEncoding.Strict().DecodeString(signature)
	if err != nil || len(sig) != sha256.Size || base64.RawURLEncoding.EncodeToString(sig) != signature {
		return bad, ErrCursor
	}
	mac := hmac.New(sha256.New, id.CursorKey)
	_, _ = mac.Write(b)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return bad, ErrCursor
	}
	var r cursorRecord
	if jsonv2.Unmarshal(b, &r, jsonv2.RejectUnknownMembers(true)) != nil || r.Version != 1 || r.ServiceID != id.ID || r.DaemonUID != id.Binding.DaemonUID || r.BindingHash != bh || r.ArgumentsHash != ah || r.Generation == nil || r.Positions == nil || len(r.Positions) > MaxProjects {
		return bad, ErrCursor
	}
	positions := make(Position, len(r.Positions))
	for k, v := range r.Positions {
		if v == nil {
			return bad, ErrCursor
		}
		positions[k] = *v
	}
	if validateCursorPosition(positions) != nil {
		return bad, ErrCursor
	}
	return Cursor{Generation: *r.Generation, Positions: positions}, nil
}

func cursorIdentityHashes(id Identity, event string, args Arguments) (string, string, error) {
	if !validID(id.ID) || len(id.CursorKey) != 32 || event != "kata.inbox.changed" {
		return "", "", ErrCursor
	}
	binding := id.Binding
	binding.ProjectUIDs = append([]string(nil), binding.ProjectUIDs...)
	sort.Strings(binding.ProjectUIDs)
	if validateBinding(binding) != nil {
		return "", "", ErrCursor
	}
	recipient, err := notification.NormalizeRecipient(args.For)
	if err != nil {
		return "", "", ErrCursor
	}
	args.For = recipient
	if args.IssueUID != "" && args.ProjectUID == "" {
		return "", "", ErrCursor
	}
	for _, p := range []*string{&args.ProjectUID, &args.IssueUID} {
		if *p == "" {
			continue
		}
		u, err := ulid.ParseStrict(*p)
		if err != nil {
			return "", "", ErrCursor
		}
		*p = u.String()
	}
	b, _ := json.Marshal(binding)
	bound := sha256.Sum256(b)
	b, _ = json.Marshal(struct {
		Event     string    `json:"event"`
		Arguments Arguments `json:"arguments"`
	}{event, args})
	arguments := sha256.Sum256(b)
	return hex.EncodeToString(bound[:]), hex.EncodeToString(arguments[:]), nil
}

func validateCursorPosition(position Position) error {
	if position == nil || validatePosition(position) != nil {
		return ErrCursor
	}
	for id := range position {
		u, _ := ulid.ParseStrict(id)
		if u.String() != id {
			return ErrCursor
		}
	}
	return nil
}
