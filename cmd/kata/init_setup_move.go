package main

import (
	"context"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"slices"

	"go.kenn.io/kata/internal/uid"
	"go.kenn.io/kata/pkg/client/generated"
)

// These are the complete request inputs and expected selection, saved before
// execution. Outcomes and committed receipts remain native daemon facts.
type initSetupKickoffRequest struct {
	Request generated.MoveTreeRequest `json:"request"`
	Members []string                  `json:"members"`
}

func prepareInitKickoffRequest(p *initSetupProgress, opts callInitOpts) (bool, error) {
	if p.Source == nil {
		return false, nil
	}
	if p.KickoffRequest == nil {
		selection, disposition, moveSeries := "subtree", opts.ProviderDisposition, opts.MoveSeries
		if disposition == "" {
			disposition = "preserve"
		}
		p.KickoffRequest = &initSetupKickoffRequest{Request: generated.MoveTreeRequest{SourceAuthorityUID: p.Source.AuthorityUID, SourceProjectUID: p.Source.ProjectUID, IssueUID: p.Source.IssueUID, DestinationAuthorityUID: p.Destination.AuthorityUID, DestinationProjectUID: p.Destination.ProjectUID, RequestUID: p.RequestUID, ActorAuthorityUID: p.Source.AuthorityUID, Actor: p.Actor, Selection: &selection, ProviderDisposition: &disposition, MoveSeries: &moveSeries}}
		return true, nil
	}
	r := &p.KickoffRequest.Request
	moveSeries := r.MoveSeries != nil && *r.MoveSeries
	disposition := "preserve"
	if r.ProviderDisposition != nil {
		disposition = *r.ProviderDisposition
	}
	changeSeries := (opts.MoveSeriesSet || opts.MoveSeries) && opts.MoveSeries != moveSeries
	changeDisposition := opts.ProviderDisposition != "" && opts.ProviderDisposition != disposition
	if !changeSeries && !changeDisposition {
		return false, nil
	}
	if r.PreviewDigest != nil {
		return false, initSetupInputConflict()
	}
	// Preview blockers may require explicit options. Before any execution,
	// those inputs can be corrected while retaining project/request identity.
	if changeSeries {
		v := opts.MoveSeries
		r.MoveSeries = &v
	}
	if changeDisposition {
		v := opts.ProviderDisposition
		r.ProviderDisposition = &v
	}
	return true, nil
}

func validateSavedInitKickoffRequest(p initSetupProgress) error {
	if p.KickoffRequest == nil {
		return nil
	}
	r := p.KickoffRequest.Request
	if p.Source == nil || r.SourceAuthorityUID != p.Source.AuthorityUID || r.SourceProjectUID != p.Source.ProjectUID || r.IssueUID != p.Source.IssueUID || r.DestinationAuthorityUID != p.Destination.AuthorityUID || r.DestinationProjectUID != p.Destination.ProjectUID || r.RequestUID != p.RequestUID || r.Actor != p.Actor || r.ActorAuthorityUID != p.Source.AuthorityUID || r.Selection == nil || *r.Selection != "subtree" || r.ProviderDisposition == nil || (*r.ProviderDisposition != "preserve" && *r.ProviderDisposition != "detach") {
		return fmt.Errorf("saved kickoff request differs from setup identity; preserve it for reconciliation")
	}
	if r.PreviewDigest == nil {
		if len(p.KickoffRequest.Members) != 0 {
			return fmt.Errorf("saved kickoff selection has no preview")
		}
		return nil
	}
	seen := make(map[string]bool)
	for _, member := range p.KickoffRequest.Members {
		if !uid.Valid(member) || seen[member] {
			return fmt.Errorf("saved kickoff selection is invalid")
		}
		seen[member] = true
	}
	if *r.PreviewDigest == "" || !seen[p.Source.IssueUID] {
		return fmt.Errorf("saved kickoff selection is incomplete")
	}
	return nil
}

