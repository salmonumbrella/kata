package mcpserver

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/pgstore/pgstoretest"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/mcpevents"
	"go.kenn.io/kata/internal/notification"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

func sourceClient(t *testing.T, handle func(http.ResponseWriter, *http.Request)) *kataclient.Client {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/instance" {
			_, _ = w.Write([]byte(`{"instance_uid":"` + sourceDaemon + `","auth":{"kind":"token"}}`))
			return
		}
		handle(w, r)
	}))
	t.Cleanup(s.Close)
	c, err := kataclient.NewWithHTTPClient(s.URL, s.Client())
	require.NoError(t, err)
	return c
}

func sourceCatalog(w http.ResponseWriter) {
	_, _ = w.Write([]byte(`{"projects":[{"id":1,"uid":"` + sourceProject + `","name":"spoke-project","metadata":{},"revision":1,"created_at":"2026-01-01T00:00:00Z"},{"id":2,"uid":"` + sourceOtherProject + `","name":"hub-project","metadata":{},"revision":1,"created_at":"2026-01-01T00:00:00Z"}]}`))
}

func TestCursorMultiProjectPageDoesNotSkip(t *testing.T) {
	key := notification.MetadataKey("example-user")
	client := sourceClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/projects" {
			sourceCatalog(w)
			return
		}
		n := int64(1)
		if strings.Contains(r.URL.Path, "/2/") {
			n = 2
		}
		e := sourceEvent("issue.created", n, `{"metadata":{"`+key+`":{"from":"example-user","message":"work"}}}`)
		if n == 2 {
			e.ProjectID = 2
			e.ProjectUID = sourceOtherProject
		}
		after, _ := strconv.ParseInt(r.URL.Query().Get("after_id"), 10, 64)
		events := []generated.EventEnvelope{}
		if after < n {
			events = append(events, e)
		}
		b, _ := json.Marshal(generated.PollEventsBody{Events: events, NextAfterID: max(n, after)})
		_, _ = w.Write(b)
	})
	source := NewEventsSource(client, NewAllScope(), sourceDaemon)
	scope, err := source.Resolve(t.Context(), mcpevents.Arguments{For: "example-user"})
	require.NoError(t, err)
	page, err := source.Read(t.Context(), scope, mcpevents.Position{sourceProject: 0, sourceOtherProject: 0}, 1)
	require.NoError(t, err)
	require.Len(t, page.Events, 1)
	require.Equal(t, int64(1), page.Positions[sourceProject])
	require.Zero(t, page.Positions[sourceOtherProject])
	firstID := page.Events[0].EventID
	next, err := source.Read(t.Context(), scope, page.Positions, 1)
	require.NoError(t, err)
	require.Len(t, next.Events, 1)
	require.Equal(t, sourceOtherProject, next.Events[0].ProjectUID)
	require.NotEqual(t, firstID, next.Events[0].EventID)
	replay, err := source.Read(t.Context(), scope, mcpevents.Position{sourceProject: 0, sourceOtherProject: 0}, 1)
	require.NoError(t, err)
	require.Equal(t, firstID, replay.Events[0].EventID)
}

