package tui

import (
	tea "charm.land/bubbletea/v2"
	"context"
	"errors"
	"fmt"
	"go.kenn.io/kata/internal/uid"
	"go.kenn.io/kata/pkg/client/generated"
	"strings"
	"time"
)

type treeMoveAPI interface {
	MoveTree(context.Context, generated.MoveTreeRequest, bool) (*generated.IssueTreeMoveResponseBody, error)
}

func (c *Client) MoveTree(ctx context.Context, request generated.MoveTreeRequest, dry bool) (*generated.IssueTreeMoveResponseBody, error) {
	client, err := c.generatedClient()
	if err != nil {
		return nil, err
	}
	response, callErr := client.MoveIssueTreeWithResponse(ctx, &generated.MoveIssueTreeRequestOptions{Body: &generated.MoveIssueTreeBody{Request: request, DryRun: &dry}})
	if response == nil {
		return nil, callErr
	}
	var out generated.IssueTreeMoveResponseBody
	if err := decodeGeneratedResponse(response.HTTPResponse, response.Body, callErr, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

type treeMoveReply struct {
	connGen   uint64
	formGen   int64
	request   generated.MoveTreeRequest
	preview   *generated.MoveTreePreviewResponse
	result    *generated.MoveTreeResultResponse
	err       error
	executing bool
}

func (m Model) commitTreeMove(value string) (Model, tea.Cmd) {
	if value == "" || m.input.saving {
		return m, nil
	}
	if !m.mutationAllowed(false) {
		m.input.err = "Move unavailable while authority is stale or read-only"
		return m, nil
	}
	service, ok := m.api.(treeMoveAPI)
	if !ok {
		m.input.err = "This daemon client does not support complete-tree moves"
		return m, nil
	}
	connGen, formGen := m.connGen, m.input.formGen
	if m.moveRequest != nil && m.movePreview != nil {
		if value != m.moveInput {
			m.input.err = "Keep the reviewed destination unchanged, or cancel and preview again"
			return m, nil
		}
		request := *m.moveRequest
		m.input.saving = true
		return m, func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			defer cancel()
			response, err := service.MoveTree(ctx, request, false)
			reply := treeMoveReply{connGen: connGen, formGen: formGen, request: request, err: err, executing: true}
			if response != nil {
				reply.result = response.Result
			}
			return reply
		}
	}
	if m.detail.issue == nil || !uid.Valid(m.detail.issue.UID) {
		m.input.err = "Select an issue with a stable UID before moving"
		return m, nil
	}
	issueUID, pid, actor := m.detail.issue.UID, m.detail.scopePID, m.detail.actor
	api := m.api
	m.moveInput = value
	m.input.saving = true
	return m, func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		reply := treeMoveReply{connGen: connGen, formGen: formGen}
		instance, err := api.GetInstance(ctx)
		if err != nil {
			reply.err = err
			return reply
		}
		projects, err := api.ListProjects(ctx)
		if err != nil {
			reply.err = err
			return reply
		}
		var sourceUID string
		for _, project := range projects {
			if project.ID == pid {
				sourceUID = project.UID
			}
		}
		targetName, selection, disposition, series, err := parseTUIMoveDestination(value)
		if err != nil {
			reply.err = err
			return reply
		}
		destinationAuthority, destinationUID := instance.InstanceUID, ""
		if pieces := strings.Split(targetName, "/"); len(pieces) == 2 && uid.Valid(pieces[0]) && uid.Valid(pieces[1]) {
			destinationAuthority, destinationUID = pieces[0], pieces[1]
		} else {
			for _, project := range projects {
				if project.Name == targetName || project.UID == targetName {
					if destinationUID != "" {
						reply.err = errors.New("destination is ambiguous; use its project UID")
						return reply
					}
					destinationUID = project.UID
				}
			}
		}
		if !uid.Valid(sourceUID) || !uid.Valid(destinationUID) {
			reply.err = errors.New("selected source or destination is unavailable")
			return reply
		}
		requestUID, err := uid.New()
		if err != nil {
			reply.err = err
			return reply
		}
		reply.request = generated.MoveTreeRequest{SourceAuthorityUID: instance.InstanceUID, SourceProjectUID: sourceUID, IssueUID: issueUID, DestinationAuthorityUID: destinationAuthority, DestinationProjectUID: destinationUID, RequestUID: requestUID, ActorAuthorityUID: instance.InstanceUID, Actor: actor, Selection: &selection, MoveSeries: &series, ProviderDisposition: &disposition}
		response, err := service.MoveTree(ctx, reply.request, true)
		reply.err = err
		if response != nil {
			reply.preview = response.Preview
		}
		return reply
	}
}
func parseTUIMoveDestination(value string) (string, string, string, bool, error) {
	selection, disposition, series := "subtree", "preserve", false
	var names []string
	for _, part := range strings.Fields(value) {
		switch part {
		case "--issue-only":
			selection = "issue-only"
		case "--move-series":
			series = true
		case "--detach":
			disposition = "detach"
		default:
			if strings.HasPrefix(part, "--") {
				return "", "", "", false, fmt.Errorf("unknown move option %s", part)
			}
			names = append(names, part)
		}
	}
	return strings.Join(names, " "), selection, disposition, series, nil
}
func (m Model) applyTreeMove(reply treeMoveReply) (Model, tea.Cmd) {
	if reply.connGen != m.connGen || reply.formGen != m.input.formGen || m.input.kind != inputMovePrompt {
		return m, nil
	}
	m.input.saving = false
	if reply.err != nil {
		if e, ok := errors.AsType[*APIError](reply.err); ok && e.Status == 401 {
			m.authCapabilitiesReady = false
			m.activeAuthRejected = true
		}
		if reply.executing {
			m.moveResult = &generated.MoveTreeResultResponse{State: "outcome-unknown", RequestUID: reply.request.RequestUID}
			m.input.err = "Move outcome unknown. Enter retries the same request; reconcile copies before changing it. request_uid=" + reply.request.RequestUID
		} else {
			m.input.err = reply.err.Error()
		}
		return m, nil
	}
	if !reply.executing {
		if reply.preview == nil || reply.preview.Digest == "" {
			m.input.err = "Daemon returned no complete-tree preview"
			return m, nil
		}
		if len(reply.preview.Blockers) > 0 {
			m.input.err = fmt.Sprintf("Move blocked: %v", reply.preview.Blockers)
			return m, nil
		}
		request := reply.request
		request.PreviewDigest = &reply.preview.Digest
		m.moveRequest, m.movePreview = &request, reply.preview
		m.input.err = fmt.Sprintf("Preview: %d issues, including completed/deleted children. Enter confirms; Esc cancels. request_uid=%s", len(reply.preview.Members), request.RequestUID)
		for _, action := range append(reply.preview.ProviderActions, reply.preview.RecurrenceActions...) {
			m.input.err += ". " + action.Message
		}
		return m, nil
	}
	if reply.result == nil {
		m.input.err = "Move outcome unknown; Enter retries the same request"
		return m, nil
	}
	m.moveResult = reply.result
	if reply.result.State != "completed" {
		m.input.err = fmt.Sprintf("Move %s: applied=%d finalized=%d unknown=%d. request_uid=%s", reply.result.State, len(reply.result.Applied), len(reply.result.Finalized), len(reply.result.UnknownRoots), reply.result.RequestUID)
		if reply.result.RetryAction != nil {
			m.input.err += ". " + *reply.result.RetryAction
		}
		for _, conflict := range reply.result.Conflicts {
			m.input.err += fmt.Sprintf(". %v", conflict)
		}
		return m, nil
	}
	m.input = inputState{}
	m.toast = &toast{text: fmt.Sprintf("Moved %d issues. request_uid=%s", len(reply.result.Applied), reply.result.RequestUID), level: toastInfo, expiresAt: m.toastNow().Add(5 * time.Second)}
	if m.cache != nil {
		m.cache.markStale()
	}
	return m, tea.Batch(withConnGen(m.list.refetchCmd(m.api, m.scope), m.connGen), toastExpireCmd(5*time.Second))
}