func runInitSetupKickoff(ctx context.Context, baseURL, path string, p initSetupProgress, out string) (string, error) {
	source, _, err := initSetupSourceClient(ctx, baseURL, p.Source.Route.Daemon)
	if err != nil {
		return out, err
	}
	if p.KickoffRequest.Request.PreviewDigest == nil {
		request := p.KickoffRequest.Request
		dryRun := true
		response, callErr := source.api.MoveIssueTreeWithResponse(ctx, &generated.MoveIssueTreeRequestOptions{Body: &generated.IssueTreeMoveRequestBody{Request: request, DryRun: &dryRun}})
		if response == nil {
			return out, externalCLITransportError(response, callErr)
		}
		if err := externalCLIResponseError(response.StatusCode, response.Body, callErr); err != nil {
			return out, err
		}
		if response.JSON200 == nil || response.JSON200.Preview == nil {
			return out, fmt.Errorf("source daemon returned no kickoff preview")
		}
		preview := response.JSON200.Preview
		if len(preview.Blockers) != 0 {
			return out, initSetupKickoffPending(p, "conflict", preview, "resolve the native preview blockers before retrying kickoff")
		}
		members := make([]string, 0, len(preview.Members))
		for _, member := range preview.Members {
			if !uid.Valid(member.IssueUID) || slices.Contains(members, member.IssueUID) {
				return out, fmt.Errorf("source daemon returned an invalid kickoff selection")
			}
			members = append(members, member.IssueUID)
		}
		if preview.Digest == "" || !slices.Contains(members, p.Source.IssueUID) {
			return out, fmt.Errorf("source daemon returned an incomplete kickoff preview")
		}
		request.PreviewDigest = &preview.Digest
		p.KickoffRequest = &initSetupKickoffRequest{Request: request, Members: members}
		if err := saveInitSetupProgress(path, p); err != nil {
			return out, fmt.Errorf("save complete kickoff request before execution: %w", err)
		}
	}
	response, callErr := source.api.MoveIssueTreeWithResponse(ctx, &generated.MoveIssueTreeRequestOptions{Body: &generated.IssueTreeMoveRequestBody{Request: p.KickoffRequest.Request}})
	if response == nil {
		return out, initSetupKickoffPending(p, "outcome-unknown", nil, "retry kata init with the saved complete request to resolve the actual committed outcome")
	}
	if err := externalCLIResponseError(response.StatusCode, response.Body, callErr); err != nil {
		return out, err
	}
	if response.JSON200 == nil || response.JSON200.Result == nil {
		return out, initSetupKickoffPending(p, "outcome-unknown", nil, "retry the saved complete kickoff request; the daemon returned no result")
	}
	result := response.JSON200.Result
	if result.State != "completed" {
		hint := "retry the same saved request or reconcile the native move conflicts"
		if result.RetryAction != nil && *result.RetryAction != "" {
			hint = *result.RetryAction
		}
		return out, initSetupKickoffPending(p, result.State, result, hint)
	}
	if err := validateInitSetupKickoffResult(p, result); err != nil {
		return out, initSetupKickoffPending(p, "outcome-unknown", result, err.Error()+"; preserve the saved request for reconciliation")
	}
	return formatInitSetupKickoffOutput(out, result)
}

func validateInitSetupKickoffResult(p initSetupProgress, result *generated.MoveTreeResultResponse) error {
	if result.RequestUID != p.RequestUID || len(result.Conflicts) != 0 || len(result.UnknownRoots) != 0 || len(result.Applied) != len(p.KickoffRequest.Members) || len(result.Finalized) != len(p.KickoffRequest.Members) {
		return fmt.Errorf("kickoff completion does not match the saved complete request")
	}
	seen := make(map[string]bool)
	for _, member := range result.Applied {
		if seen[member.IssueUID] || !slices.Contains(p.KickoffRequest.Members, member.IssueUID) || !slices.Contains(result.Finalized, member.IssueUID) || member.AuthorityUID != p.Destination.AuthorityUID || member.ProjectUID != p.Destination.ProjectUID || member.CommittedEventUID == nil || !uid.Valid(*member.CommittedEventUID) || member.CommittedEventHash == nil || len(*member.CommittedEventHash) != 64 {
			return fmt.Errorf("kickoff completion lacks exact committed evidence for the selected tree")
		}
		if _, err := hex.DecodeString(*member.CommittedEventHash); err != nil {
			return fmt.Errorf("kickoff completion has an invalid committed event hash")
		}
		seen[member.IssueUID] = true
	}
	return nil
}

func initSetupKickoffPending(p initSetupProgress, state string, result any, hint string) error {
	data, _ := json.Marshal(map[string]any{"setup_state": "bound", "kickoff_state": state, "request_uid": p.RequestUID, "issue_uid": p.Source.IssueUID, "destination_authority_uid": p.Destination.AuthorityUID, "destination_project_uid": p.Destination.ProjectUID, "move": result})
	return &cliError{Message: fmt.Sprintf("workspace is bound to %s; kickoff move is %s", p.Destination.Name, state), Hint: hint, Code: "kickoff_move_" + state, Kind: kindConflict, ExitCode: ExitConflict, Data: data}
}

func formatInitSetupKickoffOutput(out string, result *generated.MoveTreeResultResponse) (string, error) {
	if currentOutputMode() == outputJSON {
		var response map[string]jsontext.Value
		if err := json.Unmarshal([]byte(out), &response); err != nil {
			return "", err
		}
		kickoff, err := json.Marshal(result)
		if err != nil {
			return "", err
		}
		response["kickoff"] = kickoff
		bs, err := json.Marshal(response, json.Deterministic(true))
		return string(append(bs, '\n')), err
	}
	if currentOutputMode() == outputAgent {
		return out + fmt.Sprintf("kickoff_state=completed request_uid=%s members=%d\n", result.RequestUID, len(result.Applied)), nil
	}
	return out + fmt.Sprintf("moved kickoff tree (%d issues)\n", len(result.Applied)), nil
}
