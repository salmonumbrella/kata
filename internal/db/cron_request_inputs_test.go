package db

import (
	"encoding/json/jsontext"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Contract: inputs are bounded scalars and retained identity preserves literals.
func TestCronRequestInputsBoundsAndIdentity(t *testing.T) {
	values := map[string]jsontext.Value{}
	for i := range 32 {
		values[fmt.Sprintf("v%d", i)] = jsontext.Value("1")
	}
	_, err := canonicalCronRequestInputs(values)
	require.NoError(t, err)
	values["extra"] = jsontext.Value("1")
	_, err = canonicalCronRequestInputs(values)
	require.Error(t, err)
	for _, invalid := range []string{"null", "[]", "{}"} {
		_, err = canonicalCronRequestInputs(map[string]jsontext.Value{"value": jsontext.Value(invalid)})
		require.Error(t, err)
	}
	_, err = canonicalCronRequestInputs(map[string]jsontext.Value{"value": jsontext.Value(`"` + strings.Repeat("x", 4084) + `"`)})
	require.NoError(t, err) // {"value":"..."} is exactly 4096 bytes.
	_, err = canonicalCronRequestInputs(map[string]jsontext.Value{"value": jsontext.Value(`"` + strings.Repeat("x", 4085) + `"`)})
	require.Error(t, err)
	id := "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	first := jsontext.Value(`{"version":1,"values":{"batch":-0}}`)
	require.NoError(t, validateCronRequestInputsChange(id, nil, first))
	require.NoError(t, validateCronRequestInputsChange(id, first, jsontext.Value(`{"values":{"batch":-0},"version":1}`)))
	require.ErrorIs(t, validateCronRequestInputsChange(id, first, jsontext.Value(`{"version":1,"values":{"batch":0}}`)), ErrCronRequestConflict)
	require.NoError(t, validateCronRequestInputsChange(id, first, jsontext.Value("null")))
}
