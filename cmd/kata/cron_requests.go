package main

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/cron"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
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
	if !cancel {
		cmd.Flags().StringArrayVar(&inputs, "input", nil, "request input name=value (repeatable)")
	}
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
				if err := requireDaemonAPIVersion(ctx, daemon.client, daemon.baseURL, cron.APIVersionRequestInputs, "cron request inputs"); err != nil {
					return nil, err
				}
				var declarations map[string]string
				list, listErr := c.ListCronRequestsWithResponse(ctx, &generated.ListCronRequestsRequestOptions{PathParams: &generated.ListCronRequestsPath{ProjectID: projectID}})
				if listErr == nil && list != nil && list.StatusCode == 200 {
					var retained struct {
						Requests []struct {
							UID string `json:"uid"`
							Inputs map[string]jsontext.Value `json:"inputs"`
						} `json:"requests"`
					}
					if json.Unmarshal(list.Body, &retained) == nil {
						for _, request := range retained.Requests {
							if request.UID != body.UID {
								continue
							}
							declarations = map[string]string{}
							for name, value := range request.Inputs {
								switch value.Kind() {
								case jsontext.KindString:
									declarations[name] = "string"
								case jsontext.KindNumber:
									declarations[name] = "int"
								case jsontext.KindTrue, jsontext.KindFalse:
									declarations[name] = "bool"
								}
							}
							// Let the server compare changed names to the retained identity.
							for _, input := range inputs {
								name, _, ok := strings.Cut(input, "=")
								if _, known := declarations[name]; ok && !known {
									declarations[name] = ""
								}
							}
							break
						}
					}
				}
				if declarations == nil {
					read, readErr := c.GetCronJobInputsWithResponse(ctx, &generated.GetCronJobInputsRequestOptions{PathParams: &generated.GetCronJobInputsPath{ProjectID: projectID, CronUID: target}})
					if readErr == nil && read != nil && read.StatusCode == 200 {
					var resolved struct {
						Inputs []struct {
							Name string `json:"name"`
							Type string `json:"type"`
						} `json:"inputs"`
						Errors []cron.InputError `json:"errors"`
					}
					if json.Unmarshal(read.Body, &resolved) == nil {
						readable := true
						for _, problem := range resolved.Errors {
							if problem.Code == "declaration_invalid" {
								readable = false
							}
						}
						if readable {
							declarations = map[string]string{}
							for _, input := range resolved.Inputs {
								declarations[input.Name] = input.Type
							}
						}
					}
					}
				}
				values, err := parseCronRequestInputs(inputs, declarations)
				if err != nil {
					return nil, err
				}
				runBody.Inputs = make(map[string]any, len(values))
				for name, value := range values {
					runBody.Inputs[name] = value
				}
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