func TestProjectRenamePinsUID(t *testing.T) {
	client := sourceClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"projects":[{"id":1,"uid":"` + sourceProject + `","name":"renamed-project"},{"id":2,"uid":"` + sourceOtherProject + `","name":"spoke-project"}]}`))
	})
	scope, err := NewAllowlistScope([]ProjectIdentity{{ID: 1, UID: sourceProject, Name: "spoke-project"}})
	require.NoError(t, err)
	source := NewEventsSource(client, scope, sourceDaemon)
	resolved, err := source.Resolve(t.Context(), mcpevents.Arguments{For: "example-user"})
	require.NoError(t, err)
	require.Len(t, resolved.Projects, 1)
	require.Equal(t, sourceProject, resolved.Projects[0].UID)
	require.Equal(t, "renamed-project", resolved.Projects[0].Name)
	_, err = source.Resolve(t.Context(), mcpevents.Arguments{For: "example-user", ProjectUID: sourceOtherProject})
	require.ErrorIs(t, err, mcpevents.ErrRevoked)
}

func TestDynamicProjectLimitSuspendsWithoutTruncating(t *testing.T) {
	client := sourceClient(t, func(w http.ResponseWriter, r *http.Request) {
		projects := make([]map[string]any, 129)
		for i := range projects {
			projects[i] = map[string]any{"id": i + 1, "uid": fmt.Sprintf("01ARZ3NDEKTSV4RRFFQ%07d", i), "name": fmt.Sprint("project-", i)}
		}
		b, _ := json.Marshal(map[string]any{"projects": projects})
		_, _ = w.Write(b)
	})
	source := NewEventsSource(client, NewAllScope(), sourceDaemon)
	_, err := source.Resolve(t.Context(), mcpevents.Arguments{For: "example-user"})
	require.ErrorIs(t, err, mcpevents.ErrResourceLimit)
}

func TestSourceRawResponseLimitShrinksPage(t *testing.T) {
	var limits []int
	client := sourceClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/projects" {
			sourceCatalog(w)
			return
		}
		n, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		limits = append(limits, n)
		if n > 25 {
			_, _ = w.Write([]byte(strings.Repeat("x", (8<<20)+1)))
			return
		}
		_, _ = w.Write([]byte(`{"events":[],"next_after_id":0}`))
	})
	source := NewEventsSource(client, NewAllScope(), sourceDaemon)
	scope, err := source.Resolve(t.Context(), mcpevents.Arguments{For: "example-user", ProjectUID: sourceProject})
	require.NoError(t, err)
	_, err = source.Read(t.Context(), scope, mcpevents.Position{sourceProject: 0}, 100)
	require.NoError(t, err)
	require.Equal(t, []int{100, 50, 25}, limits)
}

func TestOversizedSingleEventProducesExplicitGap(t *testing.T) {
	for _, reset := range []bool{false, true} {
		t.Run(fmt.Sprint(reset), func(t *testing.T) {
			client := sourceClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/projects" {
					sourceCatalog(w)
					return
				}
				if strings.HasSuffix(r.URL.Path, "/head") {
					_, _ = w.Write([]byte(`{"after_id":77,"instance_uid":"` + sourceDaemon + `"}`))
					return
				}
				if reset {
					_, _ = w.Write([]byte(`{"events":[],"reset_required":true,"reset_after_id":77,"next_after_id":77}`))
					return
				}
				_, _ = w.Write([]byte(strings.Repeat("x", (8<<20)+1)))
			})
			source := NewEventsSource(client, NewAllScope(), sourceDaemon)
			scope, err := source.Resolve(t.Context(), mcpevents.Arguments{For: "example-user", ProjectUID: sourceProject})
			require.NoError(t, err)
			page, err := source.Read(t.Context(), scope, mcpevents.Position{sourceProject: 0}, 1)
			require.NoError(t, err)
			require.True(t, page.Reset)
			require.Equal(t, int64(77), page.Positions[sourceProject])
			require.Empty(t, page.Events)
		})
	}
}

func TestSourceRejectsInvalidRowsWithoutAdvancing(t *testing.T) {
	for _, kind := range []string{"foreign-project", "invalid-event-uid", "over-limit", "backward-cursor"} {
		t.Run(kind, func(t *testing.T) {
			client := sourceClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/projects" {
					sourceCatalog(w)
					return
				}
				e := sourceEvent("issue.closed", 1, `{}`)
				page := generated.PollEventsBody{Events: []generated.EventEnvelope{e}, NextAfterID: 1}
				switch kind {
				case "foreign-project":
					page.Events[0].ProjectUID = sourceOtherProject
				case "invalid-event-uid":
					page.Events[0].EventUID = "invalid"
				case "over-limit":
					e.EventID = 2
					page.Events = append(page.Events, e)
					page.NextAfterID = 2
				case "backward-cursor":
					page.NextAfterID = 0
				}
				b, _ := json.Marshal(page)
				_, _ = w.Write(b)
			})
			source := NewEventsSource(client, NewAllScope(), sourceDaemon)
			scope, err := source.Resolve(t.Context(), mcpevents.Arguments{For: "example-user", ProjectUID: sourceProject})
			require.NoError(t, err)
			page, err := source.Read(t.Context(), scope, mcpevents.Position{sourceProject: 0}, 1)
			require.ErrorIs(t, err, mcpevents.ErrUnavailable)
			require.Empty(t, page.Positions)
		})
	}
}

func TestSourceRevalidatesScopeAndIssue(t *testing.T) {
	mode := "initial"
	client := sourceClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/projects" {
			if mode == "lost" || mode == "empty" {
				_, _ = w.Write([]byte(`{"projects":[]}`))
				return
			}
			if mode == "unavailable" {
				w.WriteHeader(503)
				return
			}
			sourceCatalog(w)
			return
		}
		id := 1
		if mode == "moved" {
			id = 2
		}
		_, _ = fmt.Fprintf(w, `{"issue":{"uid":"%s","project_id":%d},"comments":[],"labels":[],"links":[]}`, sourceIssue, id)
	})
	source := NewEventsSource(client, NewAllScope(), sourceDaemon)
	scope, err := source.Resolve(t.Context(), mcpevents.Arguments{For: "example-user"})
	require.NoError(t, err)
	mode = "lost"
	require.ErrorIs(t, source.Authorize(t.Context(), scope), mcpevents.ErrScopeChanged)
	empty, err := source.Resolve(t.Context(), scope.Arguments)
	require.NoError(t, err)
	require.Empty(t, empty.Projects)
	mode = "initial"
	issueScope, err := source.Resolve(t.Context(), mcpevents.Arguments{For: "example-user", ProjectUID: sourceProject, IssueUID: sourceIssue})
	require.NoError(t, err)
	mode = "moved"
	require.ErrorIs(t, source.Authorize(t.Context(), issueScope), mcpevents.ErrRevoked)
	mode = "unavailable"
	require.ErrorIs(t, source.Authorize(t.Context(), scope), mcpevents.ErrUnavailable)
}

func TestSourceDynamicProjectAccessRace(t *testing.T) {
	for _, operation := range []string{"read", "head"} {
		for _, status := range []int{403, 404} {
			for _, change := range []string{"removed", "unchanged", "catalog-outage", "credential-revoked"} {
				t.Run(fmt.Sprintf("%s/%d/%s", operation, status, change), func(t *testing.T) {
					var rejected atomic.Bool
					var catalogs, requests atomic.Int32
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						w.Header().Set("Content-Type", "application/json")
						switch r.URL.Path {
						case "/api/v1/instance":
							if rejected.Load() && change == "credential-revoked" {
								w.WriteHeader(401)
								return
							}
							_, _ = w.Write([]byte(`{"instance_uid":"` + sourceDaemon + `","auth":{"kind":"token"}}`))
						case "/api/v1/projects":
							catalogs.Add(1)
							if rejected.Load() && change == "catalog-outage" {
								w.WriteHeader(503)
								return
							}
							if rejected.Load() && change == "removed" {
								_, _ = w.Write([]byte(`{"projects":[{"id":1,"uid":"` + sourceProject + `","name":"spoke-project"}]}`))
								return
							}
							sourceCatalog(w)
						default:
							requests.Add(1)
							if strings.Contains(r.URL.Path, "/2/") {
								rejected.Store(true)
								w.WriteHeader(status)
								return
							}
							if operation == "head" {
								_, _ = w.Write([]byte(`{"after_id":1,"instance_uid":"` + sourceDaemon + `"}`))
								return
							}
							b, _ := json.Marshal(generated.PollEventsBody{Events: []generated.EventEnvelope{sourceEvent("issue.closed", 1, `{}`)}, NextAfterID: 1})
							_, _ = w.Write(b)
						}
					}))
					t.Cleanup(server.Close)
					client, err := kataclient.NewWithHTTPClient(server.URL, server.Client())
					require.NoError(t, err)
					source := NewEventsSource(client, NewAllScope(), sourceDaemon)
					scope, err := source.Resolve(t.Context(), mcpevents.Arguments{For: "example-user"})
					require.NoError(t, err)
					positions := mcpevents.Position{sourceProject: 0, sourceOtherProject: 0}
					if operation == "head" {
						var got mcpevents.Position
						got, err = source.Head(t.Context(), scope)
						require.Empty(t, got)
					} else {
						var page mcpevents.SourcePage
						page, err = source.Read(t.Context(), scope, positions, 100)
						require.Empty(t, page.Positions)
						require.Empty(t, page.Events)
					}
					want := mcpevents.ErrUnavailable
					if change == "removed" {
						want = mcpevents.ErrScopeChanged
					}
					if change == "credential-revoked" {
						want = mcpevents.ErrRevoked
					}
					require.ErrorIs(t, err, want)
					require.Equal(t, mcpevents.Position{sourceProject: 0, sourceOtherProject: 0}, positions)
					wantCatalogs := int32(3)
					if change == "credential-revoked" {
						wantCatalogs = 2
					}
					require.Equal(t, wantCatalogs, catalogs.Load(), "at most one authorization refresh after rejection")
					require.Equal(t, int32(2), requests.Load(), "never retry the rejected project in this operation")
					if change == "removed" {
						remaining, resolveErr := source.Resolve(t.Context(), scope.Arguments)
						require.NoError(t, resolveErr)
						require.Len(t, remaining.Projects, 1)
						if operation == "head" {
							got, err := source.Head(t.Context(), remaining)
							require.NoError(t, err)
							require.Equal(t, mcpevents.Position{sourceProject: 1}, got)
						} else {
							page, err := source.Read(t.Context(), remaining, mcpevents.Position{sourceProject: 0}, 100)
							require.NoError(t, err)
							require.Len(t, page.Events, 1)
							require.Equal(t, sourceProject, page.Events[0].ProjectUID)
						}
					}
				})
			}
		}
	}
}

func TestSourceBackendCommittedHeadAndNotification(t *testing.T) {
	for _, backend := range sourceBackendFixtures() {
		t.Run(backend.Name, func(t *testing.T) {
			store := backend.Open(t)
			project, err := store.CreateProject(t.Context(), "spoke-project")
			require.NoError(t, err)
			daemonServer := daemon.NewServer(daemon.ServerConfig{DB: store, StartedAt: time.Now().UTC()})
			t.Cleanup(func() { _ = daemonServer.Close() })
			server := httptest.NewServer(daemonServer.Handler())
			t.Cleanup(server.Close)
			client, err := kataclient.NewWithHTTPClient(server.URL, server.Client())
			require.NoError(t, err)
			startup, err := NewBoundScope(ProjectIdentity{ID: project.ID, UID: project.UID, Name: project.Name})
			require.NoError(t, err)
			source := NewEventsSource(client, startup, store.InstanceUID())
			scope, err := source.Resolve(t.Context(), mcpevents.Arguments{For: "example-user"})
			require.NoError(t, err)
			head, err := source.Head(t.Context(), scope)
			require.NoError(t, err)
			issue, event, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Requested work", Author: "example-user", Metadata: map[string]jsontext.Value{notification.MetadataKey("example-user"): jsontext.Value(`{"from":"sender","message":"work"}`)}})
			require.NoError(t, err)
			page, err := source.Read(t.Context(), scope, head, 100)
			require.NoError(t, err)
			require.Len(t, page.Events, 1)
			require.Equal(t, "notification", page.Events[0].Reason)
			require.Equal(t, issue.UID, page.Events[0].IssueUID)
			require.Equal(t, event.UID, page.Events[0].SourceEventUID)
			require.Equal(t, event.ID, page.Positions[project.UID])
			next, err := source.Read(t.Context(), scope, page.Positions, 100)
			require.NoError(t, err)
			require.Empty(t, next.Events)
		})
	}
}

func TestSourceMoveDeparturesConformance(t *testing.T) {
	for _, backend := range sourceBackendFixtures() {
		t.Run(backend.Name, func(t *testing.T) {
			for _, both := range []bool{false, true} {
				t.Run(fmt.Sprintf("both-projects-%t", both), func(t *testing.T) {
					store := backend.Open(t)
					a, err := store.CreateProject(t.Context(), "spoke-project")
					require.NoError(t, err)
					b, err := store.CreateProject(t.Context(), "hub-project")
					require.NoError(t, err)
					issue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: a.ID, Title: "Moved issue", Author: "example-user"})
					require.NoError(t, err)
					daemonServer := daemon.NewServer(daemon.ServerConfig{DB: store, StartedAt: time.Now().UTC()})
					t.Cleanup(func() { _ = daemonServer.Close() })
					server := httptest.NewServer(daemonServer.Handler())
					t.Cleanup(server.Close)
					client, err := kataclient.NewWithHTTPClient(server.URL, server.Client())
					require.NoError(t, err)
					startup, err := NewBoundScope(ProjectIdentity{ID: a.ID, UID: a.UID, Name: a.Name})
					require.NoError(t, err)
					if both {
						startup, err = NewAllowlistScope([]ProjectIdentity{{ID: a.ID, UID: a.UID, Name: a.Name}, {ID: b.ID, UID: b.UID, Name: b.Name}})
						require.NoError(t, err)
					}
					source := NewEventsSource(client, startup, store.InstanceUID())
					scope, err := source.Resolve(t.Context(), mcpevents.Arguments{For: "example-user"})
					require.NoError(t, err)
					head, err := source.Head(t.Context(), scope)
					require.NoError(t, err)
					_, earlier, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: b.ID, Title: "Earlier destination work", Author: "example-user", Metadata: map[string]jsontext.Value{notification.MetadataKey("example-user"): jsontext.Value(`{"from":"sender","message":"work"}`)}})
					require.NoError(t, err)
					moved, err := store.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: a.ID, ToProjectID: b.ID, IfMatchRev: issue.Revision, Actor: "example-user"})
					require.NoError(t, err)
					require.NotNil(t, moved.Event)
					pending := mcpevents.Payload{Recipient: "example-user", Reason: "notification", ProjectUID: a.UID, IssueUID: issue.UID}
					guardErr := source.(*inboxEventsSource).AuthorizeOccurrence(t.Context(), scope, pending)
					if both {
						require.NoError(t, guardErr)
					} else {
						require.ErrorIs(t, guardErr, mcpevents.ErrOccurrenceHidden)
					}
					page, err := source.Read(t.Context(), scope, head, 1)
					require.NoError(t, err)
					require.Len(t, page.Events, 1)
					if both {
						require.Equal(t, earlier.UID, page.Events[0].SourceEventUID)
						require.Equal(t, head[a.UID], page.Positions[a.UID], "unreturned departure cannot advance the source cursor")
						page, err = source.Read(t.Context(), scope, page.Positions, 1)
						require.NoError(t, err)
						require.Len(t, page.Events, 1, "two project copies form one occurrence")
						require.Equal(t, b.UID, page.Events[0].ProjectUID)
						require.Equal(t, issue.UID, page.Events[0].IssueUID)
						require.Equal(t, moved.EventID, page.Positions[b.UID])
					} else {
						require.Equal(t, a.UID, page.Events[0].ProjectUID)
						require.Empty(t, page.Events[0].IssueUID)
						encoded, err := json.Marshal(page.Events)
						require.NoError(t, err)
						require.NotContains(t, string(encoded), b.UID)
						require.NotContains(t, string(encoded), issue.UID)
					}
					require.Equal(t, moved.Event.UID, page.Events[0].SourceEventUID)
					require.Equal(t, "visibility", page.Events[0].Reason)
					require.Equal(t, moved.EventID, page.Positions[a.UID])
					again, err := source.Read(t.Context(), scope, page.Positions, 1)
					require.NoError(t, err)
					require.Empty(t, again.Events)
				})
			}
		})
	}
}

func TestSourceOccurrenceRevalidatesIssueAccess(t *testing.T) {
	for _, mode := range []string{"visible", "detached", "forbidden", "show-outage", "credential-revoked", "refresh-outage", "foreign-project", "generic-resync"} {
		t.Run(mode, func(t *testing.T) {
			var active, denied atomic.Bool
			var catalogs, shows atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/v1/instance":
					if denied.Load() && mode == "credential-revoked" {
						w.WriteHeader(401)
						return
					}
					_, _ = w.Write([]byte(`{"instance_uid":"` + sourceDaemon + `","auth":{"kind":"token","scope":{"kind":"issue_subtree","project_uid":"` + sourceProject + `","root_issue_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAZ"}}}`))
				case "/api/v1/projects":
					catalogs.Add(1)
					if denied.Load() && mode == "refresh-outage" {
						w.WriteHeader(503)
						return
					}
					sourceCatalog(w)
				default:
					shows.Add(1)
					if active.Load() && mode == "show-outage" {
						w.WriteHeader(503)
						return
					}
					if active.Load() && (mode == "detached" || mode == "forbidden" || mode == "credential-revoked" || mode == "refresh-outage") {
						denied.Store(true)
						status := 404
						if mode == "forbidden" {
							status = 403
						}
						w.WriteHeader(status)
						return
					}
					_, _ = w.Write([]byte(`{"issue":{"uid":"` + sourceIssue + `","project_id":1,"project_uid":"` + sourceProject + `"},"comments":[],"labels":[],"links":[]}`))
				}
			}))
			t.Cleanup(server.Close)
			client, err := kataclient.NewWithHTTPClient(server.URL, server.Client())
			require.NoError(t, err)
			startup, err := NewBoundScope(ProjectIdentity{ID: 1, UID: sourceProject, Name: "spoke-project"})
			require.NoError(t, err)
			source := NewEventsSource(client, startup, sourceDaemon).(*inboxEventsSource)
			scope, err := source.Resolve(t.Context(), mcpevents.Arguments{For: "example-user"})
			require.NoError(t, err)
			payload := mcpevents.Payload{Recipient: "example-user", Reason: "notification", ProjectUID: sourceProject, IssueUID: sourceIssue}
			if mode == "foreign-project" {
				payload.ProjectUID = sourceOtherProject
			}
			if mode == "generic-resync" {
				payload = mcpevents.Payload{Recipient: "example-user", Reason: "resync", ResyncRequired: true}
			}
			active.Store(true)
			err = source.AuthorizeOccurrence(t.Context(), scope, payload)
			switch mode {
			case "visible", "generic-resync":
				require.NoError(t, err)
			case "credential-revoked":
				require.ErrorIs(t, err, mcpevents.ErrRevoked)
			case "show-outage", "refresh-outage":
				require.ErrorIs(t, err, mcpevents.ErrUnavailable)
			default:
				require.ErrorIs(t, err, mcpevents.ErrOccurrenceHidden)
			}
			wantShows := int32(1)
			if mode == "foreign-project" || mode == "generic-resync" {
				wantShows = 0
			}
			require.Equal(t, wantShows, shows.Load(), "never retry the denied issue lookup")
			wantCatalogs := int32(2)
			if mode == "detached" || mode == "forbidden" || mode == "refresh-outage" {
				wantCatalogs = 3
			}
			require.Equal(t, wantCatalogs, catalogs.Load(), "at most one refresh after the issue rejection")
		})
	}
}

func TestSourceRejectsLivePrincipalDrift(t *testing.T) {
	for _, field := range []string{"actor", "kind", "scope", "actions"} {
		t.Run(field, func(t *testing.T) {
			actor := "example-user"
			auth := generated.AuthInfoOut{Kind: "token", Actor: &actor, AllowedActions: []string{"read"}}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/api/v1/instance" {
					b, _ := json.Marshal(generated.InstanceResponseBody{InstanceUID: sourceDaemon, Auth: auth})
					_, _ = w.Write(b)
					return
				}
				sourceCatalog(w)
			}))
			t.Cleanup(server.Close)
			client, err := kataclient.NewWithHTTPClient(server.URL, server.Client())
			require.NoError(t, err)
			source := NewEventsSource(client, NewAllScope(), sourceDaemon, auth)
			scope, err := source.Resolve(t.Context(), mcpevents.Arguments{For: "example-user"})
			require.NoError(t, err)
			switch field {
			case "actor":
				actor = "other-user"
			case "kind":
				auth.Kind = "owner-local"
			case "scope":
				auth.Scope = &generated.TokenScopeOut{ProjectUID: sourceProject, RootIssueUID: sourceIssue}
			case "actions":
				auth.AllowedActions = []string{"read", "write"}
			}
			require.ErrorIs(t, source.Authorize(t.Context(), scope), mcpevents.ErrRevoked)
		})
	}
}

func TestEventsPrincipalFingerprintCanonicalActions(t *testing.T) {
	auth := generated.AuthInfoOut{Kind: "token", AllowedActions: []string{"write", "read"}}
	want := EventsPrincipalFingerprint(auth)
	require.Equal(t, []string{"write", "read"}, auth.AllowedActions)
	auth.AllowedActions = []string{"read", "write"}
	expiration := time.Now().UTC()
	auth.ExpiresAt = &expiration
	require.Equal(t, want, EventsPrincipalFingerprint(auth))
}

func TestSourceAuthorityFailureIsNotAnEmptyInbox(t *testing.T) {
	for status, want := range map[int]error{401: mcpevents.ErrRevoked, 403: mcpevents.ErrRevoked, 503: mcpevents.ErrUnavailable} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			source := NewEventsSource(sourceClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"error":{"code":"fixture","message":"unavailable"}}`))
			}), NewAllScope(), sourceDaemon)
			_, err := source.Resolve(t.Context(), mcpevents.Arguments{For: "example-user"})
			require.True(t, errors.Is(err, want), "%v", err)
		})
	}
}

