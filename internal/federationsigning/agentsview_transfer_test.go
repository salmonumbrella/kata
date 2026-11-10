package federationsigning

import (
	"bytes"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// A signed upload must reject a control header changed after signing: it
// identifies the consented close and destination as well as the snapshot.
func TestAgentsViewTransferRejectsChangedControl(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(map[bool]string{false: "original", true: "changed"}[changed], func(t *testing.T) {
			source := testSource(t)
			verifier := testVerifier(t, "https://hub.example", source)
			r, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://hub.example/api/v1/projects/1/federation/transcript-service/issues/abc4", bytes.NewReader([]byte("snapshot")))
			require.NoError(t, err)
			r.Header.Set("Authorization", "Bearer enrollment")
			r.Header.Set("Content-Type", "application/octet-stream")
			r.Header.Set("X-Kata-Transcript-Control", "original-control")
			require.NoError(t, Sign(r, source))
			signedBody := r.Body
			t.Cleanup(func() { _ = signedBody.Close(); _ = r.Body.Close() })
			if changed {
				r.Header.Set("X-Kata-Transcript-Control", "changed-control")
				require.Error(t, verifier.Verify(r, 7))
				return
			}
			require.NoError(t, verifier.Verify(r, 7))
			body, err := io.ReadAll(r.Body)
			require.NoError(t, err)
			require.Equal(t, "snapshot", string(body))
		})
	}
}
