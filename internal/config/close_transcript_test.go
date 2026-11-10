package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

func TestReadCloseTranscriptConfig_InvalidAgentNamesKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_CLOSE_TRANSCRIPT_ENABLED", "")
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("[close.transcript.session_id_env]\nPi = \"PI_SESSION_ID\"\n"), 0600))
	_, err := ReadCloseTranscriptConfig()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Pi")
}

func TestReadCloseTranscriptConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_CLOSE_TRANSCRIPT_ENABLED", "")
	cfg, err := ReadCloseTranscriptConfig()
	require.NoError(t, err)
	assert.False(t, cfg.Enabled)
	// Client attachment must not resolve unrelated daemon secrets or policy.
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(`[auth]
token_file = "/missing/daemon-secret"
[close.throttle]
window = "invalid"
[close.transcript]
enabled = true
agentsview_url = "https://agentsview.example"
[close.transcript.session_id_env]
pi = "PI_SESSION_ID"
`), 0600))
	cfg, err = ReadCloseTranscriptConfig()
	require.NoError(t, err)
	assert.True(t, cfg.Enabled)
	assert.Equal(t, "https://agentsview.example", cfg.AgentsViewURL)
	assert.Equal(t, map[string]string{"pi": "PI_SESSION_ID"}, cfg.SessionIDEnv)
}

func TestReadCloseTranscriptConfig_ParseErrorNamesFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_CLOSE_TRANSCRIPT_ENABLED", "")
	path := filepath.Join(home, "config.toml")
	require.NoError(t, os.WriteFile(path, []byte("[close.transcript\n"), 0600))
	_, err := ReadCloseTranscriptConfig()
	require.Error(t, err)
	assert.Contains(t, err.Error(), path)
	_, ok := errors.AsType[toml.ParseError](err)
	assert.True(t, ok, "the parse position must reach the warning")
}

func TestReadCloseTranscriptConfig_EnabledEnvironment(t *testing.T) {
	t.Run("true keeps the file's link and session settings", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("KATA_HOME", home)
		t.Setenv("KATA_CLOSE_TRANSCRIPT_ENABLED", " true ")
		require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(`[close.transcript]
enabled = false
agentsview_url = "https://agentsview.example"
[close.transcript.session_id_env]
pi = "PI_SESSION_ID"
`), 0600))
		cfg, err := ReadCloseTranscriptConfig()
		require.NoError(t, err)
		assert.Equal(t, CloseTranscriptConfig{Enabled: true, AgentsViewURL: "https://agentsview.example", SessionIDEnv: map[string]string{"pi": "PI_SESSION_ID"}}, cfg)
	})
	t.Run("false ignores the file's invalid session settings", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("KATA_HOME", home)
		t.Setenv("KATA_CLOSE_TRANSCRIPT_ENABLED", "false")
		require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("[close.transcript]\nenabled = true\n[close.transcript.session_id_env]\nPi = \"PI_SESSION_ID\"\n"), 0600))
		cfg, err := ReadCloseTranscriptConfig()
		require.NoError(t, err)
		assert.False(t, cfg.Enabled)
	})
	t.Run("an invalid value is an error naming the variable", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("KATA_HOME", home)
		t.Setenv("KATA_CLOSE_TRANSCRIPT_ENABLED", "sometimes")
		require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("[close.transcript]\nenabled = true\n"), 0600))
		_, err := ReadCloseTranscriptConfig()
		require.ErrorContains(t, err, "KATA_CLOSE_TRANSCRIPT_ENABLED")
	})
}

// The client preference shares the daemon's strict config file, so the
// daemon must still start when a user enables it.
func TestReadDaemonConfigAcceptsCloseTranscript(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(`[close.transcript]
enabled = true
agentsview_url = "https://agentsview.example"
[close.transcript.session_id_env]
pi = "PI_SESSION_ID"
`), 0600))
	_, err := ReadDaemonConfig()
	require.NoError(t, err)
}

func TestReadCloseTranscriptProjectTargets(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(`[close.transcript]
enabled = true
agentsview_url = "https://default.example"
[close.transcript.project."team-project"]
agentsview_url = "https://team.example/archive"
push = true
token_env = "MISSING_TRANSCRIPT_TOKEN"
[close.transcript.project."disabled-project"]
enabled = false
`), 0600))
	cfg, err := ReadCloseTranscriptConfig()
	require.NoError(t, err)
	target := cfg.Target("team-project")
	require.NotNil(t, target.Enabled)
	assert.True(t, *target.Enabled)
	assert.Equal(t, "https://team.example/archive", target.AgentsViewURL)
	assert.True(t, target.Push)
	assert.Equal(t, "MISSING_TRANSCRIPT_TOKEN", target.TokenEnv)
	assert.False(t, *cfg.Target("disabled-project").Enabled)
	assert.Empty(t, cfg.Target("disabled-project").AgentsViewURL)
	assert.Equal(t, "https://default.example", cfg.Target("other-project").AgentsViewURL)
	_, err = ReadDaemonConfig()
	require.NoError(t, err, "client preferences must not resolve AgentsView secrets at daemon startup")
}

func TestCloseTranscriptTargetURLIsolation(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		name := rapid.String().Draw(rt, "project name")
		globalURL := rapid.String().Draw(rt, "global URL")
		targetURL := rapid.String().Draw(rt, "project URL")
		push := rapid.Bool().Draw(rt, "push enabled")
		cfg := CloseTranscriptConfig{AgentsViewURL: globalURL, Project: map[string]CloseTranscriptTarget{
			name: {AgentsViewURL: targetURL, Push: push, TokenEnv: "SELECTED_TOKEN"},
		}}
		got := cfg.Target(name)
		if got.AgentsViewURL != targetURL || got.Push != push || got.TokenEnv != "SELECTED_TOKEN" {
			rt.Fatalf("selected target changed: %#v", got)
		}
		other := cfg.Target(name + "\x00")
		if other.AgentsViewURL != globalURL || other.Push || other.TokenEnv != "" {
			rt.Fatalf("unmatched project inherited another project's upload authority: %#v", other)
		}
	})
}

func TestCloseTranscriptTargetEnabledInheritance(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		global := rapid.Bool().Draw(rt, "global enabled")
		override := rapid.Bool().Draw(rt, "project override")
		present := rapid.Bool().Draw(rt, "override present")
		name := rapid.String().Draw(rt, "project name")
		target := CloseTranscriptTarget{}
		want := global
		if present {
			target.Enabled = &override
			want = override
		}
		cfg := CloseTranscriptConfig{Enabled: global, Project: map[string]CloseTranscriptTarget{name: target}}
		got := cfg.Target(name)
		if got.Enabled == nil || *got.Enabled != want || cfg.Project[name].Enabled != target.Enabled {
			rt.Fatalf("enabled inheritance mutated configuration or changed the selected value")
		}
	})
}