func TestSourceOversizedHTTPFailureNeverCreatesAGap(t *testing.T) {
	for status, want := range map[int]error{401: mcpevents.ErrRevoked, 403: mcpevents.ErrRevoked, 429: mcpevents.ErrUnavailable, 500: mcpevents.ErrUnavailable, 503: mcpevents.ErrUnavailable} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			heads := 0
			client := sourceClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/projects" {
					sourceCatalog(w)
					return
				}
				if strings.HasSuffix(r.URL.Path, "/head") {
					heads++
					_, _ = w.Write([]byte(`{"after_id":77,"instance_uid":"` + sourceDaemon + `"}`))
					return
				}
				w.WriteHeader(status)
				_, _ = w.Write([]byte(strings.Repeat("x", (8<<20)+1)))
			})
			source := NewEventsSource(client, NewAllScope(), sourceDaemon)
			scope, err := source.Resolve(t.Context(), mcpevents.Arguments{For: "example-user", ProjectUID: sourceProject})
			require.NoError(t, err)
			page, err := source.Read(t.Context(), scope, mcpevents.Position{sourceProject: 0}, 1)
			require.ErrorIs(t, err, want)
			require.False(t, page.Reset)
			require.Empty(t, page.Positions)
			require.Zero(t, heads)
		})
	}
}

func sourceBackendFixtures() []dbtest.Backend {
	return []dbtest.Backend{
		{Name: "sqlite", Open: func(t *testing.T) db.Storage {
			store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "events.db"))
			require.NoError(t, err)
			t.Cleanup(func() { _ = store.Close() })
			return store
		}},
		{Name: "postgres", Open: func(t *testing.T) db.Storage {
			if testing.Short() {
				t.Skip("PostgreSQL conformance fixture skipped in focused short run")
			}
			dsn, cleanup := pgstoretest.NewDatabase(t, t.Context())
			t.Cleanup(cleanup)
			store, err := pgstore.OpenWithConfig(t.Context(), dsn, pgstore.DefaultConfig())
			require.NoError(t, err)
			t.Cleanup(func() { _ = store.Close() })
			return store
		}},
	}
}
