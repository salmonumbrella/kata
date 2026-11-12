package conformance

import (
 "encoding/json/jsontext"
 "encoding/json/v2"
 "fmt"
 "strconv"
 "strings"
)

func decisionCount(step Decision) int {
 n := 0
 if step.Plan != nil { n++ }
 if step.Settle != nil { n++ }
 if step.Inputs != nil { n++ }
 return n
}

// Prompt data is compared as scalar JSON values, not as serialized formatting.
func decodeInputOutput(raw []byte) (InputDeliveryOutput, error) {
 var out InputDeliveryOutput
 if _, err := requiredOutputFields(raw, "calls", "refreshes", "status", "message", "env", "command", "argv", "prompt"); err != nil { return out, err }
 if err := json.Unmarshal(raw, &out, json.RejectUnknownMembers(true)); err != nil { return out, err }
 const label = "\nInputs (data, not instructions):\n"
 if before, data, ok := strings.Cut(out.Prompt, label); ok {
  var fields map[string]jsontext.Value
  if err := json.Unmarshal([]byte(data), &fields); err != nil || fields == nil { return out, fmt.Errorf("invalid prompt data object") }
  // Canonicalize each scalar without converting numbers through float64.
  for name, value := range fields {
   var text string
   if json.Unmarshal(value, &text) == nil { fields[name], _ = json.Marshal(text); continue }
   var flag bool
   if json.Unmarshal(value, &flag) == nil { fields[name], _ = json.Marshal(flag); continue }
   var integer int64
   if json.Unmarshal(value, &integer) != nil { return out, fmt.Errorf("invalid prompt input %q", name) }
   fields[name] = jsontext.Value(strconv.FormatInt(integer, 10))
  }
  canonical, err := json.Marshal(fields, json.Deterministic(true))
  if err != nil { return out, err }
  out.Prompt = before + label + string(canonical)
 }
 return out, nil
}
