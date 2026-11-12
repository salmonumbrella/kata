package conformance

import "testing"

func TestInputSuiteVersionSelection(t *testing.T) {
	suite, err := NewSuite(t.Context(), "kata.cron.adapter-conformance/v3", Reference{}.Declare())
	if err != nil {
		t.Fatalf("v3 input suite unavailable: %v", err)
	}
	files, err := suite.ExportFiles(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := files["inputs.json"]; !ok {
		t.Fatal("v3 export is missing input delivery scenarios")
	}
}
