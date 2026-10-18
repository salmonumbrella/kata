package mcpevents

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"
)

func cursorIdentity() Identity {
	return Identity{ID: strings.Repeat("c", 64), CursorKey: bytes.Repeat([]byte{9}, 32), Binding: stateBinding()}
}

func TestCursorExactVectorRoundTrip(t *testing.T) {
	id := cursorIdentity()
	want := Position{"01ARZ3NDEKTSV4RRFFQ69G5FAV": math.MaxInt64, "01ARZ3NDEKTSV4RRFFQ69G5FAW": 0}
	raw, err := EncodeCursor(id, "kata.inbox.changed", Arguments{For: " example-user "}, math.MaxUint64, want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeCursor(id, "kata.inbox.changed", Arguments{For: "example-user"}, raw)
	if err != nil || got.Generation != math.MaxUint64 || !reflect.DeepEqual(got.Positions, want) {
		t.Fatalf("cursor lost exact integers: %#v %v", got, err)
	}
	other, err := EncodeCursor(id, "kata.inbox.changed", Arguments{For: "example-user"}, math.MaxUint64, Position{"01ARZ3NDEKTSV4RRFFQ69G5FAW": 0, "01ARZ3NDEKTSV4RRFFQ69G5FAV": math.MaxInt64})
	if err != nil || other != raw {
		t.Fatalf("equivalent canonical identity differs: %v", err)
	}
	body, _, _ := strings.Cut(raw, ".")
	decoded, _ := base64.RawURLEncoding.DecodeString(body)
	if bytes.Contains(decoded, []byte("restricted")) || bytes.Contains(decoded, []byte("example-user")) {
		t.Fatal("opaque cursor exposed principal or recipient")
	}
}

func TestCursorRejectsDifferentAuthorityOrArguments(t *testing.T) {
	id := cursorIdentity()
	args := Arguments{For: "example-user"}
	raw, err := EncodeCursor(id, "kata.inbox.changed", args, 0, Position{})
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"ID", "CursorKey", "DaemonUID", "BackendOrigin", "BackendCredentialFingerprint", "BackendPrincipal", "TransportPrincipal", "Actor", "ScopeMode", "ProjectUIDs"} {
		t.Run(field, func(t *testing.T) {
			other := cursorIdentity()
			switch field {
			case "ID":
				other.ID = strings.Repeat("d", 64)
			case "CursorKey":
				other.CursorKey[0]++
			case "ProjectUIDs":
				other.Binding.ProjectUIDs = nil
			default:
				v := reflect.ValueOf(&other.Binding).Elem().FieldByName(field)
				v.SetString(v.String() + "-changed")
			}
			if _, err := DecodeCursor(other, "kata.inbox.changed", args, raw); err == nil {
				t.Fatal("accepted foreign authority")
			}
		})
	}
	for _, bad := range []Arguments{{For: "another-user"}, {For: "example-user", ProjectUID: "01ARZ3NDEKTSV4RRFFQ69G5FAW"}} {
		if _, err := DecodeCursor(id, "kata.inbox.changed", bad, raw); err == nil {
			t.Fatal("accepted other arguments")
		}
	}
	if _, err := DecodeCursor(id, "another.event", args, raw); err == nil {
		t.Fatal("accepted another event")
	}
}

func signedTestCursor(body []byte) string {
	mac := hmac.New(sha256.New, cursorIdentity().CursorKey)
	_, _ = mac.Write(body)
	return base64.RawURLEncoding.EncodeToString(body) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func TestCursorRejectsMalformedAuthenticatedBodies(t *testing.T) {
	id := cursorIdentity()
	args := Arguments{For: "example-user"}
	raw, err := EncodeCursor(id, "kata.inbox.changed", args, 0, Position{})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _, _ := strings.Cut(raw, ".")
	valid, _ := base64.RawURLEncoding.DecodeString(encoded)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(valid, &fields); err != nil {
		t.Fatal(err)
	}
	bad := [][]byte{[]byte("{"), []byte("null"), append(append([]byte(nil), valid...), []byte(" {}")...), append([]byte(`{"version":1,`), valid[1:]...)}
	for field, values := range map[string][]string{"version": {"0", "2", "null"}, "generation": {"-1", "1.5", "18446744073709551616", "null"}, "positions": {"null", `{"01ARZ3NDEKTSV4RRFFQ69G5FAV":null}`, `{"01ARZ3NDEKTSV4RRFFQ69G5FAV":-1}`, `{"01ARZ3NDEKTSV4RRFFQ69G5FAV":1.5}`, `{"01ARZ3NDEKTSV4RRFFQ69G5FAV":9223372036854775808}`, `{"../path":1}`, `{"01ARZ3NDEKTSV4RRFFQ69G5FAV":1,"01ARZ3NDEKTSV4RRFFQ69G5FAV":2}`}, "unknown": {"true"}, "Generation": {"1"}} {
		for _, value := range values {
			copyFields := map[string]json.RawMessage{}
			for k, v := range fields {
				copyFields[k] = v
			}
			copyFields[field] = json.RawMessage(value)
			b, _ := json.Marshal(copyFields)
			bad = append(bad, b)
		}
	}
	for i, b := range bad {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			if _, err := DecodeCursor(id, "kata.inbox.changed", args, signedTestCursor(b)); err == nil {
				t.Fatalf("accepted malformed authenticated payload %s", b)
			}
		})
	}
}

