package mcpevents

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"

	"github.com/gofrs/flock"
	"github.com/oklog/ulid/v2"
	"go.kenn.io/kit/atomicfile"
	"go.kenn.io/kit/safefileio"
)

const stateVersion = 1

var (
	ErrState       = errors.New("MCP Events state unavailable; restore a complete private backup or explicitly re-register in a new state directory")
	ErrStateLocked = errors.New("MCP Events state already has a writer")
)

type stateRecord[T any] struct {
	Version int `json:"version"`
	Value   T   `json:"value"`
}

// State serializes private atomic replacements under one nonblocking process
// lock. Any persistence error poisons the open handle: callers must stop sends
// and reopen before interpreting uncertain on-disk watermarks.
type State struct {
	mu       sync.Mutex
	dir      string
	lock     *flock.Flock
	identity Identity
	closed   bool
	failed   bool
	write    func(string, []byte) error
	syncDir  func(string) error
}

func OpenState(dir string, binding Binding) (_ *State, retErr error) {
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return nil, ErrState
	}
	// Reject symlinked ancestors too: atomicfile normally resolves parent links.
	for p := dir; ; p = filepath.Dir(p) {
		info, err := os.Lstat(p)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, ErrState
		}
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			return nil, ErrState
		}
		if filepath.Dir(p) == p {
			break
		}
	}
	if _, err := os.Lstat(dir); errors.Is(err, os.ErrNotExist) {
		if err := safefileio.EnsurePrivateDir(dir); err != nil {
			return nil, fmt.Errorf("%w: create directory", ErrState)
		}
		if err := atomicfile.SyncDir(filepath.Dir(dir)); err != nil {
			return nil, errors.Join(ErrState, err)
		}
	}
	if err := safefileio.ValidatePrivateDir(dir); err != nil {
		return nil, fmt.Errorf("%w: directory privacy", ErrState)
	}
	lockPath := filepath.Join(dir, "writer.lock")
	f, err := safefileio.CreatePrivateFile(lockPath)
	if err == nil {
		err = f.Close()
	} else if errors.Is(err, os.ErrExist) {
		err = checkPrivateFile(lockPath)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: lock privacy", ErrState)
	}
	lock := flock.New(lockPath, flock.SetPermissions(0600))
	locked, err := lock.TryLock()
	if err != nil {
		return nil, errors.Join(ErrState, err)
	}
	if !locked {
		return nil, ErrStateLocked
	}
	s := &State{dir: dir, lock: lock, write: func(path string, b []byte) error { return atomicfile.WriteFile(path, b, atomicfile.WithPrivate()) }, syncDir: atomicfile.SyncDir}
	defer func() {
		if retErr != nil {
			_ = lock.Unlock()
		}
	}()
	if err := checkPrivateFile(lockPath); err != nil {
		return nil, ErrState
	}
	// Sort a copy, so caller-owned allowlists cannot mutate service authority.
	binding.ProjectUIDs = append([]string(nil), binding.ProjectUIDs...)
	sort.Strings(binding.ProjectUIDs)
	if err := validateBinding(binding); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "identity.json")
	err = readState(path, &s.identity)
	if errors.Is(err, os.ErrNotExist) {
		entries, readErr := os.ReadDir(dir)
		if readErr != nil {
			return nil, ErrState
		}
		for _, e := range entries {
			if e.Name() != "writer.lock" && !interruptedTemp(e.Name()) {
				return nil, ErrState
			}
		}
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, errors.Join(ErrState, err)
		}
		id := make([]byte, 32)
		if _, err := rand.Read(id); err != nil {
			return nil, errors.Join(ErrState, err)
		}
		s.identity = Identity{ID: hex.EncodeToString(id), CursorKey: key, Binding: binding}
		b, err := encodeState(s.identity)
		if err != nil {
			return nil, err
		}
		if err := atomicfile.WriteNew(path, b, atomicfile.WithPrivate()); err != nil {
			return nil, errors.Join(ErrState, err)
		}
	} else if err != nil {
		return nil, err
	}
	if !validID(s.identity.ID) || len(s.identity.CursorKey) != 32 || !reflect.DeepEqual(s.identity.Binding, binding) {
		return nil, fmt.Errorf("%w: service identity or authority binding mismatch", ErrState)
	}
	if _, err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// Identity returns a copy; consumers cannot mutate the bound identity or MAC key.
func (s *State) Identity() Identity {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.identity
	id.CursorKey = append([]byte(nil), id.CursorKey...)
	id.Binding.ProjectUIDs = append([]string(nil), id.Binding.ProjectUIDs...)
	return id
}

func (s *State) usable() error {
	if s.closed || s.failed {
		return ErrState
	}
	if err := safefileio.ValidatePrivateDir(s.dir); err != nil {
		return ErrState
	}
	for _, name := range []string{"identity.json", "writer.lock"} {
		if err := checkPrivateFile(filepath.Join(s.dir, name)); err != nil {
			return ErrState
		}
	}
	return nil
}

func (s *State) Load() ([]Subscription, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(); err != nil {
		return nil, err
	}
	return s.load()
}

