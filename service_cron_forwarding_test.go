package kata_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/kata"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/testenv"
)

type fixedCredentialStore struct {
	credential kata.FederationCredential
}

type serviceCredentialMap map[string]kata.FederationCredential

func (s serviceCredentialMap) FederationCredential(_ context.Context, projectUID string) (kata.FederationCredential, bool, error) {
	credential, ok := s[projectUID]
	return credential, ok, nil
}

func (s serviceCredentialMap) StoreFederationCredential(_ context.Context, projectUID string, credential kata.FederationCredential) error {
	s[projectUID] = credential
	return nil
}

func (s serviceCredentialMap) DeleteFederationCredential(_ context.Context, projectUID string) error {
	delete(s, projectUID)
	return nil
}

func (s *fixedCredentialStore) FederationCredential(context.Context, string) (kata.FederationCredential, bool, error) {
	return s.credential, true, nil
}

func (s *fixedCredentialStore) StoreFederationCredential(context.Context, string, kata.FederationCredential) error {
	return nil
}

func (s *fixedCredentialStore) DeleteFederationCredential(context.Context, string) error {
	return nil
}

// A mounted service must forward native cron writes like the standalone
// daemon: the shared push callback drains pending spoke events before the hub
// mutation, so a newly created local parent reaches the hub first.
func TestServiceCronForwardUsesPushPending(t *testing.T) {
	hub := testenv.New(t)
	hubProject, err := hub.DB.CreateProject(t.Context(), "hub-project")
	require.NoError(t, err)
	_, err = hub.DB.EnableProjectFederation(t.Context(), hubProject.ID, "tester")
	require.NoError(t, err)

	dsn := filepath.Join(t.TempDir(), "service.db")
	credentials := &fixedCredentialStore{credential: kata.FederationCredential{
		HubURL: hub.URL, HubProjectID: hubProject.ID, Token: "spoke-push-token",
		Capabilities: "push", Actor: "tester",
	}}
	service, err := kata.New(t.Context(), kata.Config{DSN: dsn, FederationCredentials: credentials, Auth: kata.AuthConfig{Token: "service-token"}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })

	spoke, err := sqlitestore.Open(t.Context(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, spoke.Close()) })

	result, err := service.EnsureProject(t.Context(), kata.ProjectSpec{UID: hubProject.UID, Name: "spoke-project"})
	require.NoError(t, err)
	spokeProjectID := result.Project.ID
	spokeParent, _, err := spoke.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: spokeProjectID, Title: "Local parent", Author: "tester"})
	require.NoError(t, err)
	_, err = spoke.UpsertFederationBinding(t.Context(), db.FederationBinding{
		ProjectID: spokeProjectID, Role: db.FederationRoleSpoke, HubURL: hub.URL,
		HubProjectID: hubProject.ID, HubProjectUID: hubProject.UID,
		Actor: "tester", Enabled: true, PushEnabled: true,
	})
	require.NoError(t, err)
	_, err = hub.DB.CreateFederationEnrollment(t.Context(), db.CreateFederationEnrollmentParams{
		Token:            "spoke-push-token",
		SpokeInstanceUID: spoke.InstanceUID(),
		ProjectID:        &hubProject.ID,
		Capabilities:     "push",
		Actor:            "tester",
	})
	require.NoError(t, err)

	server := httptest.NewServer(service.Handler())
	t.Cleanup(server.Close)
	definition := cron.JobDefinition{Version: 1, Kind: "job", Enabled: true,
		Trigger: cron.Trigger{Kind: "interval", IntervalSeconds: 7200},
		Action:  cron.Action{Kind: "notify", Recipient: "current-owner-or-author"},
		Issue:   &cron.IssuePolicy{Kind: "per-run", ParentUID: spokeParent.UID, Title: "Review service"}, Overlap: "allow", Catchup: "all"}
	body, err := json.Marshal(map[string]any{"actor": "tester", "name": "Review", "definition": definition})
	require.NoError(t, err)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		server.URL+"/api/v1/projects/"+strconv.FormatInt(spokeProjectID, 10)+"/cron/jobs",
		bytes.NewReader(body))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer service-token")
	response, err := server.Client().Do(request)
	require.NoError(t, err)
	raw := readAll(t, response)
	require.Equalf(t, http.StatusCreated, response.StatusCode, "%s", raw)
	require.NoError(t, response.Body.Close())

	jobs, err := hub.DB.ListCronJobs(t.Context(), db.CronList{ProjectID: hubProject.ID})
	require.NoError(t, err)
	require.Len(t, jobs, 1, "forwarded write must create the hub job")
	require.Equal(t, "tester", jobs[0].Author)
}

