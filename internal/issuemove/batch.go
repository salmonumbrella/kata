// Package issuemove coordinates complete issue selections between authorities.
package issuemove

import (
	"cmp"
	"errors"
	"slices"
	"strings"

	"go.kenn.io/kata/internal/uid"
)

var (
	ErrInvalidBatchSelection    = errors.New("move batch requires stable destination and member UIDs")
	ErrBatchDestinationConflict = errors.New("move batch assigns an overlapping selection to different destinations")
)

// BatchSelection contains a fully expanded, authorized closure. Membership must
// include completed and deleted descendants before normalization is called.
type BatchSelection struct {
	DestinationAuthorityUID string
	DestinationProjectUID   string
	MemberUIDs              []string
}

// NormalizeBatchSelections merges overlapping closures into indivisible trees.
// Disjoint closures remain independent operations, even at the same destination.
// It neither discovers descendants nor establishes permission to move them.
func NormalizeBatchSelections(input []BatchSelection) ([]BatchSelection, error) {
	parents := make([]int, len(input))
	sizes := make([]int, len(input))
	selections := make([]BatchSelection, len(input))
	owners := make(map[string]int)
	for i, selection := range input {
		parents[i], sizes[i] = i, 1
		selection.DestinationAuthorityUID = strings.ToUpper(selection.DestinationAuthorityUID)
		selection.DestinationProjectUID = strings.ToUpper(selection.DestinationProjectUID)
		if !uid.Valid(selection.DestinationAuthorityUID) || !uid.Valid(selection.DestinationProjectUID) || len(selection.MemberUIDs) == 0 {
			return nil, ErrInvalidBatchSelection
		}
		selection.MemberUIDs = slices.Clone(selection.MemberUIDs)
		selections[i] = selection
		for j, member := range selection.MemberUIDs {
			member = strings.ToUpper(member)
			if !uid.Valid(member) {
				return nil, ErrInvalidBatchSelection
			}
			selection.MemberUIDs[j] = member
			previous, exists := owners[member]
			if !exists {
				owners[member] = i
				continue
			}
			other := selections[previous]
			if selection.DestinationAuthorityUID != other.DestinationAuthorityUID || selection.DestinationProjectUID != other.DestinationProjectUID {
				return nil, ErrBatchDestinationConflict
			}
			a, b := batchComponent(parents, i), batchComponent(parents, previous)
			if a != b {
				if sizes[a] < sizes[b] {
					a, b = b, a
				}
				parents[b] = a
				sizes[a] += sizes[b]
			}
		}
	}
	components := make(map[int]*BatchSelection)
	for i, selection := range selections {
		root := batchComponent(parents, i)
		component := components[root]
		if component == nil {
			component = &BatchSelection{DestinationAuthorityUID: selection.DestinationAuthorityUID, DestinationProjectUID: selection.DestinationProjectUID}
			components[root] = component
		}
		component.MemberUIDs = append(component.MemberUIDs, selection.MemberUIDs...)
	}
	var output []BatchSelection
	for _, component := range components {
		slices.Sort(component.MemberUIDs)
		component.MemberUIDs = slices.Compact(component.MemberUIDs)
		output = append(output, *component)
	}
	slices.SortFunc(output, func(a, b BatchSelection) int {
		if c := cmp.Compare(a.DestinationAuthorityUID, b.DestinationAuthorityUID); c != 0 {
			return c
		}
		if c := cmp.Compare(a.DestinationProjectUID, b.DestinationProjectUID); c != 0 {
			return c
		}
		return cmp.Compare(a.MemberUIDs[0], b.MemberUIDs[0])
	})
	return output, nil
}

func batchComponent(parents []int, i int) int {
	for parents[i] != i {
		parents[i] = parents[parents[i]]
		i = parents[i]
	}
	return i
}
