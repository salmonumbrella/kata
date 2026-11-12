package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"strconv"
	"strings"
)

// parseCronRequestInputs uses a nil declaration map when the read was unavailable.
func parseCronRequestInputs(args []string, declarations map[string]string) (map[string]jsontext.Value, error) {
	values := make(map[string]jsontext.Value, len(args))
	for _, arg := range args {
		name, value, ok := strings.Cut(arg, "=")
		if !ok || name == "" {
			return nil, fmt.Errorf("invalid input %q: expected name=value", arg)
		}
		if _, duplicate := values[name]; duplicate {
			return nil, fmt.Errorf("%s: duplicate input", name)
		}
		kind, known := declarations[name]
		if declarations != nil && !known {
			return nil, fmt.Errorf("%s: undeclared input", name)
		}
		var raw jsontext.Value
		if strings.HasPrefix(value, `"`) && (kind == "" || kind == "string" || kind == "enum") {
			var text string
			if err := json.Unmarshal([]byte(value), &text); err != nil {
				return nil, fmt.Errorf("%s: invalid quoted string", name)
			}
			raw, _ = json.Marshal(text)
		} else {
			switch kind {
			case "string", "enum":
				raw, _ = json.Marshal(value)
			case "int":
				n, err := strconv.ParseInt(value, 10, 64)
				if err != nil {
					return nil, fmt.Errorf("%s: invalid integer", name)
				}
				raw = jsontext.Value(strconv.FormatInt(n, 10))
				if candidate := jsontext.Value(value); candidate.IsValid() && candidate.Kind() == jsontext.KindNumber {
					raw = candidate
				}
			case "bool":
				if value != "true" && value != "false" {
					return nil, fmt.Errorf("%s: invalid boolean", name)
				}
				raw = jsontext.Value(value)
			default:
				candidate := jsontext.Value(value)
				if candidate.IsValid() && candidate.Kind() == jsontext.KindNull {
					return nil, fmt.Errorf("%s: null is not an input value", name)
				}
				if candidate.IsValid() && (candidate.Kind() == jsontext.KindNumber || candidate.Kind() == jsontext.KindTrue || candidate.Kind() == jsontext.KindFalse) {
					raw = candidate
				} else {
					raw, _ = json.Marshal(value)
				}
			}
		}
		values[name] = raw
	}
	return values, nil
}
