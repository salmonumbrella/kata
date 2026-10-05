package daemon

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
)

func registerCronDefinitionHandlers(h huma.API, cfg ServerConfig) {
	registerCronJobHandlers(h, cfg)
	registerCronFlowHandlers(h, cfg)
}

func cronAPIError(ctx context.Context, cfg ServerConfig, err error, projectID int64, uid string, job bool) error {
	if apiErr, ok := errors.AsType[*api.APIError](err); ok {
		return apiErr
	}
	switch {
	case errors.Is(err, db.ErrNotFound):
		return api.NewError(404, "not_found", "cron not found", "", nil)
	case errors.Is(err, cron.ErrInvalid):
		return api.NewError(400, "validation", err.Error(), "", nil)
	case errors.Is(err, db.ErrCronConflict):
		data := map[string]any{}
		if uid != "" {
			if job {
				if current, readErr := cfg.DB.CronJob(ctx, projectID, uid); readErr == nil {
					data["current"] = api.CronJobFrom(current)
				}
			} else {
				if current, readErr := cfg.DB.CronFlow(ctx, projectID, uid); readErr == nil {
					data["current"] = api.CronFlowFrom(current)
				}
			}
		}
		return api.NewError(409, "cron_conflict", err.Error(), "refresh the current cron before retrying", data)
	case errors.Is(err, db.ErrFederatedReadOnly):
		return federationReadOnlyError(err)
	default:
		return internalAPIError(err)
	}
}

func registerCronJobHandlers(h huma.API, cfg ServerConfig) {
	path := "/api/v1/projects/{project_id}/cron/jobs"
	put := func(ctx context.Context, projectID int64, id string, body api.PutCronJobBody, deleted bool) (*api.CronJobResponse, error) {
		ctx, actor, err := cronWriteContext(ctx, body.Actor)
		if err != nil {
			return nil, err
		}
		if _, err := activeProjectByID(ctx, cfg.DB, projectID); err != nil {
			return nil, err
		}
		value, events, err := cfg.DB.PutCronJob(ctx, db.PutCronJob{ProjectID: projectID, UID: id, ExpectedEventUID: body.ExpectedEventUID, Name: body.Name, Definition: body.Definition.Native(), Actor: actor, Deleted: deleted})
		if err != nil {
			return nil, cronAPIError(ctx, cfg, err, projectID, id, true)
		}

		cfg.Publish().Events(projectID, events)
		out := &api.CronJobResponse{}
		out.Body.Job = api.CronJobFrom(value)
		out.Body.Events = events
		return out, nil
	}
	huma.Register(h, huma.Operation{OperationID: "createCronJob", Method: http.MethodPost, Path: path, DefaultStatus: 201}, func(ctx context.Context, in *api.CreateCronJobRequest) (*api.CronJobResponse, error) {
		return put(ctx, in.ProjectID, "", in.Body, false)
	})
	huma.Register(h, huma.Operation{OperationID: "replaceCronJob", Method: http.MethodPut, Path: path + "/{cron_uid}"}, func(ctx context.Context, in *api.ReplaceCronJobRequest) (*api.CronJobResponse, error) {
		return put(ctx, in.ProjectID, in.UID, in.Body, false)
	})
	huma.Register(h, huma.Operation{OperationID: "showCronJob", Method: http.MethodGet, Path: path + "/{cron_uid}"}, func(ctx context.Context, in *api.CronDefinitionRequest) (*api.CronJobResponse, error) {
		if _, err := activeProjectByID(ctx, cfg.DB, in.ProjectID); err != nil {
			return nil, err
		}
		value, err := cfg.DB.CronJob(ctx, in.ProjectID, in.UID)
		if err != nil {
			return nil, cronAPIError(ctx, cfg, err, in.ProjectID, in.UID, true)
		}
		out := &api.CronJobResponse{}
		out.Body.Job = api.CronJobFrom(value)
		return out, nil
	})
	huma.Register(h, huma.Operation{OperationID: "listCronJobs", Method: http.MethodGet, Path: path}, func(ctx context.Context, in *api.ListCronDefinitionsRequest) (*api.ListCronJobsResponse, error) {
		if _, err := activeProjectByID(ctx, cfg.DB, in.ProjectID); err != nil {
			return nil, err
		}
		values, err := cfg.DB.ListCronJobs(ctx, db.CronList{ProjectID: in.ProjectID, IncludeDeleted: in.IncludeDeleted})
		if err != nil {
			return nil, cronAPIError(ctx, cfg, err, in.ProjectID, "", true)
		}
		out := &api.ListCronJobsResponse{}
		out.Body.Jobs = []api.CronJob{}
		for _, value := range values {
			out.Body.Jobs = append(out.Body.Jobs, api.CronJobFrom(value))
		}
		return out, nil
	})
	for _, action := range []struct {
		name, method, suffix string
		deleted              bool
	}{{"archive", http.MethodDelete, "", true}, {"restore", http.MethodPost, "/restore", false}} {
		huma.Register(h, huma.Operation{OperationID: action.name + "CronJob", Method: action.method, Path: path + "/{cron_uid}" + action.suffix}, func(ctx context.Context, in *api.CronDefinitionActionRequest) (*api.CronJobResponse, error) {
			if _, err := attributedActor(ctx, in.Body.Actor); err != nil {
				return nil, err
			}
			if _, err := activeProjectByID(ctx, cfg.DB, in.ProjectID); err != nil {
				return nil, err
			}
			value, err := cfg.DB.CronJob(ctx, in.ProjectID, in.UID)
			if err != nil {
				return nil, cronAPIError(ctx, cfg, err, in.ProjectID, in.UID, true)
			}
			return put(ctx, in.ProjectID, in.UID, api.PutCronJobBody{Actor: in.Body.Actor, Name: value.Name, Definition: api.CronJobDefinitionFrom(value.Definition), ExpectedEventUID: in.Body.ExpectedEventUID}, action.deleted)
		})
	}
}

