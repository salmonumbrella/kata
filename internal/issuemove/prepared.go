package issuemove

import (
	"go.kenn.io/kata/pkg/client/generated"
	"slices"
)

// PreparedMove is a reviewed request and its current authorized closure.
// It is transient client data; the daemon validates it again when committing.
type PreparedMove struct {
	Request generated.MoveTreeRequest
	Preview generated.MoveTreePreviewResponse
}

// NormalizePreparedMoves executes each overlapping tree once. A retained copy
// claiming another owner is a conflict, rather than permission to drop a source.
func NormalizePreparedMoves(input []PreparedMove) ([]PreparedMove, error) {
	selections := make([]BatchSelection, len(input))
	owners := map[string]generated.MoveTreeMemberResponse{}
	for i, operation := range input {
		selection := BatchSelection{DestinationAuthorityUID: operation.Request.DestinationAuthorityUID, DestinationProjectUID: operation.Request.DestinationProjectUID}
		for _, member := range operation.Preview.Members {
			if previous, ok := owners[member.IssueUID]; ok && (previous.AuthorityUID != member.AuthorityUID || previous.ProjectUID != member.ProjectUID) {
				return nil, ErrBatchDestinationConflict
			}
			owners[member.IssueUID] = member
			selection.MemberUIDs = append(selection.MemberUIDs, member.IssueUID)
		}
		selections[i] = selection
	}
	normalized, err := NormalizeBatchSelections(selections)
	if err != nil {
		return nil, err
	}
	out := make([]PreparedMove, 0, len(normalized))
	for _, component := range normalized {
		found := false
		for i, operation := range input {
			if operation.Request.DestinationAuthorityUID != component.DestinationAuthorityUID || operation.Request.DestinationProjectUID != component.DestinationProjectUID {
				continue
			}
			covers := true
			for _, member := range component.MemberUIDs {
				if !slices.Contains(selections[i].MemberUIDs, member) {
					covers = false
					break
				}
			}
			if covers {
				out = append(out, operation)
				found = true
				break
			}
		}
		if !found {
			return nil, ErrInvalidBatchSelection
		}
	}
	return out, nil
}
