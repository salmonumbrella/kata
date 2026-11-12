package main

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.kenn.io/kata/pkg/cron/conformance"
)

// The package bridge tests delivery; this checks the offline wire format.
func TestCronConformanceInputSuiteOffline(t *testing.T) {
	d := conformance.Reference{}.Declare()
	suite, err := conformance.NewSuite(t.Context(), conformance.SchemaV3, d)
	if err != nil {
		t.Fatal(err)
	}
	header, err := json.Marshal(conformanceHeader{Schema: conformance.SchemaV3, Declaration: d})
	if err != nil {
		t.Fatal(err)
	}
	raw := append(header, '\n')
	for _, scenario := range suite.Scenarios {
		answer := conformance.Answer{Name: scenario.Name}
		for _, step := range scenario.Steps {
			answer.Outputs = append(answer.Outputs, step.Expect)
		}
		line, err := json.Marshal(answer)
		if err != nil {
			t.Fatal(err)
		}
		raw = append(append(raw, line...), '\n')
	}
	report, err := runConformance(t.Context(), t, raw, "check", "--answers", "-")
	if err != nil || !strings.Contains(string(report), `"failed":[]`) {
		t.Fatalf("%s %v", report, err)
	}
	if _, err := runConformance(t.Context(), t, append(header, '\n'), "check", "--answers", "-"); err == nil {
		t.Fatal("accepted missing answers")
	}
	if _, err := runConformance(t.Context(), t, append(append(header, '\n'), []byte(`{"name":"bad"}`)...), "check", "--answers", "-"); err == nil {
		t.Fatal("accepted malformed answer")
	}
	dir := t.TempDir()
	if _, err := runConformance(t.Context(), t, nil, "export", "--dir", dir, "--schema", conformance.SchemaV3); err != nil {
		t.Fatal(err)
	}
	manifest, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var exported struct {
		Files []string `json:"files"`
	}
	if err := json.Unmarshal(manifest, &exported); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, name := range exported.Files {
		if name == "inputs.json" {
			found = true
		}
	}
	if !found {
		t.Fatal("manifest missing inputs.json")
	}
}
