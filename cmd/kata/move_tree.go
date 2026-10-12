package main

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/issuemove"
	"go.kenn.io/kata/internal/textsafe"
	"go.kenn.io/kata/internal/uid"
	"go.kenn.io/kata/pkg/client/generated"
	"strings"
)

type moveTreeOptions struct {
	DryRun, IssueOnly, MoveSeries                                              bool
	ProviderDisposition, ToDaemon, RequestUID, PreviewDigest, SourceProjectUID string
}
type moveCLISelection struct {
	source        initSetupClient
	target        initSetupClient
	context       context.Context
	targetURL     string
	targetProject projectRef
	from          string
	request       generated.MoveTreeRequest
	preview       *generated.MoveTreePreviewResponse
	result        *generated.MoveTreeResultResponse
}
type moveCLIOutput struct {
	Request   generated.MoveTreeRequest          `json:"request"`
	Preview   *generated.MoveTreePreviewResponse `json:"preview,omitempty"`
	Result    *generated.MoveTreeResultResponse  `json:"result,omitempty"`
	From      string                             `json:"from"`
	ToProject string                             `json:"to_project"`
	DryRun    bool                               `json:"dry_run"`
	Changed   bool                               `json:"changed"`
}

func runMoveSelection(cmd *cobra.Command, refs []string, targetName string, opts moveTreeOptions) error {
	if (opts.RequestUID != "" || opts.PreviewDigest != "" || opts.SourceProjectUID != "") && len(refs) != 1 {
		return fmt.Errorf("exact retry options require one selected root")
	}
	if opts.DryRun && opts.PreviewDigest != "" {
		return fmt.Errorf("--dry cannot reuse an execution preview digest")
	}
	if opts.PreviewDigest != "" && opts.RequestUID == "" {
		return fmt.Errorf("--preview-digest requires --request-uid")
	}
	if opts.RequestUID != "" && !uid.Valid(opts.RequestUID) {
		return fmt.Errorf("request UID must be a ULID")
	}
	if opts.ProviderDisposition != "preserve" && opts.ProviderDisposition != "detach" {
		return fmt.Errorf("provider disposition must be preserve or detach")
	}
	comment, teammate, err := prepareFollowupComment(cmd)
	if err != nil {
		return err
	}
	selections := make([]moveCLISelection, 0, len(refs))
	for _, raw := range refs {
		selection, err := prepareCLIMove(cmd, raw, targetName, opts)
		if err != nil {
			return err
		}
		selections = append(selections, selection)
	}
	if opts.PreviewDigest == "" {
		prepared := make([]issuemove.PreparedMove, len(selections))
		for i, selection := range selections {
			prepared[i] = issuemove.PreparedMove{Request: selection.request, Preview: *selection.preview}
		}
		normalized, err := issuemove.NormalizePreparedMoves(prepared)
		if err != nil {
			return err
		}
		kept := make([]moveCLISelection, 0, len(normalized))
		for _, operation := range normalized {
			for _, selection := range selections {
				if selection.request.RequestUID == operation.Request.RequestUID {
					kept = append(kept, selection)
					break
				}
			}
		}
		selections = kept
	}
	var outputs []moveCLIOutput
	var firstError error
	for i := range selections {
		selection := &selections[i]
		if !opts.DryRun {
			response, callErr := selection.source.api.MoveIssueTreeWithResponse(selection.context, &generated.MoveIssueTreeRequestOptions{Body: &generated.MoveIssueTreeBody{Request: selection.request}})
			if response == nil || (callErr != nil && response.StatusCode < 400) {
				selection.result = &generated.MoveTreeResultResponse{State: "outcome-unknown", RequestUID: selection.request.RequestUID, RetryAction: new("retry with the same request UID and preview digest"), UnknownRoots: []string{selection.request.IssueUID}}
				if firstError == nil {
					firstError = moveCLIIncomplete(*selection)
				}
			} else if err := externalCLIResponseError(response.StatusCode, response.Body, nil); err != nil {
				selection.result = &generated.MoveTreeResultResponse{State: "conflict", RequestUID: selection.request.RequestUID, RetryAction: new("refresh the selection or reconcile retained copies")}
				if firstError == nil {
					firstError = err
				}
			} else if response.JSON200 == nil || response.JSON200.Result == nil {
				selection.result = &generated.MoveTreeResultResponse{State: "outcome-unknown", RequestUID: selection.request.RequestUID, RetryAction: new("retry with the same request UID and preview digest")}
				if firstError == nil {
					firstError = moveCLIIncomplete(*selection)
				}
			} else {
				selection.result = response.JSON200.Result
				if selection.result.State != "completed" {
					if firstError == nil {
						firstError = moveCLIIncomplete(*selection)
					}
				} else if comment != "" {
					shortID := moveCLIRootShortID(*selection)
					if err := postFollowupCommentWithKey(selection.context, selection.target.httpClient, selection.targetURL, selection.targetProject.ID, shortID, selection.request.Actor, comment, teammate, "move-"+selection.request.RequestUID); err != nil && firstError == nil {
						firstError = err
					}
				}
			}
		}
		outputs = append(outputs, moveCLIOutput{Request: selection.request, Preview: selection.preview, Result: selection.result, From: selection.from, ToProject: selection.targetProject.Name, DryRun: opts.DryRun, Changed: selection.result != nil && len(selection.result.Applied) > 0})
	}
	if err := printCLIMoves(cmd, selections, outputs, opts.DryRun); err != nil {
		return err
	}
	return firstError
}