func TestServiceCronForwardUsesServiceCredentialStoreForMoveProof(t *testing.T) {
	hub := testenv.New(t)
	sourceHub, err := hub.DB.CreateProject(t.Context(), "source-project")
	require.NoError(t, err)
	destinationHub, err := hub.DB.CreateProject(t.Context(), "destination-project")
	require.NoError(t, err)
	for _, project := range []db.Project{sourceHub, destinationHub} {
		_, err := hub.DB.EnableProjectFederation(t.Context(), project.ID, "worker")
		require.NoError(t, err)
	}

	dsn := filepath.Join(t.TempDir(), "service.db")
	credentials := serviceCredentialMap{}
	service, err := kata.New(t.Context(), kata.Config{DSN: dsn, FederationCredentials: credentials, Auth: kata.AuthConfig{Token: "service-token"}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })
	spoke, err := sqlitestore.Open(t.Context(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, spoke.Close()) })
	t.Setenv("KATA_HOME", hub.Home)

	source, err := service.EnsureProject(t.Context(), kata.ProjectSpec{UID: sourceHub.UID, Name: "source-project"})
	require.NoError(t, err)
	destination, err := service.EnsureProject(t.Context(), kata.ProjectSpec{UID: destinationHub.UID, Name: "destination-project"})
	require.NoError(t, err)
	for _, pair := range []struct {
		localID int64
		hub     db.Project
	}{{source.Project.ID, sourceHub}, {destination.Project.ID, destinationHub}} {
		_, err = spoke.UpsertFederationBinding(t.Context(), db.FederationBinding{
			ProjectID: pair.localID, Role: db.FederationRoleSpoke, HubURL: hub.URL,
			HubProjectID: pair.hub.ID, HubProjectUID: pair.hub.UID,
			Actor: "worker", Enabled: true, PushEnabled: true,
		})
		require.NoError(t, err)
		grant, err := hub.DB.CreateFederationEnrollment(t.Context(), db.CreateFederationEnrollmentParams{
			Token: "push-" + pair.hub.Name, SpokeInstanceUID: spoke.InstanceUID(),
			ProjectID: &pair.hub.ID, Capabilities: "push", Actor: "worker",
		})
		require.NoError(t, err)
		require.NoError(t, credentials.StoreFederationCredential(t.Context(), pair.hub.UID, kata.FederationCredential{
			HubURL: hub.URL, HubProjectID: pair.hub.ID, Token: grant.Token, Capabilities: "push", Actor: "worker",
		}))
	}

	issue, _, err := spoke.CreateIssue(t.Context(), db.CreateIssueParams{
		ProjectID: source.Project.ID, Title: "Moved parent", Author: "worker",
	})
	require.NoError(t, err)
	sourceCronParent, _, err := spoke.CreateIssue(t.Context(), db.CreateIssueParams{
		ProjectID: source.Project.ID, Title: "Source cron parent", Author: "worker",
	})
	require.NoError(t, err)
	server := httptest.NewServer(service.Handler())
	t.Cleanup(server.Close)
	createCronJob := func(projectID int64, name string, parentUID string) {
		t.Helper()
		definition := cron.JobDefinition{
			Version: 1, Kind: "job", Enabled: true,
			Trigger: cron.Trigger{Kind: "interval", IntervalSeconds: 7200},
			Action:  cron.Action{Kind: "notify", Recipient: "current-owner-or-author"},
			Issue:   &cron.IssuePolicy{Kind: "per-run", ParentUID: parentUID, Title: "Review service"},
			Overlap: "allow", Catchup: "all",
		}
		body, err := json.Marshal(map[string]any{"actor": "worker", "name": name, "definition": definition})
		require.NoError(t, err)
		request, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
			server.URL+"/api/v1/projects/"+strconv.FormatInt(projectID, 10)+"/cron/jobs", bytes.NewReader(body))
		require.NoError(t, err)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer service-token")
		response, err := server.Client().Do(request)
		require.NoError(t, err)
		raw := readAll(t, response)
		if response.StatusCode != http.StatusCreated {
			status, statusErr := spoke.FederationSyncStatusByProject(t.Context(), projectID)
			require.NoError(t, statusErr)
			lastError := ""
			if status.LastError != nil {
				lastError = *status.LastError
			}
			t.Logf("pending push status for project %d: last_error=%s", projectID, lastError)
		}
		require.Equalf(t, http.StatusCreated, response.StatusCode, "%s", raw)
		require.NoError(t, response.Body.Close())
	}

	createCronJob(source.Project.ID, "Source review", sourceCronParent.UID)
	moved, err := spoke.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	_, err = spoke.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{
		IssueID: moved.ID, FromProjectID: source.Project.ID, ToProjectID: destination.Project.ID,
		IfMatchRev: moved.Revision, Actor: "worker",
	})
	require.NoError(t, err)
	createCronJob(destination.Project.ID, "Destination review", issue.UID)

	current, err := hub.DB.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, destinationHub.ID, current.ProjectID)
	jobs, err := hub.DB.ListCronJobs(t.Context(), db.CronList{ProjectID: destinationHub.ID})
	require.NoError(t, err)
	require.Len(t, jobs, 1, "the destination mutation must push the move using the service-scoped source credential")
}

func readAll(t *testing.T, response *http.Response) string {
	t.Helper()
	out := new(bytes.Buffer)
	_, err := out.ReadFrom(response.Body)
	require.NoError(t, err)
	return out.String()
}
