package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

// Native cron commands are finite API clients. Scheduling, process
// execution and durable executor journals belong to the external adapter.
func newCronCmd() *cobra.Command {
	command := &cobra.Command{Use: "cron", Short: "manage shared jobs, flows and run evidence", Long: "Manage dormant shared cron definitions and attributed run evidence. Results are JSON. These commands do not schedule work or launch processes."}
	for _, resource := range []string{"job", "flow"} {
		group := &cobra.Command{Use: resource, Short: "manage native " + resource + " definitions"}
		for _, action := range []string{"list", "show", "create", "update", "delete", "restore"} {
			group.AddCommand(newCronOperationCmd(resource, action))
		}
		command.AddCommand(group)
	}
	runs := &cobra.Command{Use: "run", Short: "read independent run observations"}
	for _, action := range []string{"list", "show", "observe"} {
		runs.AddCommand(newCronOperationCmd("run", action))
	}
	capabilities := newCronOperationCmd("capabilities", "show")
	capabilities.Use = "capabilities"
	capabilities.Short = "discover local protocol support without creating cron state"
	command.AddCommand(runs, capabilities)
	return command
}

type cronCLIOptions struct {
	resource, action                 string
	file, identity, expectedEventUID string
	includeDeleted                   bool
	limit                            int64
	beforeUID, jobUID                string
}

func (o cronCLIOptions) mutation() bool   { return o.action != "list" && o.action != "show" }
func (o cronCLIOptions) definition() bool { return o.resource == "job" || o.resource == "flow" }
func newCronOperationCmd(resource, action string) *cobra.Command {
	options := cronCLIOptions{resource: resource, action: action, limit: 100}
	takesUID := (resource == "job" || resource == "flow" || resource == "run") && action != "list" && action != "create"
	command := &cobra.Command{Use: action, Short: action + " native " + resource, Args: cobra.NoArgs}
	if takesUID {
		command.Use += " <uid>"
		command.Args = cobra.ExactArgs(1)
	}
	command.RunE = func(cmd *cobra.Command, args []string) error {
		if takesUID {
			options.identity = args[0]
		}
		return options.execute(cmd)
	}
	if options.mutation() {
		if resource == "run" {
			command.Flags().StringVar(&options.file, "json-input", "", "observation JSON file, or - for stdin")
		} else {
			command.Flags().StringVar(&options.file, "file", "", "JSON request body file, or - for stdin")
		}
		if options.definition() {
			if action == "create" {
				command.Flags().StringVar(&options.identity, "uid", "", "retained definition ULID (generated once and printed if absent)")
			} else {
				command.Flags().StringVar(&options.expectedEventUID, "expected-event-uid", "", "current definition event UID; must match the request body if present")
			}
		}
	}
	if action == "list" && options.definition() {
		command.Flags().BoolVar(&options.includeDeleted, "include-deleted", false, "include tombstoned definitions")
	}
	if resource == "run" && action == "list" {
		command.Flags().Int64Var(&options.limit, "limit", 100, "maximum results in one page (1-100)")
	}
	if resource == "run" && action == "list" {
		command.Flags().StringVar(&options.beforeUID, "before-uid", "", "run UID cursor from next_before_uid")
	}
	return command
}
func cronCLIValidation(message string) error {
	return &cliError{Message: message, Kind: kindValidation, ExitCode: ExitValidation}
}
func normalizedCronCLIUID(value string) (string, error) {
	value = strings.ToUpper(strings.TrimSpace(value))
	if !uid.Valid(value) {
		return "", cronCLIValidation("cron identity must be a 26-character ULID")
	}
	return value, nil
}
func (o *cronCLIOptions) execute(cmd *cobra.Command) error {
	for _, value := range []*string{&o.identity, &o.beforeUID, &o.jobUID} {
		if *value != "" {
			normalized, err := normalizedCronCLIUID(*value)
			if err != nil {
				return err
			}
			*value = normalized
		}
	}
	if o.limit < 1 || o.limit > 100 {
		return cronCLIValidation("limit must be between 1 and 100")
	}
	var body []byte
	if o.mutation() {
		var err error
		body, err = o.requestBody(cmd)
		if err != nil {
			return err
		}
		if o.definition() && o.action == "create" {
			if o.identity == "" {
				o.identity, err = uid.New()
				if err != nil {
					return err
				}
			}
			if _, err = fmt.Fprintf(cmd.ErrOrStderr(), "Definition UID: %s (reuse --uid on retry)\n", o.identity); err != nil {
				return err
			}
		}
	}
	daemon, err := dialDaemon(cmd.Context())
	if err != nil {
		return err
	}
	var project projectRef
	if strings.TrimSpace(flags.Project) != "" {
		project, err = resolveProjectSelectorIncludingArchived(daemon, flags.Project)
	} else {
		project, err = resolveProjectArgFlagOrWorkspace(daemon, nil)
	}
	if err != nil {
		return err
	}
	client, err := kataclient.NewWithHTTPClient(daemon.baseURL, daemon.client)
	if err != nil {
		return err
	}
	raw, err := o.call(daemon, client, project.ID, body)
	if err != nil {
		return err
	}
	if flags.Quiet && currentOutputMode() != outputJSON {
		return nil
	}
	return emitJSON(cmd.OutOrStdout(), jsontext.Value(raw))
}

