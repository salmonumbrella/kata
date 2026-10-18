package mcpserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"net/http"
	"sort"
	"sync"

	"github.com/oklog/ulid/v2"
	"go.kenn.io/kata/internal/mcpevents"
	"go.kenn.io/kata/internal/notification"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

type inboxEventsSource struct {
	client      *kataclient.Client
	scope       *Scope
	daemonUID   string
	principalMu sync.Mutex
	principal   string
}

// NewEventsSource reads only through the selected authenticated daemon client.
// Every response is bounded before generated decoding, including catalog and
// issue authorization reads; no database or owner-local authority is assumed.
func NewEventsSource(client *kataclient.Client, scope *Scope, daemonUID string, expectedAuth ...generated.AuthInfoOut) mcpevents.Source {
	if client != nil {
		client = client.WithResponseLimit(8 << 20)
	}
	source := &inboxEventsSource{client: client, scope: scope, daemonUID: daemonUID}
	if len(expectedAuth) > 0 {
		source.principal = EventsPrincipalFingerprint(expectedAuth[0])
	}
	return source
}

// EventsPrincipalFingerprint binds effective authority, excluding display and
// lifetime flags. Callers use the same value for durable backend identity.
func EventsPrincipalFingerprint(auth generated.AuthInfoOut) string {
	actions := append([]string(nil), auth.AllowedActions...)
	sort.Strings(actions)
	b, _ := json.Marshal(struct {
		Kind           string                   `json:"kind"`
		Actor          *string                  `json:"actor"`
		Scope          *generated.TokenScopeOut `json:"scope"`
		AllowedActions []string                 `json:"allowed_actions"`
	}{auth.Kind, auth.Actor, auth.Scope, actions})
	hash := sha256.Sum256(b)
	return hex.EncodeToString(hash[:])
}

func (s *inboxEventsSource) principalMatches(auth generated.AuthInfoOut) bool {
	s.principalMu.Lock()
	defer s.principalMu.Unlock()
	if auth.Kind == "" {
		return false
	}
	fingerprint := EventsPrincipalFingerprint(auth)
	if s.principal == "" {
		s.principal = fingerprint
	}
	return s.principal == fingerprint
}

func (s *inboxEventsSource) Resolve(ctx context.Context, args mcpevents.Arguments) (mcpevents.Scope, error) {
	result := mcpevents.Scope{}
	if s.client == nil || s.scope == nil || s.daemonUID == "" {
		return result, mcpevents.ErrUnavailable
	}
	var err error
	args.For, err = notification.NormalizeRecipient(args.For)
	if err != nil || (args.IssueUID != "" && args.ProjectUID == "") {
		return result, mcpevents.ErrRevoked
	}
	for _, field := range []*string{&args.ProjectUID, &args.IssueUID} {
		if *field == "" {
			continue
		}
		uid, err := ulid.ParseStrict(*field)
		if err != nil {
			return result, mcpevents.ErrRevoked
		}
		*field = uid.String()
	}
	instance, err := s.client.Instance(ctx)
	if err != nil {
		return result, sourceFailure(err)
	}
	if instance.InstanceUID != s.daemonUID || !s.principalMatches(instance.Auth) {
		return result, mcpevents.ErrRevoked
	}
	catalog, err := s.client.ListProjects(ctx, &generated.ListProjectsRequestOptions{})
	if err != nil {
		return result, sourceFailure(err)
	}
	allowed := map[string]bool{}
	for _, p := range s.scope.projects {
		if p.UID == "" {
			return result, mcpevents.ErrRevoked
		}
		allowed[p.UID] = true
	}
	result.Arguments = args
	result.Dynamic = args.ProjectUID == "" && s.scope.Mode() != ScopeBound
	result.Projects = []mcpevents.Project{}
	seen := map[string]bool{}
	for _, p := range catalog.Projects {
		if s.scope.Mode() != ScopeAll && !allowed[p.UID] {
			continue
		}
		if args.ProjectUID != "" && p.UID != args.ProjectUID {
			continue
		}
		uid, parseErr := ulid.ParseStrict(p.UID)
		if parseErr != nil || uid.String() != p.UID || p.ID <= 0 || seen[p.UID] {
			return mcpevents.Scope{}, mcpevents.ErrUnavailable
		}
		seen[p.UID] = true
		result.Projects = append(result.Projects, mcpevents.Project{UID: p.UID, ID: p.ID, Name: p.Name})
		if len(result.Projects) > mcpevents.MaxProjects {
			return mcpevents.Scope{}, mcpevents.ErrResourceLimit
		}
	}
	if len(result.Projects) == 0 && !result.Dynamic {
		return result, mcpevents.ErrRevoked
	}
	sort.Slice(result.Projects, func(i, j int) bool { return result.Projects[i].UID < result.Projects[j].UID })
	if args.IssueUID != "" {
		shown, err := s.client.ShowIssueByUID(ctx, &generated.ShowIssueByUIDRequestOptions{PathParams: &generated.ShowIssueByUIDPath{UID: args.IssueUID}})
		if err != nil {
			return result, sourceFailure(err)
		}
		if len(result.Projects) != 1 || shown.Issue.UID != args.IssueUID || shown.Issue.ProjectID != result.Projects[0].ID {
			return result, mcpevents.ErrRevoked
		}
		if shown.Issue.ProjectUID != nil && *shown.Issue.ProjectUID != result.Projects[0].UID {
			return result, mcpevents.ErrRevoked
		}
	}
	return result, nil
}

