package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Contract: readable declarations choose the scalar type; fallback preserves
// JSON scalars and explicit quotes force a string.
func TestParseCronRequestInputs(t *testing.T) {
	values, err := parseCronRequestInputs([]string{"count=9007199254740993", "flag=false", "text=true", `quoted="false"`}, map[string]string{"count": "int", "flag": "bool", "text": "string", "quoted": "string"})
	require.NoError(t, err)
	require.Equal(t, "9007199254740993", string(values["count"]))
	require.Equal(t, "false", string(values["flag"]))
	require.Equal(t, `"true"`, string(values["text"]))
	require.Equal(t, `"false"`, string(values["quoted"]))
	values, err = parseCronRequestInputs([]string{"n=123", "s=staging", `q="true"`}, nil)
	require.NoError(t, err)
	require.Equal(t, "123", string(values["n"]))
	require.Equal(t, `"staging"`, string(values["s"]))
	require.Equal(t, `"true"`, string(values["q"]))
	_, err = parseCronRequestInputs([]string{"other=value"}, map[string]string{})
	require.ErrorContains(t, err, "undeclared input")
	_, err = parseCronRequestInputs([]string{"count=bad"}, map[string]string{"count": "int"})
	require.ErrorContains(t, err, "invalid integer")
}
