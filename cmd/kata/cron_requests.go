package main

import (
	"context"
	"encoding/json/v2"
	"strings"

	"github.com/spf13/cobra"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
	"go.kenn.io/kata/internal/cron"
)

func newCronRequestWriteCmd(cancel bool) *cobra.Command {
	action, short := "request-run", "ask an optional runtime to run an execute job now"
	if cancel {
		action, short = "request-cancel", "ask an optional runtime to cancel a running run"
	}
	var requestUID string
	var ttl int64
	var inputs []string
	cmd := &cobra.Command{Use: action + " <uid>", Short: short, Args: cobra.ExactArgs(1), Long: "Submit an advisory request at the project's owning hub. Supply a fresh ULID, retain it with the TTL, and reuse both after a lost response. Request admission launches or cancels nothing itself."}
	cmd.Flags().StringVar(&requestUID, "request-uid", "", "request ULID; retain for retry")
	cmd.Flags().Int64Var(&ttl, "ttl", 600, "request lifetime in seconds (60..86400)")
	if !cancel { cmd.Flags().StringArrayVar(&inputs, "input", nil, "request input name=value (repeatable)") }
	_ = cmd.MarkFlagRequired("request-uid")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		return runCronProjectCommandWithDaemon(cmd, true, func(ctx context.Context, daemon daemonAPI, c *kataclient.Client, projectID int64, actor string) ([]byte, error) {
			target := strings.ToUpper(strings.TrimSpace(args[0]))
			body := &generated.CronRequestBody{UID: strings.ToUpper(strings.TrimSpace(requestUID)), Actor: &actor, TTLSeconds: &ttl}
			if cancel {
				wire, err := c.RequestCronRunCancelWithResponse(ctx, &generated.RequestCronRunCancelRequestOptions{PathParams: &generated.RequestCronRunCancelPath{ProjectID: projectID, RunUID: target}, Body: body})
				if wire == nil {
					return nil, externalCLITransportError(wire, err)
				}
				return cronCLIResponse(wire.StatusCode, wire.Body, err)
			}
			runBody := &generated.CronJobRequestRunBody{UID: body.UID, Actor: body.Actor, TTLSeconds: body.TTLSeconds}
			if len(inputs) > 0 {
				if err := requireDaemonAPIVersion(ctx, daemon.client, daemon.baseURL, cron.APIVersionRequestInputs, "cron request inputs"); err != nil { return nil, err }
				var declarations map[string]string
				read, readErr := c.GetCronJobInputsWithResponse(ctx, &generated.GetCronJobInputsRequestOptions{PathParams: &generated.GetCronJobInputsPath{ProjectID: projectID, CronUID: target}})
				if readErr == nil && read != nil && read.StatusCode == 200 {
					var resolved struct { Inputs []struct { Name string `json:"name"`; Type string `json:"type"` } `json:"inputs"`; Errors []cron.InputError `json:"errors"` }
					if json.Unmarshal(read.Body, &resolved) == nil {
						readable := true
						for _, problem := range resolved.Errors { if problem.Code == "declaration_invalid" { readable = false } }
						if readable { declarations = map[string]string{}; for _, input := range resolved.Inputs { declarations[input.Name] = input.Type } }
					}
				}
				values, err := parseCronRequestInputs(inputs, declarations)
				if err != nil { return nil, err }
				runBody.Inputs = make(map[string]any, len(values))
				for name, value := range values { runBody.Inputs[name] = value }
			}
			wire, err := c.RequestCronJobRunWithResponse(ctx, &generated.RequestCronJobRunRequestOptions{PathParams: &generated.RequestCronJobRunPath{ProjectID: projectID, CronUID: target}, Body: runBody})
			if wire == nil {
				return nil, externalCLITransportError(wire, err)
			}
			return cronCLIResponse(wire.StatusCode, wire.Body, err)
		})
	}
	return cmd
}

func newCronRequestListCmd() *cobra.Command {
	var state string
	cmd := &cobra.Command{Use: "list", Short: "list cron run and cancel requests with their answer state", Args: cobra.NoArgs}
	cmd.Flags().StringVar(&state, "state", "", "filter pending, answered or expired requests")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		return runCronProjectCommand(cmd, false, func(ctx context.Context, c *kataclient.Client, projectID int64, _ string) ([]byte, error) {
			query := &generated.ListCronRequestsQuery{}
			if state != "" {
				query.State = new(generated.ListCronRequestsQueryState(state))
			}
			wire, err := c.ListCronRequestsWithResponse(ctx, &generated.ListCronRequestsRequestOptions{PathParams: &generated.ListCronRequestsPath{ProjectID: projectID}, Query: query})
			if wire == nil {
				return nil, externalCLITransportError(wire, err)
			}
			return cronCLIResponse(wire.StatusCode, wire.Body, err)
		})
	}
	return cmd
}
