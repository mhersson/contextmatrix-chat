package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaultsYAML(t *testing.T) {
	clearServiceEnv(t)

	out, err := DefaultsYAML()
	require.NoError(t, err)

	text := string(out)

	for _, key := range []string{
		"contextmatrix_url:", "container_contextmatrix_url:", "api_key:", "port: 9093",
		"base_image:", "image_pull_policy: if-not-present", "secrets_dir:", "chat_run_dir:",
		"compaction:", "threshold: 0.85", "keep_recent_turns: 6",
	} {
		assert.Contains(t, text, key)
	}

	assert.Contains(t, text, "worker_extra_env: {}")
	assert.Contains(t, text, "image_list_filters: []")
	assert.NotContains(t, text, "null")

	path := filepath.Join(t.TempDir(), "serve.yaml")
	require.NoError(t, os.WriteFile(path, out, 0o600))

	cfg, err := LoadService(path)
	require.NoError(t, err)
	assert.Equal(t, 9093, cfg.Port)

	err = cfg.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "contextmatrix_url")
}