func (s *State) load() ([]Subscription, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, errors.Join(ErrState, err)
	}
	var result []Subscription
	for _, e := range entries {
		name := e.Name()
		if name == "identity.json" || name == "writer.lock" || interruptedTemp(name) {
			continue
		}
		if !strings.HasSuffix(name, ".json") || !validID(strings.TrimSuffix(name, ".json")) || len(result) == MaxSubscriptions {
			return nil, ErrState
		}
		var sub Subscription
		if err := readState(filepath.Join(s.dir, name), &sub); err != nil {
			return nil, err
		}
		if sub.ID+".json" != name {
			return nil, ErrState
		}
		if err := validateSubscription(sub); err != nil {
			return nil, err
		}
		result = append(result, sub)
	}
	return result, nil
}

func (s *State) Save(sub Subscription) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(); err != nil {
		return err
	}
	if err := validateSubscription(sub); err != nil {
		return err
	}
	records, err := s.load()
	if err != nil {
		return err
	}
	if len(records) == MaxSubscriptions {
		found := false
		for _, r := range records {
			found = found || r.ID == sub.ID
		}
		if !found {
			return fmt.Errorf("%w: subscription capacity", ErrState)
		}
	}
	b, err := encodeState(sub)
	if err != nil {
		return err
	}
	if err := s.write(filepath.Join(s.dir, sub.ID+".json"), b); err != nil {
		s.failed = true
		return errors.Join(ErrState, err)
	}
	return nil
}

func (s *State) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(); err != nil {
		return err
	}
	if !validID(id) {
		return ErrState
	}
	path := filepath.Join(s.dir, id+".json")
	if err := checkPrivateFile(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return ErrState
	}
	if err := os.Remove(path); err != nil {
		s.failed = true
		return errors.Join(ErrState, err)
	}
	if err := s.syncDir(s.dir); err != nil {
		s.failed = true
		return errors.Join(ErrState, atomicfile.ErrNotDurable, err)
	}
	return nil
}

func (s *State) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.lock.Unlock()
}

func checkPrivateFile(path string) error {
	f, err := safefileio.OpenCurrentUserFile(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return safefileio.ValidatePrivateCurrentUserFile(f)
}

func readState[T any](path string, out *T) error {
	f, err := safefileio.OpenCurrentUserFile(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := safefileio.ValidatePrivateCurrentUserFile(f); err != nil {
		return ErrState
	}
	b, err := io.ReadAll(io.LimitReader(f, MaxStateBytes+1))
	if err != nil || len(b) > MaxStateBytes {
		return ErrState
	}
	var r stateRecord[T]
	if jsonv2.Unmarshal(b, &r, jsonv2.RejectUnknownMembers(true)) != nil || r.Version != stateVersion {
		return ErrState
	}
	*out = r.Value
	return nil
}

func encodeState[T any](v T) ([]byte, error) {
	b, err := json.Marshal(stateRecord[T]{Version: stateVersion, Value: v})
	if err != nil || len(b) > MaxStateBytes {
		return nil, ErrState
	}
	return b, nil
}

func validID(id string) bool {
	if len(id) != 64 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil && id == strings.ToLower(id)
}

func interruptedTemp(name string) bool {
	if !strings.HasPrefix(name, ".") {
		return false
	}
	stem, suffix, ok := strings.Cut(strings.TrimPrefix(name, "."), ".tmp-")
	if !ok || suffix == "" || (stem != "identity.json" && (!strings.HasSuffix(stem, ".json") || !validID(strings.TrimSuffix(stem, ".json")))) {
		return false
	}
	for _, c := range suffix {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func validateBinding(b Binding) error {
	if b.BackendOrigin == "" || b.DaemonUID == "" || b.BackendCredentialFingerprint == "" || b.BackendPrincipal == "" || b.TransportPrincipal == "" || b.Actor == "" || b.ScopeMode == "" || len(b.ProjectUIDs) > MaxProjects {
		return ErrState
	}
	for i, id := range b.ProjectUIDs {
		if _, err := ulid.ParseStrict(id); err != nil || (i > 0 && b.ProjectUIDs[i-1] == id) {
			return ErrState
		}
	}
	return nil
}

func validatePosition(p Position) error {
	if len(p) > MaxProjects {
		return ErrState
	}
	for id, n := range p {
		if _, err := ulid.ParseStrict(id); err != nil || n < 0 {
			return ErrState
		}
	}
	return nil
}

func validateSubscription(s Subscription) error {
	if !validID(s.ID) || s.Event != "kata.inbox.changed" || s.Arguments.For == "" || s.CallbackURL == "" || len(s.SigningKey) < 24 || len(s.SigningKey) > 64 || s.ExpiresAt.IsZero() || s.Generation == 0 || len(s.Scope.Projects) > MaxProjects {
		return ErrState
	}
	if len(s.PreviousSigningKey) != 0 && (len(s.PreviousSigningKey) < 24 || len(s.PreviousSigningKey) > 64 || s.PreviousKeyExpiresAt.IsZero()) {
		return ErrState
	}
	if err := validatePosition(s.Positions); err != nil {
		return err
	}
	if p := s.Pending; p != nil {
		if p.EventID == "" || len(p.Envelope) == 0 || len(p.Envelope) > MaxEnvelopeBytes || !json.Valid(p.Envelope) || p.Attempts < 0 || p.Attempts > 8 {
			return ErrState
		}
		if err := validatePosition(p.NextPositions); err != nil {
			return err
		}
	}
	return nil
}
