package mcpevents

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.kenn.io/kit/atomicfile"
)

func stateBinding() Binding {
	return Binding{BackendOrigin: "https://daemon.example", DaemonUID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", BackendCredentialFingerprint: strings.Repeat("a", 64), BackendPrincipal: "restricted", TransportPrincipal: "stdio:fixture-user", Actor: "example-user", ScopeMode: "allowlist", ProjectUIDs: []string{"01ARZ3NDEKTSV4RRFFQ69G5FAV"}}
}

func stateSubscription() Subscription {
	return Subscription{ID: strings.Repeat("b", 64), Event: "kata.inbox.changed", Arguments: Arguments{For: "example-user"}, Scope: Scope{Dynamic: true}, CallbackURL: "https://receiver.example/events", SigningKey: bytes.Repeat([]byte{7}, 32), ExpiresAt: time.Unix(2000000000, 0).UTC(), Generation: 1, Positions: Position{"01ARZ3NDEKTSV4RRFFQ69G5FAV": 12}, Pending: &Pending{EventID: "occurrence-1", Envelope: []byte("{\n  \"eventId\": \"occurrence-1\"\n}"), NextPositions: Position{"01ARZ3NDEKTSV4RRFFQ69G5FAV": 13}}}
}

func openTestState(t *testing.T) (*State, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "events")
	s, err := OpenState(dir, stateBinding())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, dir
}

func TestStateExclusiveOwner(t *testing.T) {
	s, dir := openTestState(t)
	if other, err := OpenState(dir, stateBinding()); err == nil {
		_ = other.Close()
		t.Fatal("second writer acquired directory")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	other, err := OpenState(dir, stateBinding())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err := s.Save(stateSubscription()); err == nil {
		t.Fatal("closed writer accepted a record")
	}
}

func TestStatePrivateFiles(t *testing.T) {
	s, dir := openTestState(t)
	if err := s.Save(stateSubscription()); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		for _, entry := range entries {
			info, _ := entry.Info()
			if info.Mode().Perm() != 0600 {
				t.Fatalf("file %s mode %o", entry.Name(), info.Mode().Perm())
			}
		}
		if err := os.Chmod(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Load(); err == nil {
			t.Fatal("accepted directory with public permissions")
		}
		_ = os.Chmod(dir, 0700)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, stateSubscription().ID+".json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "identity.json"), filepath.Join(dir, stateSubscription().ID+".json")); err != nil {
		t.Skip(err)
	}
	if reopened, err := OpenState(dir, stateBinding()); err == nil {
		reopened.Close()
		t.Fatal("followed subscription symlink")
	}
}

func TestStateAtomicRecovery(t *testing.T) {
	s, dir := openTestState(t)
	want := stateSubscription()
	if err := s.Save(want); err != nil {
		t.Fatal(err)
	}
	id := s.Identity()
	id.CursorKey[0] ^= 0xff
	if bytes.Equal(id.CursorKey, s.Identity().CursorKey) {
		t.Fatal("identity exposes mutable signing key")
	}
	if err := os.WriteFile(filepath.Join(dir, "."+want.ID+".json.tmp-123"), []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenState(dir, stateBinding())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, err := reopened.Load()
	if err != nil || len(got) != 1 || !reflect.DeepEqual(got[0], want) {
		t.Fatalf("round trip: %v %#v", err, got)
	}
	if reopened.Identity().ID != s.Identity().ID {
		t.Fatal("service identity changed across reopen")
	}
	if err := reopened.Delete(want.ID); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Delete(want.ID); err != nil {
		t.Fatal("delete is not idempotent:", err)
	}
	got, err = reopened.Load()
	if err != nil || len(got) != 0 {
		t.Fatalf("removed record retained: %v %v", got, err)
	}
}

func TestStateRejectsCorruptUnknownAndOversized(t *testing.T) {
	valid, err := encodeState(stateSubscription())
	if err != nil {
		t.Fatal(err)
	}
	for i, bad := range []string{"{", `{"version":99}`, `{"version":1,"unexpected":true}`, `{"version":1,"version":1}`, strings.Replace(string(valid), `"version"`, `"Version"`, 1), strings.Replace(string(valid), `:12`, `:null`, 1), strings.Repeat("x", (1<<20)+1)} {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			s, dir := openTestState(t)
			if err := s.Save(stateSubscription()); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, stateSubscription().ID+".json"), []byte(bad), 0600); err != nil {
				t.Fatal(err)
			}
			if opened, err := OpenState(dir, stateBinding()); err == nil {
				opened.Close()
				t.Fatal("accepted invalid state")
			}
		})
	}
}

func TestStateRejectsUnsafePathsAndPermissions(t *testing.T) {
	s, dir := openTestState(t)
	for _, id := range []string{"../outside", strings.Repeat("A", 64), "", strings.Repeat("g", 64)} {
		bad := stateSubscription()
		bad.ID = id
		if err := s.Save(bad); err == nil {
			t.Fatalf("accepted record path %q", id)
		}
		if err := s.Delete(id); err == nil {
			t.Fatalf("accepted delete path %q", id)
		}
	}
	if err := s.Save(stateSubscription()); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		return
	} // ACL enforcement belongs to safefileio's platform tests.
	for _, name := range []string{"writer.lock", "identity.json", stateSubscription().ID + ".json"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, name)
			if err := os.Chmod(path, 0644); err != nil {
				t.Fatal(err)
			}
			if opened, err := OpenState(dir, stateBinding()); err == nil {
				opened.Close()
				t.Fatal("accepted public state file")
			}
			if err := os.Chmod(path, 0600); err != nil {
				t.Fatal(err)
			}
		})
	}
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if opened, err := OpenState(dir, stateBinding()); err == nil {
		opened.Close()
		t.Fatal("accepted public directory")
	}
	info, err := os.Stat(dir)
	if err != nil || info.Mode().Perm() != 0755 {
		t.Fatal("silently repaired an insecure directory")
	}
	_ = os.Chmod(dir, 0700)
	link := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(dir, link); err != nil {
		t.Skip(err)
	}
	if opened, err := OpenState(link, stateBinding()); err == nil {
		opened.Close()
		t.Fatal("accepted symlinked directory")
	}
}