func prepareCLIMove(cmd *cobra.Command, raw, targetName string, opts moveTreeOptions) (moveCLISelection, error) {
	var out moveCLISelection
	var sourceProjectUID, issueUID, sourceName, shortID string
	var pid int64
	var err error
	if opts.SourceProjectUID != "" {
		if !uid.Valid(opts.SourceProjectUID) || !uid.Valid(raw) {
			return out, fmt.Errorf("--source-project-uid requires a full source issue UID and project UID")
		}
		a, err := dialDaemon(cmd.Context())
		if err != nil {
			return out, err
		}
		out.context, out.targetURL = a.ctx, a.baseURL
		sourceProjectUID, issueUID, sourceName, shortID = opts.SourceProjectUID, raw, opts.SourceProjectUID, raw
	} else {
		ctx, baseURL, projectID, ref, resolveErr := resolveIssueRefForCommand(cmd, raw)
		if resolveErr != nil {
			return out, resolveErr
		}
		out.context, out.targetURL, pid, sourceName, shortID = ctx, baseURL, projectID, ref.ProjectName, ref.RefForAPI
	}
	out.source, err = newInitSetupClient(out.context, out.targetURL)
	if err != nil {
		return out, err
	}
	authority, err := out.source.instanceUID(out.context)
	if err != nil {
		return out, err
	}
	if issueUID == "" {
		if uid.Valid(shortID) && opts.PreviewDigest != "" {
			project, err := out.source.projectByName(out.context, sourceName)
			if err != nil {
				return out, err
			}
			issueUID, sourceProjectUID = shortID, project.UID
		} else {
			ref, err := out.source.api.ResolveUIIssueReference(out.context, &generated.ResolveUIIssueReferenceRequestOptions{Query: &generated.ResolveUIIssueReferenceQuery{ProjectID: pid, Ref: shortID}})
			if err != nil {
				return out, err
			}
			issueUID, sourceProjectUID = ref.Issue.UID, ref.Issue.ProjectUID
		}
	}
	out.target = out.source
	destinationAuthority := authority
	if opts.ToDaemon != "" {
		var route initSetupRoute
		out.target, route, err = initSetupSourceClient(out.context, out.targetURL, opts.ToDaemon)
		if err != nil {
			return out, err
		}
		out.targetURL = route.URL
		destinationAuthority, err = out.target.instanceUID(out.context)
		if err != nil {
			return out, err
		}
	}
	projects, err := out.target.api.ListProjects(out.context, &generated.ListProjectsRequestOptions{})
	if err != nil {
		return out, err
	}
	var candidates []projectRef
	for _, project := range projects.Projects {
		candidates = append(candidates, projectRef{ID: project.ID, UID: project.UID, Name: project.Name})
	}
	out.targetProject, err = resolveProjectSelectorFromRefs(targetName, candidates)
	if err != nil {
		return out, err
	}
	if authority == destinationAuthority && sourceProjectUID == out.targetProject.UID {
		return out, fmt.Errorf("issue is already in the destination project")
	}
	requestUID := opts.RequestUID
	if requestUID == "" {
		requestUID, err = uid.New()
		if err != nil {
			return out, err
		}
	}
	actor, _ := resolveActor(out.context, flags.As, nil)
	selection := "subtree"
	if opts.IssueOnly {
		selection = "issue-only"
	}
	out.request = generated.MoveTreeRequest{SourceAuthorityUID: authority, SourceProjectUID: sourceProjectUID, IssueUID: issueUID, DestinationAuthorityUID: destinationAuthority, DestinationProjectUID: out.targetProject.UID, RequestUID: requestUID, ActorAuthorityUID: authority, Actor: actor, Selection: &selection, MoveSeries: &opts.MoveSeries, ProviderDisposition: &opts.ProviderDisposition}
	out.from = moveQualifiedID(sourceName, shortID)
	if opts.PreviewDigest != "" {
		out.request.PreviewDigest = &opts.PreviewDigest
		return out, nil
	}
	response, err := out.source.api.MoveIssueTree(out.context, &generated.MoveIssueTreeRequestOptions{Body: &generated.MoveIssueTreeBody{Request: out.request, DryRun: new(true)}})
	if err != nil {
		return out, err
	}
	if response.Preview == nil {
		return out, fmt.Errorf("daemon returned no complete-tree preview")
	}
	if len(response.Preview.Blockers) > 0 {
		return out, fmt.Errorf("move preview has blockers: %v", response.Preview.Blockers)
	}
	out.preview = response.Preview
	out.request.PreviewDigest = &out.preview.Digest
	return out, nil
}

