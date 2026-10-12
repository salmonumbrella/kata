package issuemove

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"

	"pgregory.net/rapid"
)

const (
	batchRoot    = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	batchProject = "01ARZ3NDEKTSV4RRFFQ69G5FAW"
	batchOther   = "01ARZ3NDEKTSV4RRFFQ69G5FAX"
	batchA       = "01ARZ3NDEKTSV4RRFFQ69G5FAY"
	batchB       = "01ARZ3NDEKTSV4RRFFQ69G5FAZ"
	batchC       = "01ARZ3NDEKTSV4RRFFQ69G5FB0"
	batchD       = "01ARZ3NDEKTSV4RRFFQ69G5FB1"
)

// The approved bulk contract requires one indivisible operation per overlapping
// closure and rejects a shared descendant assigned to different destinations.
func TestNormalizeBatchSelectionsOverlap(t *testing.T) {
	input := []BatchSelection{
		{batchRoot, batchProject, []string{batchC, batchB}},
		{batchRoot, batchProject, []string{batchA, batchA}},
		{batchRoot, batchProject, []string{batchB, batchA}},
	}
	want := []BatchSelection{{batchRoot, batchProject, []string{batchA, batchB, batchC}}}
	got, err := NormalizeBatchSelections(input)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, %v; want %v", got, err, want)
	}
	if input[0].MemberUIDs[0] != batchC {
		t.Fatal("normalization mutated caller's closure")
	}
}

func TestNormalizeBatchSelectionsDestinationConflict(t *testing.T) {
	for _, destination := range []BatchSelection{
		{batchRoot, batchOther, []string{batchA}},
		{batchOther, batchProject, []string{batchA}},
	} {
		got, err := NormalizeBatchSelections([]BatchSelection{{batchRoot, batchProject, []string{batchA}}, destination})
		if !errors.Is(err, ErrBatchDestinationConflict) || got != nil {
			t.Fatalf("got %v, %v; want conflict without partial result", got, err)
		}
	}
}

func TestNormalizeBatchSelectionsInvalidIdentity(t *testing.T) {
	for _, invalid := range []BatchSelection{
		{"", batchProject, []string{batchA}},
		{batchRoot, "project-name", []string{batchA}},
		{batchRoot, batchProject, nil},
		{batchRoot, batchProject, []string{""}},
	} {
		if got, err := NormalizeBatchSelections([]BatchSelection{invalid}); !errors.Is(err, ErrInvalidBatchSelection) || got != nil {
			t.Fatalf("accepted invalid closure: %v, %v", got, err)
		}
	}
}

func TestNormalizeBatchSelectionsPreservesDisjointTrees(t *testing.T) {
	input := []BatchSelection{{batchRoot, batchProject, []string{batchD}}, {batchRoot, batchProject, []string{batchA}}}
	want := []BatchSelection{{batchRoot, batchProject, []string{batchA}}, {batchRoot, batchProject, []string{batchD}}}
	got, err := NormalizeBatchSelections(input)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, %v; want separate trees %v", got, err, want)
	}
}

func TestNormalizeBatchSelectionsLargeSelection(t *testing.T) {
	// More than preview-page limits, with completed/deleted members represented
	// identically by stable UID: normalization may not filter lifecycle state.
	members := make([]string, 100_000)
	for i := range members {
		members[i] = fmt.Sprintf("%026d", i)
	}
	got, err := NormalizeBatchSelections([]BatchSelection{{batchRoot, batchProject, members}})
	if err != nil || len(got) != 1 || !slices.Equal(got[0].MemberUIDs, members) {
		t.Fatalf("large closure truncated or reordered incorrectly: %v", err)
	}
}

func TestNormalizeBatchSelectionsPermutationProperty(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		data := rapid.SliceOf(rapid.Byte()).Draw(t, "closure")
		// Bound materialization work, without filtering drawn byte values.
		if len(data) > 256 {
			data = data[:256]
		}
		if len(data) == 0 {
			data = []byte{0}
		}
		members := make([]string, len(data))
		for i, b := range data {
			members[i] = fmt.Sprintf("%026d", b)
		}
		original := []BatchSelection{{batchRoot, batchProject, members}}
		want, err := NormalizeBatchSelections(original)
		if err != nil {
			t.Fatal(err)
		}
		reversed := slices.Clone(members)
		slices.Reverse(reversed)
		got, err := NormalizeBatchSelections([]BatchSelection{{batchRoot, batchProject, reversed}, {batchRoot, batchProject, members}})
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("permutation or duplicate closure changed operation: %v, %v", got, err)
		}
	})
}

func TestNormalizeBatchSelectionsConflictProperty(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		shared := fmt.Sprintf("%026d", rapid.Uint64().Draw(t, "shared"))
		got, err := NormalizeBatchSelections([]BatchSelection{{batchRoot, batchProject, []string{shared}}, {batchRoot, batchOther, []string{shared}}})
		if !errors.Is(err, ErrBatchDestinationConflict) || got != nil {
			t.Fatalf("shared member accepted with different destinations: %v, %v", got, err)
		}
	})
}
