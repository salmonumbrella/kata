package db

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"

	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/metadata"
)

// CronRequestInputsMetadataPrefix reserves the replicated request-values sibling.
const CronRequestInputsMetadataPrefix = "cron.request_inputs."

type cronRequestInputsValue struct {
	Version int                       `json:"version"`
	Values  map[string]jsontext.Value `json:"values"`
}

func canonicalCronRequestInputs(values map[string]jsontext.Value) (jsontext.Value, error) {
	if len(values) > 32 {
		return nil, fmt.Errorf("%w: inputs exceed 32 entries", cron.ErrInvalid)
	}
	for name, value := range values {
		if !value.IsValid() || (value.Kind() != jsontext.KindString && value.Kind() != jsontext.KindNumber && value.Kind() != jsontext.KindTrue && value.Kind() != jsontext.KindFalse) {
			return nil, fmt.Errorf("%w: %s: input must be a scalar", cron.ErrInvalid, name)
		}
	}
	if len(values) == 0 {
		return nil, nil
	}
	raw, err := json.Marshal(values, json.Deterministic(true))
	if err != nil {
		return nil, err
	}
	raw = metadata.NormalizeJSON(raw)
	if len(raw) > 4096 {
		return nil, fmt.Errorf("%w: inputs exceed 4 KiB", cron.ErrInvalid)
	}
	return raw, nil
}

func decodeCronRequestInputs(id string, raw jsontext.Value) (map[string]jsontext.Value, jsontext.Value, error) {
	var wrapper cronRequestInputsValue
	members, err := decodeCronControl(raw, &wrapper)
	if err != nil || !cronUID(id) || wrapper.Version != 1 || members["values"].Kind() != jsontext.KindBeginObject || wrapper.Values == nil {
		return nil, nil, fmt.Errorf("%w: invalid request inputs wrapper", cron.ErrInvalid)
	}
	canonical, err := canonicalCronRequestInputs(wrapper.Values)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: invalid request inputs wrapper", cron.ErrInvalid)
	}
	return wrapper.Values, canonical, nil
}

func validateCronRequestInputsChange(id string, from, to jsontext.Value) error {
	if to.Kind() == jsontext.KindNull {
		return nil
	}
	_, next, err := decodeCronRequestInputs(id, to)
	if err != nil {
		return err
	}
	if len(from) == 0 || from.Kind() == jsontext.KindNull {
		return nil
	}
	_, previous, err := decodeCronRequestInputs(id, from)
	if err != nil {
		return err
	}
	if !bytes.Equal(previous, next) {
		return ErrCronRequestConflict
	}
	return nil
}