func (o *cronCLIOptions) requestBody(cmd *cobra.Command) ([]byte, error) {
	raw := []byte(`{}`)
	if o.file != "" {
		reader := cmd.InOrStdin()
		if o.file != "-" {
			file, err := os.Open(o.file)
			if err != nil {
				return nil, err
			}
			defer func() { _ = file.Close() }()
			reader = file
		}
		var err error
		limit := int64(1 << 20)
		if o.resource == "run" {
			limit = db.CronObservationLimit
		}
		raw, err = io.ReadAll(io.LimitReader(reader, limit+1))
		if err != nil {
			return nil, err
		}
		if int64(len(raw)) > limit {
			return nil, cronCLIValidation("cron request exceeds its byte limit")
		}
	} else if !o.definition() || o.action == "create" || o.action == "update" {
		if o.resource == "run" {
			return nil, cronCLIValidation("--json-input is required; use --json-input - for stdin")
		}
		return nil, cronCLIValidation("--file is required; use --file - for stdin")
	}
	fields := map[string]jsontext.Value{}
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, cronCLIValidation("invalid cron JSON: " + err.Error())
	}
	if fields == nil {
		return nil, cronCLIValidation("cron request must be a JSON object")
	}
	// Validate the public shape before adding defaults. Raw option/input objects
	// remain intact; unknown permission booleans or misspelled fields never vanish.
	var shape any
	switch {
	case o.resource == "job" && (o.action == "create" || o.action == "update"):
		shape = &api.PutCronJobBody{}
	case o.resource == "flow" && (o.action == "create" || o.action == "update"):
		shape = &api.PutCronFlowBody{}
	case o.resource == "run" && o.action == "observe":
		shape = &api.ObserveCronRunBody{}
	case o.definition():
		shape = &api.CronDefinitionActionBody{}
	default:
		return nil, cronCLIValidation("unsupported cron operation")
	}
	if err := json.Unmarshal(raw, shape, json.RejectUnknownMembers(true)); err != nil {
		return nil, cronCLIValidation("invalid cron request: " + err.Error())
	}
	stringField := func(key string) (string, error) {
		var value string
		if raw, exists := fields[key]; exists {
			if err := json.Unmarshal(raw, &value); err != nil {
				return "", cronCLIValidation(key + " must be a string")
			}
		}
		return value, nil
	}
	setString := func(key, value string) { fields[key], _ = json.Marshal(value) }
	actor, err := stringField("actor")
	if err != nil {
		return nil, err
	}
	if actor == "" {
		actor, _ = resolveActor(cmd.Context(), flags.As, nil)
		setString("actor", actor)
	} else if flags.As != "" && actor != flags.As {
		return nil, cronCLIValidation("body actor and --as must match")
	}
	if o.resource == "run" && o.action == "observe" {
		if raw, frozen := fields["teammate"]; frozen {
			// Explicit JSON, including null or invalid empty identity, never
			// inherits the environment. A retry keeps its original attribution.
			if cmd.Flags().Changed("teammate") {
				selected, err := resolveTeammate(cmd)
				if err != nil {
					return nil, err
				}
				var body *string
				if err := json.Unmarshal(raw, &body); err != nil {
					return nil, cronCLIValidation("teammate must be a string or null")
				}
				value := ""
				if body != nil {
					value = *body
				}
				if selected != value {
					return nil, cronCLIValidation("body teammate and --teammate must match")
				}
			}
		} else {
			selected, err := resolveTeammate(cmd)
			if err != nil {
				return nil, err
			}
			if selected != "" {
				setString("teammate", selected)
			}
		}
	}
	key, option := "expected_event_uid", &o.expectedEventUID
	bodyUID, err := stringField(key)
	if err != nil {
		return nil, err
	}
	if bodyUID != "" {
		bodyUID, err = normalizedCronCLIUID(bodyUID)
		if err != nil {
			return nil, err
		}
	}
	if *option != "" {
		*option, err = normalizedCronCLIUID(*option)
		if err != nil {
			return nil, err
		}
		if bodyUID != "" && bodyUID != *option {
			return nil, cronCLIValidation(key + " in body and flag must match")
		}
	} else {
		*option = bodyUID
	}
	if o.definition() {
		if o.action == "create" && *option != "" {
			return nil, cronCLIValidation("create requires absent expected_event_uid; use update for replacement")
		}
		if o.action != "create" && *option == "" {
			return nil, cronCLIValidation("expected_event_uid is required for definition mutations")
		}
	}
	if *option != "" {
		setString(key, *option)
	}
	return json.Marshal(fields)
}