func TestStateCapacityAndPendingBounds(t *testing.T) {
	s, dir := openTestState(t)
	for n := 0; n < MaxSubscriptions; n++ {
		sub := stateSubscription()
		sub.ID = fmt.Sprintf("%064x", n)
		data, err := encodeState(sub)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, sub.ID+".json"), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Save(stateSubscription()); err == nil {
		t.Fatal("silently exceeded subscription capacity")
	}
	existing := stateSubscription()
	existing.ID = strings.Repeat("0", 64)
	if err := s.Save(existing); err != nil {
		t.Fatal("could not update at capacity:", err)
	}
	if err := s.Delete(existing.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(stateSubscription()); err != nil {
		t.Fatal("capacity not released:", err)
	}
	bad := stateSubscription()
	bad.Pending.Envelope = append([]byte{'"'}, bytes.Repeat([]byte{'x'}, MaxEnvelopeBytes)...)
	bad.Pending.Envelope = append(bad.Pending.Envelope, '"')
	if err := s.Save(bad); err == nil {
		t.Fatal("accepted oversized pending envelope")
	}
	bad = stateSubscription()
	bad.Pending.NextPositions["01ARZ3NDEKTSV4RRFFQ69G5FAV"] = -1
	if err := s.Save(bad); err == nil {
		t.Fatal("accepted negative source position")
	}
	bad = stateSubscription()
	bad.Pending.Envelope = []byte("not JSON")
	if err := s.Save(bad); err == nil {
		t.Fatal("accepted invalid envelope")
	}
}

func TestStateDeletionSyncFailureClosesAuthority(t *testing.T) {
	s, _ := openTestState(t)
	if err := s.Save(stateSubscription()); err != nil {
		t.Fatal(err)
	}
	s.syncDir = func(string) error { return errors.New("fixture directory sync failure") }
	if err := s.Delete(stateSubscription().ID); !errors.Is(err, atomicfile.ErrNotDurable) {
		t.Fatalf("delete acknowledged uncertain durability: %v", err)
	}
	if err := s.Save(stateSubscription()); err == nil {
		t.Fatal("failed deletion allowed subscription resurrection")
	}
}

func TestStateMissingIdentityDoesNotRebindRecords(t *testing.T) {
	s, dir := openTestState(t)
	if err := s.Save(stateSubscription()); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "identity.json")); err != nil {
		t.Fatal(err)
	}
	b := stateBinding()
	b.Actor = "other-user"
	if reopened, err := OpenState(dir, b); err == nil {
		reopened.Close()
		t.Fatal("adopted records after losing service identity")
	}
}

func TestStateBindingMismatch(t *testing.T) {
	s, dir := openTestState(t)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"BackendOrigin", "DaemonUID", "BackendCredentialFingerprint", "BackendPrincipal", "TransportPrincipal", "Actor", "ScopeMode"} {
		t.Run(field, func(t *testing.T) {
			b := stateBinding()
			v := reflect.ValueOf(&b).Elem().FieldByName(field)
			v.SetString(v.String() + "-changed")
			if opened, err := OpenState(dir, b); err == nil {
				opened.Close()
				t.Fatal("accepted changed authority")
			}
		})
	}
}

func TestStateDurabilityFailureDoesNotAcknowledge(t *testing.T) {
	for _, published := range []bool{false, true} {
		t.Run(map[bool]string{false: "before-publish", true: "after-publish"}[published], func(t *testing.T) {
			s, dir := openTestState(t)
			want := stateSubscription()
			if err := s.Save(want); err != nil {
				t.Fatal(err)
			}
			write := s.write
			s.write = func(path string, data []byte) error {
				if published {
					if err := write(path, data); err != nil {
						t.Fatal(err)
					}
					return atomicfile.ErrNotDurable
				}
				return errors.New("fixture full disk")
			}
			want.Positions["01ARZ3NDEKTSV4RRFFQ69G5FAV"] = 13
			want.Pending = nil
			err := s.Save(want)
			if err == nil || (published && !errors.Is(err, atomicfile.ErrNotDurable)) {
				t.Fatalf("durability error lost: %v", err)
			}
			s.write = write
			if err := s.Save(want); err == nil {
				t.Fatal("failed writer resumed without reopening")
			}
			if _, err := s.Load(); err == nil {
				t.Fatal("failed writer exposed uncertain safe watermark")
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := OpenState(dir, stateBinding())
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			got, err := reopened.Load()
			if err != nil || len(got) != 1 {
				t.Fatalf("reopen %v %v", got, err)
			}
			if published && got[0].Pending != nil {
				t.Fatal("new complete record absent")
			}
			if !published && got[0].Pending == nil {
				t.Fatal("old complete record lost")
			}
		})
	}
}
