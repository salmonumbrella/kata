package main

import (
	"context"
	"fmt"
	"net/http"

	"go.kenn.io/kata/internal/uid"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

// initSetupClient uses the selected route's existing credential policy.
// It never reconstructs bearer credentials from a URL or persists them.
type initSetupClient struct {
	api        *kataclient.Client
	httpClient *http.Client
}

func newInitSetupClient(ctx context.Context, baseURL string) (initSetupClient, error) {
	hc, err := httpClientFor(ctx, baseURL)
	if err != nil {
		return initSetupClient{}, err
	}
	api, err := kataclient.NewWithHTTPClient(baseURL, hc)
	return initSetupClient{api: api, httpClient: hc}, err
}
func (c initSetupClient) instanceUID(ctx context.Context) (string, error) {
	response, err := c.api.InstanceWithResponse(ctx)
	if response == nil {
		return "", externalCLITransportError(response, err)
	}
	if err := externalCLIResponseError(response.StatusCode, response.Body, err); err != nil {
		return "", err
	}
	if response.JSON200 == nil || !uid.Valid(response.JSON200.InstanceUID) {
		return "", fmt.Errorf("daemon returned no valid instance UID")
	}
	return response.JSON200.InstanceUID, nil
}
func (c initSetupClient) projectByName(ctx context.Context, name string) (projectRef, error) {
	response, err := c.api.ResolveProjectWithResponse(ctx, &generated.ResolveProjectRequestOptions{Body: &generated.ResolveProjectBody{Name: &name}})
	if response == nil {
		return projectRef{}, externalCLITransportError(response, err)
	}
	if err := externalCLIResponseError(response.StatusCode, response.Body, err); err != nil {
		return projectRef{}, err
	}
	if response.JSON200 == nil || !uid.Valid(response.JSON200.Project.UID) {
		return projectRef{}, fmt.Errorf("daemon returned no valid project UID")
	}
	p := response.JSON200.Project
	return projectRef{ID: p.ID, UID: p.UID, Name: p.Name}, nil
}
