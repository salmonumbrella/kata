package cron

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"os"
	"testing"
)

func TestS7RequestFixtureAgainstProducer(t *testing.T) {
	raw, err := os.ReadFile("s7_request_fixture.json")
	if err != nil { t.Fatal(err) }
	var bundle struct { Scenarios []struct {
		Name string `json:"name"`
		Steps []struct { Inputs struct {
			JobOptions jsontext.Value `json:"job_options"`
			Observations []struct { Resolution jsontext.Value `json:"resolution"` } `json:"observations"`
		} `json:"inputs"` } `json:"steps"`
	} `json:"scenarios"` }
	if err := json.Unmarshal(raw, &bundle); err != nil { t.Fatal(err) }
	for _, scenario := range bundle.Scenarios {
		if scenario.Name != "inputs/request-values-trigger" { continue }
		in := scenario.Steps[0].Inputs
		job := JobDefinition{Options: in.JobOptions, Action: Action{Prompt: "Inspect"}}
		got := ResolveInputs(job, nil, map[string]jsontext.Value{"text": jsontext.Value(`"requested"`)})
		var want InputResolution
		if err := json.Unmarshal(in.Observations[0].Resolution, &want); err != nil { t.Fatal(err) }
		gotJSON, err := json.Marshal(got, json.Deterministic(true)); if err != nil { t.Fatal(err) }
		wantJSON, err := json.Marshal(want, json.Deterministic(true)); if err != nil { t.Fatal(err) }
		if string(gotJSON) != string(wantJSON) { t.Fatalf("got %s want %s", gotJSON, wantJSON) }
		return
	}
	t.Fatal("request fixture missing")
}