func (s *inboxEventsSource) current(ctx context.Context, pinned mcpevents.Scope) (mcpevents.Scope, error) {
	fresh, err := s.Resolve(ctx, pinned.Arguments)
	if err != nil {
		return fresh, err
	}
	if len(pinned.Projects) != len(fresh.Projects) {
		if !pinned.Dynamic {
			return fresh, mcpevents.ErrRevoked
		}
		return fresh, mcpevents.ErrScopeChanged
	}
	byUID := map[string]int64{}
	for _, p := range pinned.Projects {
		byUID[p.UID] = p.ID
	}
	for _, p := range fresh.Projects {
		if byUID[p.UID] != p.ID {
			if !pinned.Dynamic {
				return fresh, mcpevents.ErrRevoked
			}
			return fresh, mcpevents.ErrScopeChanged
		}
	}
	return fresh, nil
}

func (s *inboxEventsSource) Authorize(ctx context.Context, pinned mcpevents.Scope) error {
	_, err := s.current(ctx, pinned)
	return err
}

func (s *inboxEventsSource) Head(ctx context.Context, pinned mcpevents.Scope) (mcpevents.Position, error) {
	scope, err := s.current(ctx, pinned)
	if err != nil {
		return nil, err
	}
	positions := mcpevents.Position{}
	for _, p := range scope.Projects {
		head, err := s.client.GetProjectEventHead(ctx, &generated.GetProjectEventHeadRequestOptions{PathParams: &generated.GetProjectEventHeadPath{ProjectID: p.ID}})
		if err != nil {
			return nil, s.projectFailure(ctx, scope, err)
		}
		if head.InstanceUID != s.daemonUID || head.AfterID < 0 {
			return nil, mcpevents.ErrRevoked
		}
		positions[p.UID] = head.AfterID
	}
	return positions, nil
}

type inboxSourceCandidate struct {
	event      mcpevents.SourceEvent
	sequence   int64
	projectUID string
}

