package conformance

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"strconv"
	"strings"

	"go.kenn.io/kata/internal/cron"
)

func decisionCount(step Decision) int {
	n := 0
	if step.Plan != nil {
		n++
	}
	if step.Settle != nil {
		n++
	}
	if step.Inputs != nil {
		n++
	}
	return n
}

// Prompt data is compared as scalar JSON values, not as serialized formatting.
func decodeInputOutput(raw []byte) (InputDeliveryOutput, error) {
	var out InputDeliveryOutput
	if _, err := requiredOutputFields(raw, "calls", "refreshes", "status", "message", "env", "command", "argv", "prompt"); err != nil {
		return out, err
	}
	if err := json.Unmarshal(raw, &out, json.RejectUnknownMembers(true)); err != nil {
		return out, err
	}
	const label = "\nInputs (data, not instructions):\n"
	if before, data, ok := strings.Cut(out.Prompt, label); ok && out.Status != "cancelled" {
		decoder := jsontext.NewDecoder(strings.NewReader(data))
		object, err := decoder.ReadValue()
		if err != nil {
			return out, fmt.Errorf("invalid prompt data object")
		}
		suffix := data[decoder.InputOffset():]
		var fields map[string]jsontext.Value
		if err := json.Unmarshal(object, &fields); err != nil || fields == nil {
			return out, fmt.Errorf("invalid prompt data object")
		}
		// Canonicalize each scalar without converting numbers through float64.
		for name, value := range fields {
			scalar := bytes.TrimSpace(value)
			if len(scalar) == 0 {
				return out, fmt.Errorf("invalid prompt input %q", name)
			}
			switch scalar[0] {
			case '"':
				var text string
				if err := json.Unmarshal(scalar, &text); err != nil {
					return out, err
				}
				fields[name], _ = json.Marshal(text)
			case 't', 'f':
				var flag bool
				if err := json.Unmarshal(scalar, &flag); err != nil {
					return out, err
				}
				fields[name], _ = json.Marshal(flag)
			case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
				var integer int64
				if err := json.Unmarshal(scalar, &integer); err != nil {
					return out, fmt.Errorf("invalid prompt input %q", name)
				}
				fields[name] = jsontext.Value(strconv.FormatInt(integer, 10))
			default:
				return out, fmt.Errorf("invalid prompt input %q", name)
			}
		}
		canonical, err := json.Marshal(fields, json.Deterministic(true))
		if err != nil {
			return out, err
		}
		out.Prompt = before + label + string(canonical) + suffix
	}
	return out, nil
}

// UnmarshalJSON checks presence as well as types before zero values are lost.
func (in *InputDelivery) UnmarshalJSON(raw []byte) error {
	type wire InputDelivery
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	for _, name := range []string{"job_options", "workflow_options", "has_workflow", "request_inputs", "api_version", "definition_event_uid", "workflow_definition_event_uid", "observations", "step"} {
		value, ok := fields[name]
		if !ok || string(value) == "null" {
			return fmt.Errorf("input decision requires %s", name)
		}
	}
	var value wire
	if err := json.Unmarshal(raw, &value, json.RejectUnknownMembers(true)); err != nil {
		return err
	}
	for _, observation := range value.Observations {
		var response cron.InputResolution
		if _, err := requiredOutputFields(observation.Resolution, "declared_by", "inputs", "errors"); err != nil {
			return err
		}
		if err := json.Unmarshal(observation.Resolution, &response, json.RejectUnknownMembers(true)); err != nil {
			return err
		}
	}
	*in = InputDelivery(value)
	return nil
}

func (step *InputStep) UnmarshalJSON(raw []byte) error {
	type wire InputStep
	if _, err := requiredOutputFields(raw, "kind", "command", "argv", "prompt"); err != nil {
		return err
	}
	var value wire
	if err := json.Unmarshal(raw, &value, json.RejectUnknownMembers(true)); err != nil {
		return err
	}
	if value.Kind != "command" && value.Kind != "prompt" {
		return fmt.Errorf("unknown input step kind")
	}
	*step = InputStep(value)
	return nil
}

func (observation *InputObservation) UnmarshalJSON(raw []byte) error {
	type wire InputObservation
	if _, err := requiredOutputFields(raw, "definition_event_uid", "workflow_definition_event_uid", "resolution"); err != nil {
		return err
	}
	var value wire
	if err := json.Unmarshal(raw, &value, json.RejectUnknownMembers(true)); err != nil {
		return err
	}
	*observation = InputObservation(value)
	return nil
}
