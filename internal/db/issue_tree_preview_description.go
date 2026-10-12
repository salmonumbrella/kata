package db

import (
	"context"
	"fmt"
)

// PopulateLocalIssueTreePreview describes the already selected owner rows. It
// exports no event history or credentials and persists no preview state.
func PopulateLocalIssueTreePreview(ctx context.Context, store Storage, request MoveTreeRequest, preview *MoveTreePreview) error {
	selected := map[string]bool{}
	projects := map[string]bool{}
	for _, member := range preview.Members {
		selected[member.IssueUID] = true
		projects[member.ProjectUID] = true
	}
	seenLinks := map[int64]bool{}
	for projectUID := range projects {
		project, err := store.ProjectByUID(ctx, projectUID)
		if err != nil {
			return err
		}
		filter := ExportFilter{ProjectID: &project.ID, IncludeDeleted: true}
		portion := IssueTreePortion{AuthorityUID: store.InstanceUID()}
		ids := map[int64]bool{}
		for issue, err := range store.ExportIssues(ctx, filter) {
			if err != nil {
				return err
			}
			if selected[issue.UID] {
				if revision, ok := preview.SourceRevisions[issue.UID]; !ok || revision != issue.Revision {
					return ErrMovePreviewStale
				}
				portion.Issues = append(portion.Issues, issue)
				ids[issue.ID] = true
			}
		}
		for link, err := range store.ExportLinks(ctx, filter) {
			if err != nil {
				return err
			}
			if (selected[link.FromIssueUID] || selected[link.ToIssueUID]) && !seenLinks[link.ID] {
				preview.Relations = append(preview.Relations, link)
				seenLinks[link.ID] = true
			}
		}
		for mapping, err := range store.ExportImportMappings(ctx, filter) {
			if err != nil {
				return err
			}
			if mapping.IssueID != nil && ids[*mapping.IssueID] {
				portion.Mappings = append(portion.Mappings, mapping)
			}
		}
		for binding, err := range store.ExportExternalRootBindings(ctx, filter) {
			if err != nil {
				return err
			}
			if selected[binding.IssueUID] {
				portion.ExternalBindings = append(portion.ExternalBindings, binding)
			}
		}
		AddIssueTreePreviewConsequences(preview, request, portion)
	}
	return nil
}

// AddIssueTreePreviewConsequences uses the actual source portions, including a
// series whose scheduler is on another authority. Actions describe selection,
// not a claim that any destination commit has happened.
func AddIssueTreePreviewConsequences(preview *MoveTreePreview, request MoveTreeRequest, portion IssueTreePortion) {
	providerKind, providerMessage := "preserve-provider", "preserve provider ownership and pending intent at the destination; source polling retains a moved-work suppression"
	if request.ProviderDisposition == "detach" {
		providerKind, providerMessage = "detach-provider", "detach provider ownership at the destination; source polling retains a moved-work suppression"
	}
	issues := map[int64]string{}
	for _, issue := range portion.Issues {
		issues[issue.ID] = issue.UID
		if issue.RecurrenceUID != nil {
			authority := portion.AuthorityUID
			if issue.RecurrenceAuthorityUID != nil {
				authority = *issue.RecurrenceAuthorityUID
			}
			kind, message := "retain-series", "future occurrences remain at the original series authority "+authority
			if request.MoveSeries {
				kind, message = "move-series", "future occurrences move with the complete recurrence family to the destination authority "+request.DestinationAuthorityUID
			}
			preview.RecurrenceActions = append(preview.RecurrenceActions, MoveTreeAction{IssueUID: issue.UID, AuthorityUID: authority, Kind: kind, Message: message})
		}
	}
	for _, mapping := range portion.Mappings {
		if mapping.IssueID != nil && issues[*mapping.IssueID] != "" {
			preview.ProviderActions = append(preview.ProviderActions, MoveTreeAction{IssueUID: issues[*mapping.IssueID], AuthorityUID: portion.AuthorityUID, Kind: providerKind, Message: fmt.Sprintf("%s: %s", mapping.Source, providerMessage)})
		}
	}
	for _, binding := range portion.ExternalBindings {
		preview.ProviderActions = append(preview.ProviderActions, MoveTreeAction{IssueUID: binding.IssueUID, AuthorityUID: portion.AuthorityUID, Kind: providerKind, Message: fmt.Sprintf("%s: %s", binding.ConnectorInstance, providerMessage)})
	}
}