// Validate the native public shape first; generated option maps preserve their
// opaque JSON values through JSONv2 and the SDK's precise-number decoders.
func decodeCronCLIRequest(raw []byte, value any) error {
	return json.Unmarshal(raw, value, json.RejectUnknownMembers(true))
}
func cronCLIResponse(status int, raw []byte, err error) ([]byte, error) {
	if responseErr := externalCLIResponseError(status, raw, err); responseErr != nil {
		return nil, responseErr
	}
	return raw, nil
}

func (o cronCLIOptions) call(a daemonAPI, c *kataclient.Client, projectID int64, raw []byte) ([]byte, error) {
	switch {
	case o.resource == "job" && o.action == "list":
		response, err := c.ListCronJobsWithResponse(a.ctx, &generated.ListCronJobsRequestOptions{PathParams: &generated.ListCronJobsPath{ProjectID: projectID}, Query: &generated.ListCronJobsQuery{IncludeDeleted: &o.includeDeleted}})
		if response == nil {
			return nil, externalCLITransportError(response, err)
		}
		return cronCLIResponse(response.StatusCode, response.Body, err)
	case o.resource == "job" && o.action == "show":
		response, err := c.ShowCronJobWithResponse(a.ctx, &generated.ShowCronJobRequestOptions{PathParams: &generated.ShowCronJobPath{ProjectID: projectID, CronUID: o.identity}})
		if response == nil {
			return nil, externalCLITransportError(response, err)
		}
		return cronCLIResponse(response.StatusCode, response.Body, err)
	case o.resource == "flow" && o.action == "list":
		response, err := c.ListCronFlowsWithResponse(a.ctx, &generated.ListCronFlowsRequestOptions{PathParams: &generated.ListCronFlowsPath{ProjectID: projectID}, Query: &generated.ListCronFlowsQuery{IncludeDeleted: &o.includeDeleted}})
		if response == nil {
			return nil, externalCLITransportError(response, err)
		}
		return cronCLIResponse(response.StatusCode, response.Body, err)
	case o.resource == "flow" && o.action == "show":
		response, err := c.ShowCronFlowWithResponse(a.ctx, &generated.ShowCronFlowRequestOptions{PathParams: &generated.ShowCronFlowPath{ProjectID: projectID, CronUID: o.identity}})
		if response == nil {
			return nil, externalCLITransportError(response, err)
		}
		return cronCLIResponse(response.StatusCode, response.Body, err)
	case o.resource == "run" && o.action == "list":
		response, err := c.ListCronRunsWithResponse(a.ctx, &generated.ListCronRunsRequestOptions{PathParams: &generated.ListCronRunsPath{ProjectID: projectID}, Query: &generated.ListCronRunsQuery{Limit: &o.limit, BeforeUID: &o.beforeUID}})
		if response == nil {
			return nil, externalCLITransportError(response, err)
		}
		return cronCLIResponse(response.StatusCode, response.Body, err)
	case o.resource == "run" && o.action == "show":
		response, err := c.ShowCronRunWithResponse(a.ctx, &generated.ShowCronRunRequestOptions{PathParams: &generated.ShowCronRunPath{ProjectID: projectID, RunUID: o.identity}})
		if response == nil {
			return nil, externalCLITransportError(response, err)
		}
		return cronCLIResponse(response.StatusCode, response.Body, err)
	case o.resource == "capabilities":
		response, err := c.GetCronCapabilitiesWithResponse(a.ctx, &generated.GetCronCapabilitiesRequestOptions{PathParams: &generated.GetCronCapabilitiesPath{ProjectID: projectID}})
		if response == nil {
			return nil, externalCLITransportError(response, err)
		}
		raw, err := cronCLIResponse(response.StatusCode, response.Body, err)
		if err != nil {
			return nil, err
		}
		header := ""
		if response.Headers200 != nil {
			header = response.Headers200.XKataEventFeatures
		}
		return cronCapabilitiesCLIResponse(raw, header)
	}

	if o.resource == "run" && o.action == "observe" {
		var body generated.ObserveCronRunBody
		if err := decodeCronCLIRequest(raw, &body); err != nil {
			return nil, err
		}
		response, err := c.ObserveCronRunWithResponse(a.ctx, &generated.ObserveCronRunRequestOptions{PathParams: &generated.ObserveCronRunPath{ProjectID: projectID, RunUID: o.identity}, Body: &body})
		if response == nil {
			return nil, externalCLITransportError(response, err)
		}
		return cronCLIResponse(response.StatusCode, response.Body, err)
	}
	if o.resource == "job" && (o.action == "create" || o.action == "update") {
		var body generated.PutCronJobBody
		if err := decodeCronCLIRequest(raw, &body); err != nil {
			return nil, err
		}
		response, err := c.ReplaceCronJobWithResponse(a.ctx, &generated.ReplaceCronJobRequestOptions{PathParams: &generated.ReplaceCronJobPath{ProjectID: projectID, CronUID: o.identity}, Body: &body})
		if response == nil {
			return nil, externalCLITransportError(response, err)
		}
		return cronCLIResponse(response.StatusCode, response.Body, err)
	}
	if o.resource == "flow" && (o.action == "create" || o.action == "update") {
		var body generated.PutCronFlowBody
		if err := decodeCronCLIRequest(raw, &body); err != nil {
			return nil, err
		}
		response, err := c.ReplaceCronFlowWithResponse(a.ctx, &generated.ReplaceCronFlowRequestOptions{PathParams: &generated.ReplaceCronFlowPath{ProjectID: projectID, CronUID: o.identity}, Body: &body})
		if response == nil {
			return nil, externalCLITransportError(response, err)
		}
		return cronCLIResponse(response.StatusCode, response.Body, err)
	}
	if o.definition() {
		var body generated.CronDefinitionActionBody
		if err := decodeCronCLIRequest(raw, &body); err != nil {
			return nil, err
		}
		switch {
		case o.resource == "job" && o.action == "delete":
			response, err := c.ArchiveCronJobWithResponse(a.ctx, &generated.ArchiveCronJobRequestOptions{PathParams: &generated.ArchiveCronJobPath{ProjectID: projectID, CronUID: o.identity}, Body: &body})
			if response == nil {
				return nil, externalCLITransportError(response, err)
			}
			return cronCLIResponse(response.StatusCode, response.Body, err)
		case o.resource == "job" && o.action == "restore":
			response, err := c.RestoreCronJobWithResponse(a.ctx, &generated.RestoreCronJobRequestOptions{PathParams: &generated.RestoreCronJobPath{ProjectID: projectID, CronUID: o.identity}, Body: &body})
			if response == nil {
				return nil, externalCLITransportError(response, err)
			}
			return cronCLIResponse(response.StatusCode, response.Body, err)
		case o.resource == "flow" && o.action == "delete":
			response, err := c.ArchiveCronFlowWithResponse(a.ctx, &generated.ArchiveCronFlowRequestOptions{PathParams: &generated.ArchiveCronFlowPath{ProjectID: projectID, CronUID: o.identity}, Body: &body})
			if response == nil {
				return nil, externalCLITransportError(response, err)
			}
			return cronCLIResponse(response.StatusCode, response.Body, err)
		case o.resource == "flow" && o.action == "restore":
			response, err := c.RestoreCronFlowWithResponse(a.ctx, &generated.RestoreCronFlowRequestOptions{PathParams: &generated.RestoreCronFlowPath{ProjectID: projectID, CronUID: o.identity}, Body: &body})
			if response == nil {
				return nil, externalCLITransportError(response, err)
			}
			return cronCLIResponse(response.StatusCode, response.Body, err)
		}
	}
	return nil, cronCLIValidation("unsupported cron operation")
}

func cronCapabilitiesCLIResponse(raw []byte, header string) ([]byte, error) {
	var body map[string]jsontext.Value
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, err
	}
	if body == nil {
		return nil, cronCLIValidation("cron discovery response must be a JSON object")
	}
	features := []string{}
	seen := map[string]bool{}
	for part := range strings.SplitSeq(header, ",") {
		feature := strings.TrimSpace(part)
		if feature != "" && !seen[feature] {
			features = append(features, feature)
			seen[feature] = true
		}
	}
	encoded, err := json.Marshal(features)
	if err != nil {
		return nil, err
	}
	body["event_features"] = encoded
	return json.Marshal(body)
}