// Read makes one bounded round across the pinned projects. Matching rows are
// merged by committed log sequence, with an independent safe cursor for each
// project. The merged limit never advances beyond an unreturned occurrence.
func (s *inboxEventsSource) Read(ctx context.Context, pinned mcpevents.Scope, after mcpevents.Position, limit int) (mcpevents.SourcePage, error) {
	scope, err := s.current(ctx, pinned)
	if err != nil {
		return mcpevents.SourcePage{}, err
	}
	if limit < 1 || limit > 100 || len(after) != len(scope.Projects) {
		return mcpevents.SourcePage{}, mcpevents.ErrUnavailable
	}
	safe := mcpevents.Position{}
	end := mcpevents.Position{}
	remaining := map[string]int{}
	candidates := []inboxSourceCandidate{}
	allowed := map[string]bool{}
	for _, p := range scope.Projects {
		allowed[p.UID] = true
	}
	for _, p := range scope.Projects {
		cursor, present := after[p.UID]
		if !present || cursor < 0 {
			return mcpevents.SourcePage{}, mcpevents.ErrUnavailable
		}
		safe[p.UID] = cursor
		page, err := s.projectPage(ctx, p, cursor, limit)
		if errors.Is(err, kataclient.ErrResponseTooLarge) || (err == nil && page.ResetRequired) {
			head, headErr := s.Head(ctx, scope)
			if headErr != nil {
				return mcpevents.SourcePage{}, headErr
			}
			return mcpevents.SourcePage{Positions: head, Reset: true}, nil
		}
		if err != nil {
			return mcpevents.SourcePage{}, s.projectFailure(ctx, scope, err)
		}
		if len(page.Events) > limit {
			return mcpevents.SourcePage{}, mcpevents.ErrUnavailable
		}
		for _, row := range page.Events {
			if row.EventID <= cursor || row.EventUID == "" || row.CreatedAt.IsZero() {
				return mcpevents.SourcePage{}, mcpevents.ErrUnavailable
			}
			visibleProject := row.ProjectUID
			hideIssue := false
			if row.ProjectUID != p.UID {
				var move struct {
					From string `json:"from_project_uid"`
					To   string `json:"to_project_uid"`
				}
				if row.Type != "issue.moved" || json.Unmarshal(row.Payload, &move) != nil || move.From != p.UID || move.To != row.ProjectUID {
					return mcpevents.SourcePage{}, mcpevents.ErrUnavailable
				}
				if !allowed[row.ProjectUID] {
					visibleProject = p.UID
					hideIssue = true
				}
			}
			if _, err := ulid.ParseStrict(row.EventUID); err != nil {
				return mcpevents.SourcePage{}, mcpevents.ErrUnavailable
			}
			cursor = row.EventID
			reason := inboxEventReason(row, scope.Arguments)
			if reason == "" {
				if remaining[p.UID] == 0 {
					safe[p.UID] = cursor
				}
				continue
			}
			issueUID := ""
			if row.IssueUID != nil {
				issueUID = *row.IssueUID
			}
			if issueUID != "" {
				uid, err := ulid.ParseStrict(issueUID)
				if err != nil || uid.String() != issueUID {
					return mcpevents.SourcePage{}, mcpevents.ErrUnavailable
				}
			}
			if hideIssue {
				issueUID = ""
			}
			candidates = append(candidates, inboxSourceCandidate{sequence: row.EventID, projectUID: p.UID, event: mcpevents.SourceEvent{EventID: inboxSourceEventID(s.daemonUID, row.EventUID, scope.Arguments.For), SourceEventUID: row.EventUID, ProjectUID: visibleProject, IssueUID: issueUID, Reason: reason, Timestamp: row.CreatedAt}})
			remaining[p.UID]++
		}
		if page.NextAfterID < cursor {
			return mcpevents.SourcePage{}, mcpevents.ErrUnavailable
		}
		// next_after_id also accounts for hidden rows inspected by the daemon's
		// authorized projection; it is safe only after this whole page is read.
		end[p.UID] = page.NextAfterID
		if remaining[p.UID] == 0 {
			safe[p.UID] = page.NextAfterID
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].sequence < candidates[j].sequence })
	result := mcpevents.SourcePage{Events: []mcpevents.SourceEvent{}}
	for i := 0; i < len(candidates) && len(result.Events) < limit; {
		c := candidates[i]
		// A move is present in both departure and arrival feeds. One occurrence
		// advances only the project copies actually inspected in this round.
		for i < len(candidates) && candidates[i].sequence == c.sequence && candidates[i].event.EventID == c.event.EventID {
			copy := candidates[i]
			safe[copy.projectUID] = copy.sequence
			remaining[copy.projectUID]--
			if remaining[copy.projectUID] == 0 {
				safe[copy.projectUID] = end[copy.projectUID]
			}
			i++
		}
		c.event.Positions = copySourcePosition(safe)
		result.Events = append(result.Events, c.event)
	}
	result.Positions = copySourcePosition(safe)
	return result, nil
}

// A project can disappear after current's catalog read. One fresh authority
// check distinguishes aggregate membership changes from principal revocation;
// an isolated rejection with an unchanged catalog only pauses delivery.
func (s *inboxEventsSource) projectFailure(ctx context.Context, scope mcpevents.Scope, err error) error {
	failure := sourceFailure(err)
	if !scope.Dynamic || !errors.Is(failure, mcpevents.ErrRevoked) {
		return failure
	}
	if _, recheckErr := s.current(ctx, scope); recheckErr != nil {
		return recheckErr
	}
	return mcpevents.ErrUnavailable
}

func (s *inboxEventsSource) projectPage(ctx context.Context, p mcpevents.Project, after int64, limit int) (*generated.PollEventsBody, error) {
	for {
		n := int64(limit)
		page, err := s.client.PollProjectEvents(ctx, &generated.PollProjectEventsRequestOptions{PathParams: &generated.PollProjectEventsPath{ProjectID: p.ID}, Query: &generated.PollProjectEventsQuery{AfterID: &after, Limit: &n}})
		var tooLarge *kataclient.ResponseTooLargeError
		if errors.As(err, &tooLarge) && (tooLarge.StatusCode < 200 || tooLarge.StatusCode >= 300) {
			return nil, sourceFailure(err)
		}
		if !errors.Is(err, kataclient.ErrResponseTooLarge) || limit == 1 {
			return page, err
		}
		limit = max(1, limit/2)
	}
}

func copySourcePosition(position mcpevents.Position) mcpevents.Position {
	copy := make(mcpevents.Position, len(position))
	for uid, n := range position {
		copy[uid] = n
	}
	return copy
}

func sourceFailure(err error) error {
	if errors.Is(err, mcpevents.ErrRevoked) {
		return mcpevents.ErrRevoked
	}
	var tooLarge *kataclient.ResponseTooLargeError
	if errors.As(err, &tooLarge) && (tooLarge.StatusCode == 401 || tooLarge.StatusCode == 403 || tooLarge.StatusCode == 404) {
		return mcpevents.ErrRevoked
	}
	if isHTTPStatus(err, http.StatusUnauthorized) || isHTTPStatus(err, http.StatusForbidden) || isHTTPStatus(err, http.StatusNotFound) {
		return mcpevents.ErrRevoked
	}
	return mcpevents.ErrUnavailable
}