func moveCLIIncomplete(selection moveCLISelection) error {
	data, _ := json.Marshal(map[string]any{"request": selection.request, "result": selection.result})
	hint := "retry using the full issue UID, --request-uid, --preview-digest and --source-project-uid from this request"
	if selection.result.RetryAction != nil {
		hint = *selection.result.RetryAction + "; " + hint
	}
	return &cliError{Message: "move is " + selection.result.State, Code: "issue_move_" + selection.result.State, Kind: kindConflict, ExitCode: ExitConflict, Hint: hint, Data: data}
}
func moveCLIRootShortID(selection moveCLISelection) string {
	if selection.result != nil {
		for _, applied := range selection.result.Applied {
			if applied.IssueUID == selection.request.IssueUID {
				return applied.ShortID
			}
		}
	}
	return ""
}
func printCLIMoves(cmd *cobra.Command, selections []moveCLISelection, outputs []moveCLIOutput, dry bool) error {
	if currentOutputMode() == outputJSON {
		if len(outputs) == 1 {
			return emitJSON(cmd.OutOrStdout(), outputs[0])
		}
		return emitJSON(cmd.OutOrStdout(), outputs)
	}
	if flags.Quiet {
		return nil
	}
	for i, selection := range selections {
		if dry {
			if err := printMovePreview(cmd, strings.SplitN(selection.from, "#", 2)[0], strings.SplitN(selection.from, "#", 2)[1], selection.targetProject.Name); err != nil {
				return err
			}
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "members=%d request_uid=%s preview_digest=%s\n", len(selection.preview.Members), selection.request.RequestUID, selection.preview.Digest)
			for _, action := range append(selection.preview.ProviderActions, selection.preview.RecurrenceActions...) {
				if _, actionErr := fmt.Fprintf(cmd.OutOrStdout(), "%s: %s\n", textsafe.Line(action.Kind), textsafe.Line(action.Message)); actionErr != nil {
					return actionErr
				}
			}
			for _, member := range selection.preview.Members {
				if _, memberErr := fmt.Fprintf(cmd.OutOrStdout(), "selected issue_uid=%s source=%s/%s/%s\n", member.IssueUID, member.AuthorityUID, member.ProjectUID, textsafe.Line(member.ShortID)); memberErr != nil {
					return memberErr
				}
			}
			if err != nil {
				return err
			}
		} else {
			result := selection.result
			if currentOutputMode() == outputAgent {
				_, err := fmt.Fprintf(cmd.OutOrStdout(), "move %s state=%s request_uid=%s changed=%t dry_run=false\n", agentValue(selection.from), result.State, result.RequestUID, outputs[i].Changed)
				if err != nil {
					return err
				}
			} else {
				_, err := fmt.Fprintf(cmd.OutOrStdout(), "move %s to %s (%d applied issues); state=%s request_uid=%s\n", textsafe.Line(selection.from), textsafe.Line(moveQualifiedID(selection.targetProject.Name, moveCLIRootShortID(selection))), len(result.Applied), textsafe.Line(result.State), result.RequestUID)
				if err != nil {
					return err
				}
			}
			for _, applied := range result.Applied {
				_, err := fmt.Fprintf(cmd.OutOrStdout(), "issue_uid=%s to=%s event_uid=%s event_hash=%s\n", applied.IssueUID, textsafe.Line(moveQualifiedID(selection.targetProject.Name, applied.ShortID)), optionalMoveValue(applied.CommittedEventUID), optionalMoveValue(applied.CommittedEventHash))
				if err != nil {
					return err
				}
			}
			if result.RetryAction != nil {
				if _, err := fmt.Fprintln(cmd.OutOrStdout(), textsafe.Line(*result.RetryAction)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
func optionalMoveValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