func TestCursorBoundsAndForgedInput(t *testing.T) {
	id := cursorIdentity()
	args := Arguments{For: "example-user"}
	for _, raw := range []string{"", "a.b", strings.Repeat("x", 16385), "a.b.c", "====.===="} {
		if _, err := DecodeCursor(id, "kata.inbox.changed", args, raw); err == nil {
			t.Fatal("accepted invalid or oversized cursor")
		}
	}
	positions := Position{}
	for i := 0; i < 128; i++ {
		var uid ulid.ULID
		uid[14] = byte(i)
		positions[uid.String()] = int64(i)
	}
	raw, err := EncodeCursor(id, "kata.inbox.changed", args, 1, positions)
	if err != nil || len(raw) > 16384 {
		t.Fatalf("cannot encode maximum scope: %v bytes=%d", err, len(raw))
	}
	positions["01ARZ3NDEKTSV4RRFFQ69G5FAV"] = 129
	if _, err := EncodeCursor(id, "kata.inbox.changed", args, 1, positions); err == nil {
		t.Fatal("accepted project count over bound")
	}
	for _, bad := range []Arguments{{For: ""}, {For: "user\x00"}, {For: "user", IssueUID: "01ARZ3NDEKTSV4RRFFQ69G5FAV"}, {For: "user", ProjectUID: "bad"}} {
		if _, err := EncodeCursor(id, "kata.inbox.changed", bad, 0, Position{}); err == nil {
			t.Fatalf("accepted bad arguments %#v", bad)
		}
	}
}

func FuzzCursorVectorRoundTrip(f *testing.F) {
	f.Add(uint64(0), []byte{})
	f.Add(uint64(math.MaxUint64), []byte{255, 0, 1, 8, 16, 32})
	f.Fuzz(func(t *testing.T, generation uint64, data []byte) {
		// Materialization bounds match the documented scope, not the drawn domain.
		if len(data) > 128*24 {
			data = data[:128*24]
		}
		positions := Position{}
		for i := 0; i < len(data); i += 24 {
			var uid ulid.ULID
			chunk := data[i:min(i+24, len(data))]
			copy(uid[:], chunk)
			var n int64
			for _, b := range chunk[min(16, len(chunk)):] {
				n = (n << 8) | int64(b)
			}
			positions[uid.String()] = n & math.MaxInt64
		}
		id := cursorIdentity()
		args := Arguments{For: "example-user"}
		raw, err := EncodeCursor(id, "kata.inbox.changed", args, generation, positions)
		if err != nil {
			t.Fatal(err)
		}
		got, err := DecodeCursor(id, "kata.inbox.changed", args, raw)
		if err != nil || got.Generation != generation || !reflect.DeepEqual(got.Positions, positions) {
			t.Fatalf("roundtrip lost vector: %#v %v", got, err)
		}
	})
}

func FuzzCursorRejectsMutation(f *testing.F) {
	f.Add(uint64(0), byte(1))
	f.Add(uint64(80), byte(255))
	f.Fuzz(func(t *testing.T, index uint64, mask byte) {
		id := cursorIdentity()
		args := Arguments{For: "example-user"}
		raw, err := EncodeCursor(id, "kata.inbox.changed", args, 0, Position{})
		if err != nil {
			t.Fatal(err)
		}
		b := []byte(raw)
		b[index%uint64(len(b))] ^= mask | 1
		if _, err := DecodeCursor(id, "kata.inbox.changed", args, string(b)); err == nil {
			t.Fatal("accepted mutated cursor")
		}
	})
}

func TestCursorCanonicalScopeAndArguments(t *testing.T) {
	id := cursorIdentity()
	id.Binding.ProjectUIDs = []string{"01ARZ3NDEKTSV4RRFFQ69G5FAW", "01ARZ3NDEKTSV4RRFFQ69G5FAV"}
	args := Arguments{For: "example-user", ProjectUID: "01arz3ndektsv4rrffq69g5fav", IssueUID: "01arz3ndektsv4rrffq69g5fax"}
	raw, err := EncodeCursor(id, "kata.inbox.changed", args, 0, Position{})
	if err != nil {
		t.Fatal(err)
	}
	id.Binding.ProjectUIDs[0], id.Binding.ProjectUIDs[1] = id.Binding.ProjectUIDs[1], id.Binding.ProjectUIDs[0]
	args.ProjectUID = strings.ToUpper(args.ProjectUID)
	args.IssueUID = strings.ToUpper(args.IssueUID)
	if _, err := DecodeCursor(id, "kata.inbox.changed", args, raw); err != nil {
		t.Fatal("equivalent canonical scope rejected:", err)
	}
	args.IssueUID = ""
	if _, err := DecodeCursor(id, "kata.inbox.changed", args, raw); err == nil {
		t.Fatal("issue filter widened during replay")
	}
}

func FuzzCursorDecodeCanonical(f *testing.F) {
	raw, err := EncodeCursor(cursorIdentity(), "kata.inbox.changed", Arguments{For: "example-user"}, 0, Position{})
	if err != nil {
		f.Fatal(err)
	}
	f.Add(raw)
	f.Add("")
	f.Add("null.bad")
	f.Fuzz(func(t *testing.T, raw string) {
		id := cursorIdentity()
		args := Arguments{For: "example-user"}
		got, err := DecodeCursor(id, "kata.inbox.changed", args, raw)
		if err != nil {
			return
		}
		canonical, err := EncodeCursor(id, "kata.inbox.changed", args, got.Generation, got.Positions)
		if err != nil || canonical != raw {
			t.Fatal("accepted noncanonical cursor")
		}
	})
}