func registerCronFlowHandlers(h huma.API, cfg ServerConfig) {
	path := "/api/v1/projects/{project_id}/cron/flows"
	put := func(ctx context.Context, projectID int64, id string, body api.PutCronFlowBody, deleted bool) (*api.CronFlowResponse, error) {
		ctx, actor, err := cronWriteContext(ctx, body.Actor)
		if err != nil {
			return nil, err
		}
		if _, err := activeProjectByID(ctx, cfg.DB, projectID); err != nil {
			return nil, err
		}
		value, event, err := cfg.DB.PutCronFlow(ctx, db.PutCronFlow{ProjectID: projectID, UID: id, ExpectedEventUID: body.ExpectedEventUID, Name: body.Name, Definition: body.Definition.Native(), Actor: actor, Deleted: deleted})
		if err != nil {
			return nil, cronAPIError(ctx, cfg, err, projectID, id, false)
		}
		events := []db.Event{event}
		cfg.Publish().Events(projectID, events)
		out := &api.CronFlowResponse{}
		out.Body.Flow = api.CronFlowFrom(value)
		out.Body.Events = events
		return out, nil
	}
	huma.Register(h, huma.Operation{OperationID: "createCronFlow", Method: http.MethodPost, Path: path, DefaultStatus: 201}, func(ctx context.Context, in *api.CreateCronFlowRequest) (*api.CronFlowResponse, error) {
		return put(ctx, in.ProjectID, "", in.Body, false)
	})
	huma.Register(h, huma.Operation{OperationID: "replaceCronFlow", Method: http.MethodPut, Path: path + "/{cron_uid}"}, func(ctx context.Context, in *api.ReplaceCronFlowRequest) (*api.CronFlowResponse, error) {
		return put(ctx, in.ProjectID, in.UID, in.Body, false)
	})
	huma.Register(h, huma.Operation{OperationID: "showCronFlow", Method: http.MethodGet, Path: path + "/{cron_uid}"}, func(ctx context.Context, in *api.CronDefinitionRequest) (*api.CronFlowResponse, error) {
		if _, err := activeProjectByID(ctx, cfg.DB, in.ProjectID); err != nil {
			return nil, err
		}
		value, err := cfg.DB.CronFlow(ctx, in.ProjectID, in.UID)
		if err != nil {
			return nil, cronAPIError(ctx, cfg, err, in.ProjectID, in.UID, false)
		}
		out := &api.CronFlowResponse{}
		out.Body.Flow = api.CronFlowFrom(value)
		return out, nil
	})
	huma.Register(h, huma.Operation{OperationID: "listCronFlows", Method: http.MethodGet, Path: path}, func(ctx context.Context, in *api.ListCronDefinitionsRequest) (*api.ListCronFlowsResponse, error) {
		if _, err := activeProjectByID(ctx, cfg.DB, in.ProjectID); err != nil {
			return nil, err
		}
		values, err := cfg.DB.ListCronFlows(ctx, db.CronList{ProjectID: in.ProjectID, IncludeDeleted: in.IncludeDeleted})
		if err != nil {
			return nil, cronAPIError(ctx, cfg, err, in.ProjectID, "", false)
		}
		out := &api.ListCronFlowsResponse{}
		out.Body.Flows = []api.CronFlow{}
		for _, value := range values {
			out.Body.Flows = append(out.Body.Flows, api.CronFlowFrom(value))
		}
		return out, nil
	})
	for _, action := range []struct {
		name, method, suffix string
		deleted              bool
	}{{"archive", http.MethodDelete, "", true}, {"restore", http.MethodPost, "/restore", false}} {
		huma.Register(h, huma.Operation{OperationID: action.name + "CronFlow", Method: action.method, Path: path + "/{cron_uid}" + action.suffix}, func(ctx context.Context, in *api.CronDefinitionActionRequest) (*api.CronFlowResponse, error) {
			if _, err := attributedActor(ctx, in.Body.Actor); err != nil {
				return nil, err
			}
			if _, err := activeProjectByID(ctx, cfg.DB, in.ProjectID); err != nil {
				return nil, err
			}
			value, err := cfg.DB.CronFlow(ctx, in.ProjectID, in.UID)
			if err != nil {
				return nil, cronAPIError(ctx, cfg, err, in.ProjectID, in.UID, false)
			}
			return put(ctx, in.ProjectID, in.UID, api.PutCronFlowBody{Actor: in.Body.Actor, Name: value.Name, Definition: api.CronFlowDefinitionFrom(value.Definition), ExpectedEventUID: in.Body.ExpectedEventUID}, action.deleted)
		})
	}
}
