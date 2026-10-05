package cron

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEnabledDefinitionHasNoCentralExecutor(t *testing.T) {
	_, err := ParseJob([]byte(`{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"interval","interval_seconds":60},"action":{"kind":"execute","prompt":"Review"},"issue":{"kind":"per-run","title":"Review"},"overlap":"allow","catchup":"skip"}`))
	require.NoError(t, err, "enabled is shared configuration; activation and executor selection are local")
}

func TestRetiredExecutorDefinitionIsRejected(t *testing.T) {
	_, err := ParseJob([]byte(`{"version":1,"kind":"job","enabled":false,"trigger":{"kind":"manual"},"action":{"kind":"execute","prompt":"Review"},"executor":{"uid":"01ARZ3NDEKTSV4RRFFQ69G5FAV","kind":"example"},"issue":{"kind":"per-run","title":"Review"},"overlap":"allow","catchup":"skip"}`))
	require.Error(t, err, "old feature-only executor authority is not silently interpreted as a label")
}
